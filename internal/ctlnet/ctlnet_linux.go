//go:build linux

// Package ctlnet is a director's network, on Linux: its own address and the
// control port for Raft and the API, and the control VIP - probed before a
// cluster is created, added to the interface and announced by the leader,
// removed when it is no longer the leader.
package ctlnet

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"syscall"
	"time"

	"github.com/dmartsapp/thelb/internal/announce"
	"github.com/dmartsapp/thelb/internal/hostnet"
	"github.com/dmartsapp/thelb/internal/logx"
	"github.com/dmartsapp/thelb/internal/netlink"
)

// Proto is theLB's address protocol (IFA_PROTO): how theLB recognises the
// addresses it added, on kernels that have the field.
const Proto = 116

// Socket options the standard library does not name.
const (
	ipFreebind   = 15 // IP_FREEBIND
	ipv6Freebind = 78 // IPV6_FREEBIND
)

// Announcements: three rounds over two seconds - switches and hosts miss single
// packets.
var announceAt = []time.Duration{0, time.Second, 2 * time.Second}

// Net is a director's network.
type Net struct {
	vip   netip.Addr
	port  uint16
	local hostnet.Address
	ifc   *net.Interface
	log   *logx.Logger

	mu     sync.Mutex
	ln     net.Listener
	cancel context.CancelFunc
}

// New is the network of a director whose control VIP is vip, on the interface
// of its address local.
func New(vip netip.Addr, port uint16, local hostnet.Address, log *logx.Logger) (*Net, error) {
	ifc, err := net.InterfaceByName(local.Interface)
	if err != nil {
		return nil, fmt.Errorf("interface %s: %w", local.Interface, err)
	}
	if len(ifc.HardwareAddr) != 6 {
		return nil, fmt.Errorf("interface %s has no Ethernet address: the control VIP needs ARP or neighbour discovery", local.Interface)
	}
	return &Net{vip: vip.Unmap(), port: port, local: local, ifc: ifc, log: log}, nil
}

func (n *Net) prefix() netip.Prefix { return netip.PrefixFrom(n.vip, n.vip.BitLen()) }

// label is the IPv4 label theLB's addresses carry - "eth0:thelb" - or "" if
// the interface's name leaves no room for it.
func (n *Net) label() string {
	if l := n.ifc.Name + ":thelb"; len(l) <= 15 && n.vip.Is4() {
		return l
	}
	return ""
}

// ListenNode listens on this node's address and the control port.
func (n *Net) ListenNode() (net.Listener, error) {
	ln, err := net.Listen("tcp", netip.AddrPortFrom(n.local.Prefix.Addr(), n.port).String())
	if err != nil {
		return nil, fmt.Errorf("listening on this node's address: %w", err)
	}
	return ln, nil
}

func (n *Net) dialer() *net.Dialer {
	return &net.Dialer{Timeout: time.Second, LocalAddr: &net.TCPAddr{IP: n.local.Prefix.Addr().AsSlice()}}
}

// DialVIP connects to the control VIP and port, from this node's address.
func (n *Net) DialVIP(ctx context.Context) (net.Conn, error) {
	return n.dialer().DialContext(ctx, "tcp", netip.AddrPortFrom(n.vip, n.port).String())
}

// Dial connects to another director.
func (n *Net) Dial(ctx context.Context, addr string) (net.Conn, error) {
	return n.dialer().DialContext(ctx, "tcp", addr)
}

// VIPHolder asks the link who has the control VIP.
func (n *Net) VIPHolder(ctx context.Context) (string, error) {
	wait := time.Second
	if d, ok := ctx.Deadline(); ok && time.Until(d) < wait {
		wait = time.Until(d)
	}
	return announce.Probe(n.ifc.Index, n.ifc.HardwareAddr, n.local.Prefix.Addr(), n.vip, wait)
}

// Take listens on the control VIP, adds it to the interface, and announces it.
// The listener comes first - bound before the address exists - so no
// connection to the VIP is ever refused while it moves here.
func (n *Net) Take() (net.Listener, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.ln != nil {
		return n.ln, nil
	}
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		err := c.Control(func(fd uintptr) {
			if n.vip.Is4() {
				serr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, ipFreebind, 1)
			} else {
				serr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IPV6, ipv6Freebind, 1)
			}
		})
		if err != nil {
			return err
		}
		return serr
	}}
	ln, err := lc.Listen(context.Background(), "tcp", netip.AddrPortFrom(n.vip, n.port).String())
	if err != nil {
		return nil, fmt.Errorf("listening on the control VIP: %w", err)
	}
	var flags uint32
	if n.vip.Is6() {
		flags = netlink.IFANoDAD // the cluster, not DAD, keeps the VIP unique
	}
	if err := netlink.AddAddr(n.ifc.Index, n.prefix(), n.label(), Proto, flags); err != nil && !errors.Is(err, syscall.EEXIST) {
		_ = ln.Close()
		return nil, fmt.Errorf("adding the control VIP to %s: %w", n.ifc.Name, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	n.ln, n.cancel = ln, cancel
	go n.announce(ctx)
	return ln, nil
}

func (n *Net) announce(ctx context.Context) {
	start := time.Now()
	for _, at := range announceAt {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(start.Add(at))):
		}
		if err := announce.Announce(n.ifc.Index, n.ifc.HardwareAddr, n.vip); err != nil {
			n.log.Warn("announcing the control VIP failed", "vip", n.vip, "interface", n.ifc.Name, "error", err)
			return
		}
	}
}

// Repair puts the control VIP back on its interface, and announces it again,
// if someone removed it while this director holds it: true if it did.
func (n *Net) Repair() (bool, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.ln == nil {
		return false, nil
	}
	addrs, err := netlink.Addrs()
	if err != nil {
		return false, err
	}
	for _, a := range addrs {
		if a.Index == n.ifc.Index && a.Prefix.Addr() == n.vip {
			return false, nil
		}
	}
	var flags uint32
	if n.vip.Is6() {
		flags = netlink.IFANoDAD
	}
	if err := netlink.AddAddr(n.ifc.Index, n.prefix(), n.label(), Proto, flags); err != nil && !errors.Is(err, syscall.EEXIST) {
		return false, fmt.Errorf("adding the control VIP to %s again: %w", n.ifc.Name, err)
	}
	n.cancel()
	ctx, cancel := context.WithCancel(context.Background())
	n.cancel = cancel
	go n.announce(ctx)
	return true, nil
}

// Release removes the control VIP, then closes its listener.
func (n *Net) Release() {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.ln == nil {
		return
	}
	n.cancel()
	if err := netlink.DelAddr(n.ifc.Index, n.prefix()); err != nil && !errors.Is(err, syscall.EADDRNOTAVAIL) && !errors.Is(err, syscall.ENOENT) {
		n.log.Error("removing the control VIP failed", "vip", n.vip, "interface", n.ifc.Name, "error", err)
	}
	_ = n.ln.Close()
	n.ln, n.cancel = nil, nil
}

// Cleanup removes what an earlier run of theLB left on this node's interfaces
// - a crash cleans up nothing: the control VIP, and every service VIP, by
// theLB's address protocol or label - and refuses if the control VIP is here
// and theLB did not add it.
func (n *Net) Cleanup() error {
	addrs, err := netlink.Addrs()
	if err != nil {
		return err
	}
	kernelHasProto := false
	for _, a := range addrs {
		kernelHasProto = kernelHasProto || a.HasProto
	}
	for _, a := range addrs {
		ifc, _ := net.InterfaceByIndex(a.Index)
		name := fmt.Sprint(a.Index)
		if ifc != nil {
			name = ifc.Name
		}
		if ifc != nil && ifc.Name == "thelb0" {
			continue // a backend's, on a node that runs both: the backend's to clear
		}
		marked := (a.HasProto && a.Proto == Proto) || (a.Prefix.Addr().Is4() && a.Label == name+":thelb")
		control := a.Prefix.Addr() == n.vip
		if control && !marked && (kernelHasProto || a.Prefix.Bits() != n.vip.BitLen()) {
			return fmt.Errorf("the control VIP %s is already on %s, and theLB did not add it: remove it, or choose another --vip", n.vip, name)
		}
		if !marked && !control {
			continue
		}
		if err := netlink.DelAddr(a.Index, a.Prefix); err != nil && !errors.Is(err, syscall.EADDRNOTAVAIL) {
			return fmt.Errorf("removing %s, which an earlier run left on %s: %w", a.Prefix.Addr(), name, err)
		}
		if control {
			n.log.Warn("removed the control VIP an earlier run left", "vip", n.vip, "interface", name)
		} else {
			n.log.Warn("removed a VIP an earlier run left", "vip", a.Prefix.Addr(), "interface", name)
		}
	}
	return nil
}

// MACOf is the neighbour table's MAC address for ip, on the interface.
func (n *Net) MACOf(ip netip.Addr) string {
	e, ok, err := netlink.Neighbour(n.ifc.Index, ip)
	if err != nil || !ok || e.State&(netlink.NUDFailed|netlink.NUDIncomplete) != 0 || len(e.MAC) == 0 {
		return ""
	}
	return e.MAC.String()
}
