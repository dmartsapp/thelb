// Package director runs a node as a director. This build goes as far as a
// cluster of one: it takes the node's local socket, settles the secret files,
// creates the cluster and its certificate authority in memory, issues the
// director its own certificate, and answers `thelb status` - with nothing sent
// on the network. Probing the control VIP, holding it, and joining a cluster
// come with the cluster phase.
package director

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/dmartsapp/thelb/internal/ca"
	"github.com/dmartsapp/thelb/internal/fingerprint"
	"github.com/dmartsapp/thelb/internal/hostnet"
	"github.com/dmartsapp/thelb/internal/localsock"
	"github.com/dmartsapp/thelb/internal/logx"
	"github.com/dmartsapp/thelb/internal/sdnotify"
	"github.com/dmartsapp/thelb/internal/secret"
	"github.com/dmartsapp/thelb/internal/status"
	"github.com/dmartsapp/thelb/internal/version"
)

// Config is how a director was started.
type Config struct {
	ControlVIP        netip.Addr
	ControlPort       uint16
	SecretFile        string
	BackendSecretFile string
	Bootstrap         bool
	Cluster           string
	Name              string
	Interface         string
}

// Deps are what the director reads the world through; tests replace them.
type Deps struct {
	RunDir  string
	Log     *logx.Logger
	Now     func() time.Time
	Host    func(mac string) (fingerprint.Inputs, error)
	PeerUID func(*net.UnixConn) (int, error) // nil: the kernel's answer
	Ready   func()                           // nil: nothing
}

// ErrJoinNotYet is what a director without --bootstrap gets in this build.
var ErrJoinNotYet = errors.New("this director has no --bootstrap, so it would join a cluster - and joining comes with the cluster phase: this build runs only the director that creates one")

// probe is what answered on the control VIP and port.
type probe int

const (
	probeNotChecked probe = iota // this build sends nothing on the network
	probeNothing
	probeTheLB
	probeOther
)

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

// node is the running director, as `status` and `admin` see it.
type node struct {
	mu        sync.Mutex
	cfg       Config
	addr      hostnet.Address
	nodeFP    string
	clusterID string
	created   time.Time
	authority *ca.Authority
	own       *ca.Certificate
	dirSecret secret.Secret
	bkSecret  secret.Secret
}

// Run runs the director until ctx is done.
func Run(ctx context.Context, cfg Config, d Deps) error {
	log := d.Log.Module("director")

	addr, err := hostnet.For(cfg.ControlVIP, cfg.Interface)
	if err != nil {
		return fmt.Errorf("the control VIP %s: %w", cfg.ControlVIP, err)
	}

	// One theLB per node: the socket first, before anything else is touched.
	n := &node{cfg: cfg, addr: addr}
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
	nodeFP := fingerprint.Compute(in)

	log.Warn("control VIP not checked on the network", "vip", cfg.ControlVIP, "port", cfg.ControlPort, "reason", "this build sends nothing on the network yet")
	if err := settleSecrets(cfg, log, probeNotChecked); err != nil {
		return err
	}
	dirSecret, err := secret.Load(cfg.SecretFile)
	if err != nil {
		return err
	}
	bkSecret, err := secret.Load(cfg.BackendSecretFile)
	if err != nil {
		return err
	}

	now := d.Now()
	id, err := clusterID()
	if err != nil {
		return err
	}
	authority, err := ca.New(cfg.Cluster, now)
	if err != nil {
		return err
	}
	own, err := authority.Issue("director", cfg.Name, []netip.Addr{addr.Prefix.Addr(), cfg.ControlVIP}, now, ca.NodeLifetime)
	if err != nil {
		return err
	}
	n.mu.Lock()
	n.nodeFP, n.clusterID, n.created, n.authority, n.own = nodeFP, id, now, authority, own
	n.dirSecret, n.bkSecret = dirSecret, bkSecret
	n.mu.Unlock()

	log.OK("cluster created", "cluster", cfg.Cluster, "id", id)
	log.OK("certificate authority created", "fingerprint", ca.Fingerprint(authority.DER), "not_after", authority.Cert.NotAfter)
	log.OK("certificate issued", "node", cfg.Name, "role", "director", "fingerprint", ca.Fingerprint(own.DER), "not_after", own.Cert.NotAfter)
	log.OK("node fingerprint", "node", cfg.Name, "fingerprint", nodeFP, "hostname", in.Hostname, "interface", addr.Interface)

	served := make(chan struct{})
	go func() {
		srv.Serve(ctx)
		close(served)
	}()
	log.OK("director ready", "socket", srv.Path(), "control_vip", cfg.ControlVIP, "port", cfg.ControlPort, "interface", addr.Interface)
	_ = sdnotify.Notify("READY=1\nSTATUS=director ready, a cluster of one")
	if d.Ready != nil {
		d.Ready()
	}

	renew := time.NewTimer(renewIn(own, d.Now()))
	defer renew.Stop()
	var watchdog <-chan time.Time
	if every := sdnotify.WatchdogInterval(); every > 0 {
		t := time.NewTicker(every)
		defer t.Stop()
		watchdog = t.C
	}
	for {
		select {
		case <-ctx.Done():
			log.OK("stopping")
			_ = sdnotify.Notify("STOPPING=1")
			<-served
			srv.Close()
			log.OK("stopped")
			return nil
		case <-watchdog:
			_ = sdnotify.Notify("WATCHDOG=1")
		case <-renew.C:
			now := d.Now()
			c, err := authority.Issue("director", cfg.Name, []netip.Addr{addr.Prefix.Addr(), cfg.ControlVIP}, now, ca.NodeLifetime)
			if err != nil {
				log.Error("renewing the director's certificate failed", "error", err)
				renew.Reset(time.Minute)
				continue
			}
			n.mu.Lock()
			n.own = c
			n.mu.Unlock()
			log.OK("certificate renewed", "node", cfg.Name, "fingerprint", ca.Fingerprint(c.DER), "not_after", c.Cert.NotAfter)
			renew.Reset(renewIn(c, now))
		}
	}
}

// settleSecrets applies the secret-file rules to both files: the directors'
// decides what this director may do; at a bootstrap, either file that is missing
// is created.
func settleSecrets(cfg Config, log *logx.Logger, p probe) error {
	exists, err := secret.Exists(cfg.SecretFile)
	if err != nil {
		return err
	}
	switch decide(exists, cfg.Bootstrap, p) {
	case actJoin, actWait:
		return ErrJoinNotYet
	case actStopNoSecret:
		return fmt.Errorf("there is no secret in %s, and this director may not create a cluster (no --bootstrap): on a director, run `sudo thelb admin secret show --role director` and put what it prints in %s", cfg.SecretFile, cfg.SecretFile)
	case actRefuseTaken:
		return fmt.Errorf("something that is not theLB answers on %s port %d: the control VIP or port is taken", cfg.ControlVIP, cfg.ControlPort)
	}
	for _, f := range []struct{ path, role string }{{cfg.SecretFile, "director"}, {cfg.BackendSecretFile, "backend"}} {
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
		log.OK("secret file created", "role", f.role, "file", f.path, "mode", "0600")
	}
	return nil
}

// renewIn is when to renew a certificate: two thirds of the way through it.
func renewIn(c *ca.Certificate, now time.Time) time.Duration {
	life := c.Cert.NotAfter.Sub(c.Cert.NotBefore)
	at := c.Cert.NotBefore.Add(life * 2 / 3)
	return max(at.Sub(now), time.Second)
}

func clusterID() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("creating the cluster ID: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// Status is the director's view, for `thelb status`.
func (n *node) Status() status.Status {
	n.mu.Lock()
	defer n.mu.Unlock()
	st := status.Status{View: "director"}
	if n.authority == nil {
		return st // still starting
	}
	st.Cluster = &status.Cluster{Name: n.cfg.Cluster, ID: n.clusterID, Created: n.created, Leader: n.cfg.Name, LeaderSince: n.created}
	st.Nodes = []status.Node{{Node: n.cfg.Name, Role: "leader", Address: n.addr.Prefix.Addr().String(), State: "up", Version: version.Version, NodeFingerprint: n.nodeFP}}
	st.Certificates = []status.Certificate{
		{Certificate: n.authority.Cert.Subject.CommonName, Kind: "ca", Fingerprint: ca.Fingerprint(n.authority.DER), NotAfter: n.authority.Cert.NotAfter},
		{Certificate: n.own.Cert.Subject.CommonName, Kind: "director", Fingerprint: ca.Fingerprint(n.own.DER), NotAfter: n.own.Cert.NotAfter},
	}
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
