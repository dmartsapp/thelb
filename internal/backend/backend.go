// Package backend runs a node as a backend: it enrols with the backends' secret
// on the control VIP and waits for a director's approval; then it sends its
// heartbeat every 2 seconds - what it offers, how its own checks of its
// applications go, which VIPs it holds - and holds the VIPs of the services
// its offers are assigned to, on thelb0, without announcing them. It never
// chooses a service: a director assigns its offers.
package backend

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dmartsapp/thelb/internal/api"
	"github.com/dmartsapp/thelb/internal/ca"
	"github.com/dmartsapp/thelb/internal/check"
	"github.com/dmartsapp/thelb/internal/cluster"
	"github.com/dmartsapp/thelb/internal/fingerprint"
	"github.com/dmartsapp/thelb/internal/hostnet"
	"github.com/dmartsapp/thelb/internal/join"
	"github.com/dmartsapp/thelb/internal/localsock"
	"github.com/dmartsapp/thelb/internal/logx"
	"github.com/dmartsapp/thelb/internal/nft"
	"github.com/dmartsapp/thelb/internal/sdnotify"
	"github.com/dmartsapp/thelb/internal/secret"
	"github.com/dmartsapp/thelb/internal/status"
	"github.com/dmartsapp/thelb/internal/version"
	"github.com/dmartsapp/thelb/internal/wire"
)

// Config is how a backend was started.
type Config struct {
	Join       netip.AddrPort // the control VIP and port
	JoinName   string         // the name --join gave, resolved at start; "" for an address
	SecretFile string
	Name       string
	Interface  string
	Offers     []cluster.Offer
}

// Kernel is the backend's kernel state.
type Kernel interface {
	// Prepare creates thelb0 and sets the ARP parameters.
	Prepare() error
	// Hold makes thelb0 hold exactly these VIPs.
	Hold(vips []netip.Addr) error
	// Map makes theLB's nftables table map exactly these ports and clamp
	// exactly these MSSs - and removes the table when there are none.
	Map(maps []nft.Map, clamps []nft.Clamp) error
	// Tunnel creates or removes theLB's IP-in-IP interface, thelbt0, which
	// unwraps what a director sends a tunnelled service.
	Tunnel(on bool) error
	// Restore removes thelb0 and puts the parameters back.
	Restore()
}

// Deps are what the backend reads the world through; tests replace them.
type Deps struct {
	RunDir  string
	Log     *logx.Logger
	Now     func() time.Time
	Host    func(mac string) (fingerprint.Inputs, error)
	Local   func(vip netip.Addr, iface string) (hostnet.Address, error) // nil: hostnet.For
	Dial    func(ctx context.Context, from netip.Addr, to string) (net.Conn, error)
	Kernel  func() (Kernel, error)
	PeerUID func(*net.UnixConn) (int, error)
	Ready   func() // nil: nothing; called once, when the backend is a member
}

// Timing; tests shorten it.
var (
	beatEvery = 2 * time.Second
	retry     = time.Second
	// strayAfter is how long a backend goes without the cluster before it
	// looks for another on the control VIP: the cluster created again.
	strayAfter = 15 * time.Second
)

var errRemoved = errors.New("this backend was removed from the cluster")

// agent is the running backend.
type agent struct {
	cfg    Config
	d      Deps
	log    *logx.Logger
	local  hostnet.Address
	claim  cluster.Claim
	secret secret.Secret
	kernel Kernel
	ready  sync.Once

	mu       sync.Mutex
	phase    string
	cluster  string
	assigned []api.Assignment
	checks   map[string]*check.State
}

// Run runs the backend until ctx is done.
func Run(ctx context.Context, cfg Config, d Deps) error {
	log := d.Log.Module("backend")
	local := d.Local
	if local == nil {
		local = hostnet.For
	}
	addr, err := local(cfg.Join.Addr(), cfg.Interface)
	routed := false
	if err != nil && cfg.Interface == "" && d.Local == nil {
		// The control VIP is in no network of this node: reached through a
		// router, which only a tunnelled service's backend may be.
		if r, rerr := hostnet.Route(cfg.Join.Addr()); rerr == nil {
			addr, err, routed = r, nil, true
		}
	}
	if err != nil {
		return fmt.Errorf("the control VIP %s: %w", cfg.Join.Addr(), err)
	}
	b := &agent{cfg: cfg, d: d, log: log, local: addr, phase: "starting", checks: map[string]*check.State{}}
	srv, err := localsock.Listen(d.RunDir, b, d.Log.Module("socket"))
	if err != nil {
		return err
	}
	defer srv.Close()
	if d.PeerUID != nil {
		srv.PeerUID = d.PeerUID
	}
	in, err := d.Host(addr.MAC)
	if err != nil {
		return err
	}
	b.claim = cluster.Claim{Name: cfg.Name, Role: "backend", Address: addr.Prefix.Addr().String(), MAC: addr.MAC,
		Fingerprint: fingerprint.Compute(in), Hostname: in.Hostname, Version: version.Version}
	exists, err := secret.Exists(cfg.SecretFile)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("there is no secret in %s: copy the backends' secret file from a director, or run `sudo thelb admin secret show --role backend` on one and put what it prints in %s", cfg.SecretFile, cfg.SecretFile)
	}
	if b.secret, err = secret.Load(cfg.SecretFile); err != nil {
		return err
	}
	if b.kernel, err = d.Kernel(); err != nil {
		return err
	}
	if err := b.kernel.Prepare(); err != nil {
		return err
	}
	defer b.kernel.Restore()
	if cfg.JoinName != "" {
		log.OK("control VIP resolved", "name", cfg.JoinName, "vip", cfg.Join.Addr())
	}
	if routed {
		log.OK("the control VIP is behind a router: this backend can serve only tunnelled services", "vip", cfg.Join.Addr(), "from", addr.Prefix.Addr(), "interface", addr.Interface)
	}
	log.OK("node fingerprint", "node", cfg.Name, "fingerprint", b.claim.Fingerprint, "hostname", in.Hostname, "interface", addr.Interface, "address", addr.Prefix.Addr())

	sctx, stopServing := context.WithCancel(ctx)
	served := make(chan struct{})
	go func() {
		srv.Serve(sctx)
		close(served)
	}()
	_ = sdnotify.Notify("READY=1\nSTATUS=enrolling on the control VIP")
	if every := sdnotify.WatchdogInterval(); every > 0 {
		go func() {
			t := time.NewTicker(every)
			defer t.Stop()
			for {
				select {
				case <-sctx.Done():
					return
				case <-t.C:
					_ = sdnotify.Notify("WATCHDOG=1")
				}
			}
		}()
	}
	for _, o := range cfg.Offers {
		go b.checkOffer(sctx, o)
	}

	err = b.loop(ctx)
	log.OK("stopping")
	_ = sdnotify.Notify("STOPPING=1")
	stopServing()
	<-served
	srv.Close()
	if err != nil {
		return err
	}
	log.OK("stopped")
	return nil
}

func (b *agent) setPhase(p string) {
	b.mu.Lock()
	b.phase = p
	b.mu.Unlock()
	_ = sdnotify.Notify("STATUS=" + p)
}

// checkOffer checks one of the backend's applications, at its own address -
// healthy after two successes, unhealthy after three failures.
func (b *agent) checkOffer(ctx context.Context, o cluster.Offer) {
	st := &check.State{}
	b.mu.Lock()
	b.checks[o.Key()] = st
	b.mu.Unlock()
	target := netip.AddrPortFrom(b.local.Prefix.Addr(), uint16(o.Port))
	t := time.NewTicker(check.Every)
	defer t.Stop()
	for {
		err := check.Probe(ctx, o.Check, target, check.Timeout)
		if ctx.Err() != nil {
			return
		}
		b.mu.Lock()
		changed := st.Observe(err)
		healthy := st.Healthy
		b.mu.Unlock()
		if changed {
			if healthy {
				b.log.OK("application healthy", "offer", o.Key(), "check", check.Describe(o.Check))
			} else {
				b.log.Warn("application unhealthy", "offer", o.Key(), "check", check.Describe(o.Check), "error", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (b *agent) dial(ctx context.Context) (net.Conn, error) {
	if b.d.Dial != nil {
		return b.d.Dial(ctx, b.local.Prefix.Addr(), b.cfg.Join.String())
	}
	d := &net.Dialer{Timeout: 2 * time.Second, LocalAddr: &net.TCPAddr{IP: b.local.Prefix.Addr().AsSlice()}}
	return d.DialContext(ctx, "tcp", b.cfg.Join.String())
}

// loop enrols and serves, until ctx is done - enrolling again when the cluster
// was created anew.
func (b *agent) loop(ctx context.Context) error {
	said := map[string]time.Time{}
	say := func(warn bool, msg string, kv ...any) {
		if time.Since(said[msg]) < time.Minute {
			return
		}
		said[msg] = time.Now()
		if warn {
			b.log.Warn(msg, kv...)
		} else {
			b.log.OK(msg, kv...)
		}
	}
	for ctx.Err() == nil {
		m, err := b.enrol(ctx, say)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			if errors.As(err, new(join.Stop)) {
				return err
			}
			say(true, "enrolling: trying again", "error", err)
			sleep(ctx, retry)
			continue
		}
		err = b.serve(ctx, m)
		if ctx.Err() != nil {
			b.leave(m)
			return nil
		}
		if errors.Is(err, errRemoved) {
			return err
		}
		b.log.Warn("enrolling again", "reason", err)
		_ = b.kernel.Hold(nil)
		b.mu.Lock()
		b.assigned = nil
		b.mu.Unlock()
	}
	return nil
}

// membership is an enrolled backend's identity.
type membership struct {
	member string
	key    *ecdsa.PrivateKey
	id     *wire.Identity
}

// enrol asks the cluster to let this backend in, and waits - for approval, if
// it needs one.
func (b *agent) enrol(ctx context.Context, say func(bool, string, ...any)) (*membership, error) {
	b.setPhase("enrolling")
	key, err := ca.NewKey()
	if err != nil {
		return nil, err
	}
	csr, err := ca.Request(key, b.cfg.Name)
	if err != nil {
		return nil, err
	}
	a := join.Asker{
		VIP: b.cfg.Join, Role: "backend", Secret: b.secret, SecretFile: b.cfg.SecretFile,
		Claim: b.claim, CSR: csr, Dial: b.dial,
		Proven: func(cl wire.Cluster, _ string) {
			b.mu.Lock()
			b.cluster = cl.Name
			b.mu.Unlock()
		},
		Waiting: func() {
			b.setPhase("waiting for approval")
			say(false, "waiting for approval", "cluster", b.clusterName(), "node", b.cfg.Name, "fingerprint", b.claim.Fingerprint, "address", b.claim.Address, "mac", b.claim.MAC)
		},
	}
	ap, err := a.Ask(ctx)
	if err != nil {
		return nil, err
	}
	defer ap.Close()
	if ap.Before {
		b.log.OK("approved before: rejoining", "cluster", ap.Cluster.Name, "approved_by", ap.By)
	} else {
		b.log.OK("approved", "cluster", ap.Cluster.Name, "by", ap.By)
	}
	id, err := wire.NewIdentity(ap.CA)
	if err != nil {
		return nil, err
	}
	if err := id.SetCertificate(key, ap.Cert); err != nil {
		return nil, err
	}
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		return nil, err
	}
	member := b.cfg.Name + "-" + hex.EncodeToString(suffix)
	if _, err := ap.Ready(member, ""); err != nil {
		return nil, err
	}
	b.log.OK("enrolled", "cluster", ap.Cluster.Name, "id", ap.ClusterID, "member", member)
	return &membership{member: member, key: key, id: id}, nil
}

func (b *agent) clusterName() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cluster
}

// serve keeps a connection to the leader, on the control VIP - reconnecting
// when the VIP moves - and sends the heartbeat on it. It returns when the
// backend is removed, or the cluster it enrolled with is gone.
func (b *agent) serve(ctx context.Context, m *membership) error {
	b.setPhase("connecting")
	lost := time.Time{}
	said := map[string]time.Time{}
	renewAt := renewTime(m.id.Leaf())
	for ctx.Err() == nil {
		if time.Now().After(renewAt) {
			if err := b.renew(ctx, m); err != nil {
				b.log.Error("renewing the certificate failed; retrying in a minute", "error", err)
				renewAt = time.Now().Add(time.Minute)
			} else {
				renewAt = renewTime(m.id.Leaf())
			}
		}
		err := b.session(ctx, m, renewAt)
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, errRemoved) {
			return err
		}
		if err == nil {
			lost = time.Time{} // it served until its certificate was due
			continue
		}
		if lost.IsZero() {
			lost = time.Now()
		}
		if msg := err.Error(); time.Since(said[msg]) > time.Minute {
			said[msg] = time.Now()
			b.log.Warn("the connection to the cluster ended; reconnecting", "error", err)
		}
		if time.Since(lost) > strayAfter {
			if b.anotherCluster(ctx, m) {
				return errors.New("another cluster answers on the control VIP: the cluster was created again")
			}
			if errors.As(err, new(handshakeError)) {
				// The cluster refuses this backend's certificate - revoked when a
				// director removed it - so it asks again, with its secret: a
				// removed backend then waits for approval, and says so.
				return fmt.Errorf("the cluster no longer accepts this backend's certificate: %w", err)
			}
		}
		sleep(ctx, retry)
	}
	return nil
}

// handshakeError is a TLS handshake with the leader that failed.
type handshakeError struct{ err error }

func (e handshakeError) Error() string { return "the TLS handshake with the leader: " + e.err.Error() }
func (e handshakeError) Unwrap() error { return e.err }

func renewTime(c *x509.Certificate) time.Time {
	return c.NotBefore.Add(c.NotAfter.Sub(c.NotBefore) * 2 / 3)
}

// session is one connection to the leader: heartbeats out, orders in, until it
// breaks or the certificate is due for renewal. It returns nil when it was
// connected and served, until the end.
func (b *agent) session(ctx context.Context, m *membership, until time.Time) error {
	c, err := b.dial(ctx)
	if err != nil {
		return err
	}
	tc := tls.Client(c, m.id.ClientConfig(wire.ALPNAPI, b.cfg.Join.Addr()))
	defer func() { _ = tc.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = tc.Close() })
	defer stop()
	_ = tc.SetDeadline(time.Now().Add(wire.HandshakeTimeout))
	if err := tc.Handshake(); err != nil {
		return handshakeError{err}
	}
	_ = tc.SetDeadline(time.Time{})
	w := wire.NewConn(tc)
	first := true
	for ctx.Err() == nil && time.Now().Before(until) {
		beat := b.beat(m)
		_ = tc.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if err := w.Send(api.Request{V: wire.Version, Op: api.OpBackend, Beat: &beat}); err != nil {
			return err
		}
		_ = tc.SetReadDeadline(time.Now().Add(5 * time.Second))
		var resp api.Response
		if err := w.Recv(&resp); err != nil {
			return err
		}
		if !resp.OK {
			return errors.New(resp.Error)
		}
		if resp.Orders == nil {
			return errors.New("the leader sent no orders")
		}
		if resp.Orders.Removed {
			return errRemoved
		}
		b.obey(resp.Orders.Assignments)
		if first {
			first = false
			b.setPhase("a member of the cluster")
			b.log.OK("backend ready", "cluster", b.clusterName(), "member", m.member, "offers", len(b.cfg.Offers))
			b.ready.Do(func() {
				if b.d.Ready != nil {
					b.d.Ready()
				}
			})
		}
		sleep(ctx, beatEvery)
	}
	return nil
}

func (b *agent) beat(m *membership) api.Beat {
	b.mu.Lock()
	defer b.mu.Unlock()
	beat := api.Beat{Member: m.member, Version: version.Version, Offers: b.cfg.Offers, Checks: map[string]bool{}}
	for k, st := range b.checks {
		beat.Checks[k] = st.Healthy
	}
	for _, a := range b.assigned {
		if !slices.Contains(beat.Holding, a.VIP) {
			beat.Holding = append(beat.Holding, a.VIP)
		}
	}
	sort.Strings(beat.Holding)
	return beat
}

// obey holds the VIPs of the services assigned to this backend.
func (b *agent) obey(as []api.Assignment) {
	b.mu.Lock()
	same := slices.Equal(b.assigned, as)
	b.mu.Unlock()
	if same {
		return
	}
	var vips []netip.Addr
	var maps []nft.Map
	var clamps []nft.Clamp
	tunnel := false
	for _, a := range as {
		v, err := netip.ParseAddr(a.VIP)
		if err != nil {
			continue
		}
		if !slices.Contains(vips, v) {
			vips = append(vips, v)
		}
		if a.AppPort != 0 && a.AppPort != a.Port {
			maps = append(maps, nft.Map{VIP: v, Proto: a.Proto, Port: uint16(a.Port), AppPort: uint16(a.AppPort)})
		}
		if a.Tunnel {
			tunnel = true
			if c := (nft.Clamp{VIP: v, MSS: b.mss()}); a.Proto == "tcp" && !slices.Contains(clamps, c) {
				clamps = append(clamps, c)
			}
		}
	}
	// The tunnel, then the mappings and clamps, then the VIPs: a packet the VIP
	// accepts is unwrapped and mapped.
	if err := b.kernel.Tunnel(tunnel); err != nil {
		b.log.Error("the IP-in-IP interface failed", "error", err)
		return
	}
	if err := b.kernel.Map(maps, clamps); err != nil {
		b.log.Error("mapping the services' ports failed", "error", err)
		return
	}
	if err := b.kernel.Hold(vips); err != nil {
		b.log.Error("holding the services' VIPs failed", "error", err)
		return
	}
	b.mu.Lock()
	old := b.assigned
	b.assigned = as
	b.mu.Unlock()
	for _, a := range as {
		if !slices.Contains(old, a) {
			b.log.OK("assigned", "service", a.Service, "offer", fmt.Sprintf("%s/%d", a.Proto, a.AppPort), "port", a.Port)
		}
	}
	for _, a := range old {
		if !slices.Contains(as, a) {
			b.log.OK("unassigned", "service", a.Service, "offer", fmt.Sprintf("%s/%d", a.Proto, a.AppPort))
		}
	}
}

// drainFor is how long a stopping backend keeps its VIPs after telling the
// leader: the leader programs weight 0 within a second.
var drainFor = 1500 * time.Millisecond

// leave tells the leader this backend is stopping - so it sends it no new
// connections - before the VIPs go.
func (b *agent) leave(m *membership) {
	b.mu.Lock()
	held := len(b.assigned) > 0
	b.mu.Unlock()
	if !held {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, err := b.dial(ctx)
	if err != nil {
		return
	}
	tc := tls.Client(c, m.id.ClientConfig(wire.ALPNAPI, b.cfg.Join.Addr()))
	defer func() { _ = tc.Close() }()
	_ = tc.SetDeadline(time.Now().Add(3 * time.Second))
	w := wire.NewConn(tc)
	beat := b.beat(m)
	beat.Leaving = true
	if w.Send(api.Request{V: wire.Version, Op: api.OpBackend, Beat: &beat}) != nil {
		return
	}
	var resp api.Response
	if w.Recv(&resp) != nil || !resp.OK {
		return
	}
	b.log.OK("the leader drains this backend", "for", drainFor)
	time.Sleep(drainFor)
}

// mss is the MSS a tunnelled service's TCP handshakes announce: the
// interface's MTU less IP-in-IP's outer header (20 bytes) and the inner IP and
// TCP headers (40), so the client's segments still fit once wrapped.
func (b *agent) mss() uint16 {
	mtu := 1500
	if ifc, err := net.InterfaceByName(b.local.Interface); err == nil && ifc.MTU > 0 {
		mtu = ifc.MTU
	}
	return uint16(mtu - 20 - 40)
}

// renew asks the leader to sign a new certificate for the backend's key.
func (b *agent) renew(ctx context.Context, m *membership) error {
	csr, err := ca.Request(m.key, b.cfg.Name)
	if err != nil {
		return err
	}
	c, err := b.dial(ctx)
	if err != nil {
		return err
	}
	tc := tls.Client(c, m.id.ClientConfig(wire.ALPNAPI, b.cfg.Join.Addr()))
	defer func() { _ = tc.Close() }()
	_ = tc.SetDeadline(time.Now().Add(10 * time.Second))
	w := wire.NewConn(tc)
	if err := w.Send(api.Request{V: wire.Version, Op: api.OpRenew, CSR: csr}); err != nil {
		return err
	}
	var resp api.Response
	if err := w.Recv(&resp); err != nil {
		return err
	}
	if !resp.OK {
		return errors.New(resp.Error)
	}
	if err := m.id.SetCertificate(m.key, resp.Cert); err != nil {
		return err
	}
	b.log.OK("certificate renewed", "node", b.cfg.Name, "not_after", m.id.Leaf().NotAfter)
	return nil
}

// anotherCluster is whether a cluster with another CA answers on the control
// VIP.
func (b *agent) anotherCluster(ctx context.Context, m *membership) bool {
	c, err := b.dial(ctx)
	if err != nil {
		return false
	}
	var cl wire.Cluster
	tc := tls.Client(c, wire.JoinConfig(b.cfg.Join.Addr(), &cl))
	_ = tc.SetDeadline(time.Now().Add(2 * time.Second))
	err = tc.Handshake()
	_ = tc.Close()
	return err == nil && cl.CA != nil && !cl.CA.Equal(m.id.CA)
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// Status is the backend's own view: what it is, the cluster's name, its offers
// with the service each is assigned to - not the service's VIP - and its node
// fingerprint.
func (b *agent) Status() status.Status {
	b.mu.Lock()
	defer b.mu.Unlock()
	mine := &status.Mine{Role: "backend", Cluster: b.cluster, State: b.phase, NodeFingerprint: b.claim.Fingerprint}
	for _, o := range b.cfg.Offers {
		row := status.MineOffer{Offer: o.Key(), Check: check.Describe(o.Check), Own: "-", Service: "-"}
		if st := b.checks[o.Key()]; st != nil && (st.Last != nil || st.Healthy) {
			row.Own = st.Word()
		}
		var to []string
		for _, a := range b.assigned {
			if a.Proto == o.Proto && a.AppPort == o.Port {
				name := a.Service
				if a.Port != a.AppPort {
					name += fmt.Sprintf(" (port %d)", a.Port)
				}
				to = append(to, name)
			}
		}
		if len(to) > 0 {
			sort.Strings(to)
			row.Service = strings.Join(to, ", ")
		}
		mine.Offers = append(mine.Offers, row)
	}
	return status.Status{View: "backend", Mine: mine}
}

// SecretText refuses: a secret is shown only on a director.
func (b *agent) SecretText(string) (string, error) {
	return "", errors.New("a secret is shown only on a director")
}

// Generate refuses: what a backend could print, a director prints.
func (b *agent) Generate(string) (string, error) {
	return "", errors.New("thelb generate commands runs on a director")
}

// Admin refuses: a backend has no admin.
func (b *agent) Admin(api.Change) (api.Result, error) {
	return api.Result{}, errors.New("a backend cannot change the cluster: run thelb admin on a director")
}
