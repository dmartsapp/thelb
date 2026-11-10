// Package director runs a node as a director: it finds the cluster on the
// control VIP or - started with --bootstrap, when nothing answers - creates
// it; joins with the directors' secret and waits for approval; and is then a
// Raft member. The leader holds the control VIP, answers joins on it, signs
// certificates and writes every change to the cluster's state; the others
// follow, check in with it, and take over when it fails.
package director

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"path/filepath"
	"sync"
	"time"

	"github.com/dmartsapp/thelb/internal/alive"
	"github.com/dmartsapp/thelb/internal/api"
	"github.com/dmartsapp/thelb/internal/cluster"
	"github.com/dmartsapp/thelb/internal/fingerprint"
	"github.com/dmartsapp/thelb/internal/hostnet"
	"github.com/dmartsapp/thelb/internal/localsock"
	"github.com/dmartsapp/thelb/internal/logx"
	"github.com/dmartsapp/thelb/internal/sdnotify"
	"github.com/dmartsapp/thelb/internal/secret"
	"github.com/dmartsapp/thelb/internal/status"
	"github.com/dmartsapp/thelb/internal/version"
	"github.com/dmartsapp/thelb/internal/wire"
)

// Config is how a director was started.
type Config struct {
	ControlVIP        netip.Addr
	ControlVIPName    string // the name --vip gave, resolved at start; "" for an address
	ControlPort       uint16
	SecretFile        string
	BackendSecretFile string
	Bootstrap         bool
	Cluster           string
	Name              string
	Interface         string
}

// Network is how a director reaches the control VIP and the other directors.
type Network interface {
	// ListenNode listens on this node's address and the control port: Raft and
	// the API between directors.
	ListenNode() (net.Listener, error)
	// DialVIP connects to the control VIP and port, from this node's address.
	DialVIP(ctx context.Context) (net.Conn, error)
	// Dial connects to another director's address and port.
	Dial(ctx context.Context, addr string) (net.Conn, error)
	// VIPHolder asks the link who has the control VIP: a MAC address, or "".
	VIPHolder(ctx context.Context) (string, error)
	// Take adds the control VIP to this node, announces it, and listens on it.
	Take() (net.Listener, error)
	// Release removes the control VIP and closes its listener.
	Release()
	// Announce announces the control VIP again while this director holds it.
	Announce()
	// SetLease sets how long the kernel keeps the control VIP after this
	// director's last renewal: a theLB killed leaves nothing answering for long.
	SetLease(time.Duration)
	// Repair puts the control VIP back, while this director holds it, if
	// someone removed it: true if it did.
	Repair() (bool, error)
	// Cleanup removes a control VIP an earlier run left on this node, and fails
	// if one is here that theLB did not add.
	Cleanup() error
	// MACOf is the MAC address the neighbour table holds for ip, or "".
	MACOf(ip netip.Addr) string
	// Holder asks the link who has ip: a MAC address, or "".
	Holder(ctx context.Context, ip netip.Addr) (string, error)
	// Uplink is whether this node still reaches its network - a gateway on its
	// interface answers, or, with none, its link is up - and, if not, why.
	Uplink(ctx context.Context) (bool, string)
}

// Deps are what the director reads the world through; tests replace them.
type Deps struct {
	RunDir  string
	Log     *logx.Logger
	Now     func() time.Time
	Host    func(mac string) (fingerprint.Inputs, error)
	Network func(local hostnet.Address) (Network, error)
	Local   func(vip netip.Addr, iface string) (hostnet.Address, error) // nil: hostnet.For
	PeerUID func(*net.UnixConn) (int, error)                            // nil: the kernel's answer
	Ready   func()                                                      // nil: nothing; called once, when the director is a member
	// Dataplane is the kernel state a leader programs - service VIPs, IPVS,
	// pinned backends; nil: a director without one.
	Dataplane func() (Dataplane, error)
	// ProbeWindow is how long a bootstrapping director must find nothing on the
	// control VIP before it creates a cluster; 0: the default.
	ProbeWindow time.Duration
}

// Timing. Variables, so that tests can shorten them.
var (
	// probeWindow is longer than a failover takes - a leader's election and its
	// announcement - so a bootstrap never mistakes a failover for no cluster.
	probeWindow = 5 * time.Second
	// joinRetry is how long a director waits before asking again.
	joinRetry = 2 * time.Second
	// checkInEvery is how often a director checks in with the leader; one that
	// has not for liveFor is down.
	checkInEvery = time.Second
	liveFor      = 3 * time.Second
	// backendLease is how long a backend's heartbeat counts: it beats every 2 s.
	backendLease = 10 * time.Second
	// pendingFor is how long a pending node that stopped asking stays listed.
	pendingFor = time.Hour
	// sweepEvery is how often the leader tidies the membership.
	sweepEvery = 10 * time.Second
	// orphanAfter is how long a director goes without a leader before it looks
	// on the control VIP for another cluster.
	orphanAfter = 10 * time.Second
)

// probe is what answered on the control VIP and port.
type probe int

const (
	probeNothing probe = iota
	probeTheLB
	probeOther
)

func (p probe) String() string {
	return [...]string{"nothing", "a theLB cluster", "something that is not theLB"}[p]
}

// action is what the secret-file rules say to do.
type action int

const (
	actBootstrap    action = iota // create the cluster with the existing secret
	actCreate                     // create the secrets, then the cluster
	actJoin                       // join the cluster that answered
	actWait                       // wait for a cluster to answer
	actStopNoSecret               // no secret, and no right to create one
	actRefuseTaken                // something else holds the VIP or port
)

// decide is the design's table: "The secret files".
func decide(fileExists, bootstrap bool, p probe) action {
	switch p {
	case probeOther:
		return actRefuseTaken
	case probeTheLB:
		if fileExists {
			return actJoin
		}
		return actStopNoSecret
	}
	if bootstrap {
		if fileExists {
			return actBootstrap
		}
		return actCreate
	}
	if fileExists {
		return actWait
	}
	return actStopNoSecret
}

// Why a director stops, or starts over.
var (
	errRejoin  = errors.New("starting over")
	errRemoved = errors.New("this director was removed from the cluster")
)

// node is the running director: what lasts from one session - one membership
// of a cluster - to the next.
type node struct {
	cfg   Config
	d     Deps
	log   *logx.Logger
	net   Network
	plane Dataplane
	addr  hostnet.Address
	claim cluster.Claim

	dirSecret secret.Secret
	bkSecret  secret.Secret

	ready sync.Once

	// alive tells systemd's watchdog this director is alive while its loops
	// go round.
	alive *alive.Watch

	mu      sync.Mutex
	phase   string // what the director is doing, while it is not a member
	joining *wire.Cluster
	joinID  string
	sess    *session
}

// Run runs the director until ctx is done.
func Run(ctx context.Context, cfg Config, d Deps) error {
	log := d.Log.Module("director")
	local := d.Local
	if local == nil {
		local = hostnet.For
	}
	addr, err := local(cfg.ControlVIP, cfg.Interface)
	if err != nil {
		return fmt.Errorf("the control VIP %s: %w", cfg.ControlVIP, err)
	}

	// One theLB per node: the socket first, before anything else is touched.
	n := &node{cfg: cfg, d: d, log: log, addr: addr, phase: "starting", alive: alive.New(d.Log.Module("watchdog"))}
	srv, err := localsock.Listen(d.RunDir, n, d.Log.Module("socket"))
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
	n.claim = cluster.Claim{
		Name: cfg.Name, Role: "director", Address: addr.Prefix.Addr().String(), MAC: addr.MAC,
		Fingerprint: fingerprint.Compute(in), Hostname: in.Hostname, Version: version.Version,
	}
	if n.net, err = d.Network(addr); err != nil {
		return err
	}
	n.plane = nothing{}
	if d.Dataplane != nil {
		if n.plane, err = d.Dataplane(); err != nil {
			return err
		}
	}
	if err := n.net.Cleanup(); err != nil {
		return err
	}
	if cfg.ControlVIPName != "" {
		log.OK("control VIP resolved", "name", cfg.ControlVIPName, "vip", cfg.ControlVIP)
	}
	log.OK("node fingerprint", "node", cfg.Name, "fingerprint", n.claim.Fingerprint, "hostname", in.Hostname, "interface", addr.Interface, "address", addr.Prefix.Addr())

	sctx, stopServing := context.WithCancel(ctx)
	served := make(chan struct{})
	go func() {
		srv.Serve(sctx)
		close(served)
	}()
	// Started, for systemd: joining may wait for an approval for as long as it
	// takes, so readiness cannot wait for membership.
	_ = sdnotify.Notify("READY=1\nSTATUS=looking for the cluster on the control VIP")
	// systemd's watchdog is told only while the loops go round: the data plane
	// while this director leads, its check-ins with the leader otherwise.
	go n.alive.Feed(sctx, sdnotify.WatchdogInterval(), sdnotify.Notify)

	err = n.loop(ctx)
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

// local is this director's address in the network of addr - on the
// interface named, if one is.
func (n *node) local(addr netip.Addr, iface string) (hostnet.Address, error) {
	if n.d.Local != nil {
		return n.d.Local(addr, iface)
	}
	return hostnet.For(addr, iface)
}

func (n *node) setPhase(p string) {
	n.mu.Lock()
	n.phase = p
	n.mu.Unlock()
	_ = sdnotify.Notify("STATUS=" + p)
}

// loop finds or creates the cluster and is a member of it, until ctx is done -
// starting over when the session ends in a way that allows it.
func (n *node) loop(ctx context.Context) error {
	var standing *takeover
	for {
		var s *session
		if standing != nil {
			// The other of two directors is gone: a session of this director
			// alone, from the state it had - no probe, no join.
			t := standing
			standing = nil
			var err error
			if s, err = n.standAlone(t); err != nil {
				return err
			}
		} else {
			exists, err := secret.Exists(n.cfg.SecretFile)
			if err != nil {
				return err
			}
			if !exists && !n.cfg.Bootstrap {
				return n.noSecret()
			}
			n.setPhase("looking for the cluster on the control VIP")
			p, takenBy, err := n.probe(ctx)
			if err != nil {
				return err
			}
			if ctx.Err() != nil {
				return nil
			}
			switch decide(exists, n.cfg.Bootstrap, p) {
			case actRefuseTaken:
				if takenBy != tcpServer {
					// It answers ARP, but no connection to the port: often a firewall
					// on the holder - theLB's own leader among them - dropping it.
					return fmt.Errorf("%s holds %s, but nothing accepts a connection on port %d: open the control port to the directors in that machine's firewall if it runs theLB, or choose another VIP", takenBy, n.cfg.ControlVIP, n.cfg.ControlPort)
				}
				return fmt.Errorf("something that is not theLB answers on %s port %d: the control VIP or port is taken", n.cfg.ControlVIP, n.cfg.ControlPort)
			case actStopNoSecret:
				return n.noSecret()
			case actCreate, actBootstrap:
				if err := n.settleSecrets(); err != nil {
					return err
				}
				if err := n.loadSecrets(); err != nil {
					return err
				}
				if s, err = n.create(); err != nil {
					return err
				}
			case actJoin:
				if err := n.loadSecrets(); err != nil {
					return err
				}
				if s, err = n.join(ctx); err != nil {
					if errors.Is(err, errRejoin) {
						n.log.Warn("starting over: looking for the cluster on the control VIP", "reason", err)
						continue
					}
					return err
				}
				if s == nil { // ctx done
					return nil
				}
			default: // actWait: probe waits for a cluster, so this is not reached
				continue
			}
		}

		n.mu.Lock()
		n.sess, n.joining = s, nil
		n.mu.Unlock()
		err := s.run(ctx)
		n.mu.Lock()
		n.sess = nil
		n.mu.Unlock()
		s.close(ctx.Err() != nil)
		var t *takeover
		switch {
		case ctx.Err() != nil:
			return nil
		case errors.As(err, &t):
			standing = t
			continue
		case errors.Is(err, errRejoin):
			n.log.Warn("starting over: looking for the cluster on the control VIP", "reason", err)
			continue
		default:
			return err
		}
	}
}

func (n *node) noSecret() error {
	return fmt.Errorf("there is no secret in %s, and this director may not create a cluster (no --bootstrap): on a director, run `sudo thelb admin secret show --role director` and put what it prints in %s", n.cfg.SecretFile, n.cfg.SecretFile)
}

// settleSecrets creates, at a bootstrap, either secret file that is missing.
func (n *node) settleSecrets() error {
	for _, f := range []struct{ path, role string }{{n.cfg.SecretFile, "director"}, {n.cfg.BackendSecretFile, "backend"}} {
		ok, err := secret.Exists(f.path)
		if err != nil {
			return err
		}
		if ok {
			continue
		}
		s, err := secret.Generate()
		if err != nil {
			return err
		}
		if err := secret.CreateExclusive(f.path, s); err != nil {
			return err
		}
		n.log.OK("secret file created", "role", f.role, "file", f.path, "mode", "0600")
	}
	return nil
}

func (n *node) loadSecrets() error {
	d, err := secret.Load(n.cfg.SecretFile)
	if err != nil {
		return err
	}
	b, err := secret.Load(n.cfg.BackendSecretFile)
	if err != nil {
		return err
	}
	n.mu.Lock()
	n.dirSecret, n.bkSecret = d, b
	n.mu.Unlock()
	return nil
}

// probe looks at the control VIP and port: at once for a cluster, and - before
// a bootstrap - for longer than a failover takes, so that it finds nothing only
// when there is nothing. A look that fails - the link is down, say - counts for
// nothing: the window starts again. Without --bootstrap it waits for a cluster.
func (n *node) probe(ctx context.Context) (probe, string, error) {
	window := n.d.ProbeWindow
	if window == 0 {
		window = probeWindow
	}
	var otherSince, nothingSince time.Time
	nextWarn := time.Now().Add(window)
	warned := map[string]time.Time{}
	for {
		p, who, err := n.probeOnce(ctx)
		if ctx.Err() != nil {
			return probeNothing, "", nil
		}
		switch {
		case err != nil:
			otherSince, nothingSince = time.Time{}, time.Time{}
			if msg := err.Error(); time.Since(warned[msg]) > time.Minute {
				warned[msg] = time.Now()
				n.log.Warn("looking at the control VIP failed; trying again", "vip", n.cfg.ControlVIP, "error", err)
			}
		case p == probeTheLB:
			return p, "", nil
		case p == probeOther:
			nothingSince = time.Time{}
			if otherSince.IsZero() {
				otherSince = time.Now()
			}
			if time.Since(otherSince) >= window {
				n.log.Error("the control VIP is taken", "vip", n.cfg.ControlVIP, "port", n.cfg.ControlPort, "by", who)
				return probeOther, who, nil
			}
		default:
			otherSince = time.Time{}
			if nothingSince.IsZero() {
				nothingSince = time.Now()
			}
			if n.cfg.Bootstrap && time.Since(nothingSince) >= window {
				return probeNothing, "", nil
			}
			if !n.cfg.Bootstrap && time.Now().After(nextWarn) {
				n.log.Warn("no cluster answers on the control VIP yet; waiting", "vip", n.cfg.ControlVIP, "port", n.cfg.ControlPort)
				nextWarn = time.Now().Add(time.Minute)
			}
		}
		select {
		case <-ctx.Done():
			return probeNothing, "", nil
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// probeOnce is one look: a theLB cluster answers a join's TLS handshake; a
// connection that does anything else, or a host that answers ARP or neighbour
// discovery for the VIP but not on the port, is something else.
// tcpServer is probeOnce's who for a control port that accepts connections but
// does not complete theLB's handshake.
const tcpServer = "a TCP server"

func (n *node) probeOnce(ctx context.Context) (probe, string, error) {
	dctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if c, err := n.net.DialVIP(dctx); err == nil {
		tc := tls.Client(c, wire.JoinConfig(n.cfg.ControlVIP, nil))
		_ = tc.SetDeadline(time.Now().Add(2 * time.Second))
		err := tc.Handshake()
		_ = tc.Close()
		if err == nil {
			return probeTheLB, "", nil
		}
		return probeOther, tcpServer, nil
	}
	if ctx.Err() != nil {
		return probeNothing, "", nil
	}
	hctx, cancel2 := context.WithTimeout(ctx, time.Second)
	defer cancel2()
	who, err := n.net.VIPHolder(hctx)
	if err != nil {
		return probeNothing, "", fmt.Errorf("probing the control VIP on the network: %w", err)
	}
	if who != "" {
		return probeOther, who, nil
	}
	return probeNothing, "", nil
}

// member is called when the director has become a member of a cluster.
func (n *node) member(s *session) {
	n.log.OK("director ready", "socket", filepath.Join(n.d.RunDir, localsock.SocketName), "control_vip", n.cfg.ControlVIP, "port", n.cfg.ControlPort, "interface", n.addr.Interface, "member", s.raftID)
	_ = sdnotify.Notify("STATUS=a member of the cluster")
	n.ready.Do(func() {
		if n.d.Ready != nil {
			n.d.Ready()
		}
	})
}

func randomHex(nbytes int) (string, error) {
	b := make([]byte, nbytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("random bytes: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// Status is the director's view, for `thelb status`.
func (n *node) Status() status.Status {
	n.mu.Lock()
	s, phase, joining, joinID := n.sess, n.phase, n.joining, n.joinID
	n.mu.Unlock()
	if s != nil {
		return s.status()
	}
	st := status.Status{View: "director"}
	if joining != nil {
		st.Cluster = &status.Cluster{Name: joining.Name, ID: joinID}
	}
	st.Nodes = []status.Node{{Node: n.cfg.Name, Role: "director", Address: n.claim.Address, State: phase, Version: version.Version, NodeFingerprint: n.claim.Fingerprint}}
	return st
}

// SecretText is a role's secret, for `thelb admin secret show`.
func (n *node) SecretText(role string) (string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	var s secret.Secret
	switch role {
	case "director":
		s = n.dirSecret
	case "backend":
		s = n.bkSecret
	default:
		return "", errors.New("a secret's role is director or backend")
	}
	if s.IsZero() {
		return "", errors.New("the director is still starting")
	}
	return s.Encode(), nil
}

// Offer refuses: a backend offers its applications, and a director's host
// offers through a backend of its own.
func (n *node) Offer(api.Offer) (api.Result, error) {
	return api.Result{}, errors.New("thelb offer runs where thelb backend runs: on this host, start one beside the director, under its own --name")
}

// Admin changes the cluster, for `thelb admin`: on the leader directly, from
// another director through the leader.
func (n *node) Admin(c api.Change) (api.Result, error) {
	n.mu.Lock()
	s := n.sess
	n.mu.Unlock()
	if s == nil {
		return api.Result{}, errors.New("this director is not a member of a cluster yet")
	}
	return s.admin(c)
}
