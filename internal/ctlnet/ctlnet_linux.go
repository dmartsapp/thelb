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
	"os"
	"strings"
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
	cancel context.CancelFunc // the announcements
	renew  context.CancelFunc // the lease's renewal
	lease  time.Duration
}

// DefaultLease is how long the kernel keeps the control VIP after its last
// renewal, until the cluster says otherwise.
const DefaultLease = 6 * time.Second

// SetLease sets how long the kernel keeps the control VIP after its last
// renewal - it is renewed every third of that - from the next renewal on.
func (n *Net) SetLease(d time.Duration) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.lease = d
}

func (n *Net) leaseLocked() time.Duration {
	if n.lease < time.Second {
		return DefaultLease
	}
	return n.lease
}

// put adds the control VIP, or renews it, leased from the kernel: a theLB that
// is killed, or hangs, stops renewing, and the kernel removes the VIP by itself
// when the lease lapses - nothing is left answering for it. n.mu is held.
func (n *Net) put() error {
	var flags uint32
	if n.vip.Is6() {
		flags = netlink.IFANoDAD // the cluster, not DAD, keeps the VIP unique
	}
	if err := netlink.PutAddr(n.ifc.Index, n.prefix(), n.label(), Proto, flags, n.leaseLocked()); err != nil {
		return fmt.Errorf("adding the control VIP to %s: %w", n.ifc.Name, err)
	}
	return nil
}

// present is whether the control VIP is on its interface. n.mu is held.
func (n *Net) present() (bool, error) {
	addrs, err := netlink.Addrs()
	if err != nil {
		return false, err
	}
	for _, a := range addrs {
		if a.Index == n.ifc.Index && a.Prefix.Addr() == n.vip {
			return true, nil
		}
	}
	return false, nil
}

// keep renews the control VIP's lease every third of it while this director
// holds it - and puts it back, announced again, if someone removed it.
func (n *Net) keep(ctx context.Context) {
	failed := ""
	for {
		n.mu.Lock()
		every := n.leaseLocked() / 3
		n.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
		n.mu.Lock()
		if ctx.Err() != nil || n.ln == nil {
			n.mu.Unlock()
			return
		}
		here, err := n.present()
		if err == nil {
			err = n.put()
		}
		if err == nil && !here {
			n.cancel()
			actx, cancel := context.WithCancel(context.Background())
			n.cancel = cancel
			go n.announce(actx)
		}
		n.mu.Unlock()
		switch {
		case err != nil:
			if err.Error() != failed {
				n.log.Error("renewing the control VIP's lease failed: the kernel removes it when the lease lapses", "vip", n.vip, "interface", n.ifc.Name, "error", err)
			}
			failed = err.Error()
		case !here:
			failed = ""
			n.log.Warn("drift repaired: the control VIP was removed from its interface; added and announced again", "vip", n.vip, "interface", n.ifc.Name)
		default:
			failed = ""
		}
	}
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

// Holder asks the link who has ip.
func (n *Net) Holder(ctx context.Context, ip netip.Addr) (string, error) {
	wait := time.Second
	if d, ok := ctx.Deadline(); ok && time.Until(d) < wait {
		wait = time.Until(d)
	}
	return announce.Probe(n.ifc.Index, n.ifc.HardwareAddr, n.local.Prefix.Addr(), ip, wait)
}

// Uplink is whether this node still reaches its network: a gateway on the
// control VIP's interface - the default route's, or another route's - answers;
// with none, the link is up.
func (n *Net) Uplink(ctx context.Context) (bool, string) {
	if gw, ok := gateway(n.ifc.Name, n.vip.Is4()); ok {
		mac, err := n.Holder(ctx, gw)
		switch {
		case err != nil:
			return false, fmt.Sprintf("looking for its gateway %s failed: %v", gw, err)
		case mac == "":
			return false, fmt.Sprintf("its gateway %s does not answer", gw)
		}
		return true, ""
	}
	b, err := os.ReadFile("/sys/class/net/" + n.ifc.Name + "/carrier")
	if err != nil || strings.TrimSpace(string(b)) != "1" {
		return false, "the link of " + n.ifc.Name + " is down"
	}
	return true, ""
}

// Gateway is a gateway on the interface, as Uplink asks it: for doctor.
func Gateway(ifname string, v4 bool) (netip.Addr, bool) { return gateway(ifname, v4) }

// gateway is a gateway on the interface, from the kernel's routes: the default
// route's if it has one there, or another route's.
func gateway(ifname string, v4 bool) (netip.Addr, bool) {
	if v4 {
		b, err := os.ReadFile("/proc/net/route")
		if err != nil {
			return netip.Addr{}, false
		}
		return gateway4(string(b), ifname)
	}
	b, err := os.ReadFile("/proc/net/ipv6_route")
	if err != nil {
		return netip.Addr{}, false
	}
	return gateway6(string(b), ifname)
}

// gateway4 reads /proc/net/route: Iface Destination Gateway Flags ... Mask ...,
// addresses in hexadecimal, in the host's byte order (little-endian here).
func gateway4(table, ifname string) (netip.Addr, bool) {
	var other netip.Addr
	for _, line := range strings.Split(table, "\n")[1:] {
		f := strings.Fields(line)
		if len(f) < 8 || f[0] != ifname || f[2] == "00000000" {
			continue
		}
		var gw [4]byte
		if _, err := fmt.Sscanf(f[2], "%02x%02x%02x%02x", &gw[3], &gw[2], &gw[1], &gw[0]); err != nil {
			continue
		}
		a := netip.AddrFrom4(gw)
		if f[1] == "00000000" && f[7] == "00000000" {
			return a, true
		}
		if !other.IsValid() {
			other = a
		}
	}
	return other, other.IsValid()
}

// gateway6 reads /proc/net/ipv6_route: destination, its length, source, its
// length, next hop, metric, references, use, flags, interface.
func gateway6(table, ifname string) (netip.Addr, bool) {
	var other netip.Addr
	for _, line := range strings.Split(table, "\n") {
		f := strings.Fields(line)
		if len(f) < 10 || f[9] != ifname || f[4] == strings.Repeat("0", 32) {
			continue
		}
		var hop [16]byte
		ok := len(f[4]) == 32
		for i := 0; ok && i < 16; i++ {
			_, err := fmt.Sscanf(f[4][2*i:2*i+2], "%02x", &hop[i])
			ok = err == nil
		}
		if !ok {
			continue
		}
		a := netip.AddrFrom16(hop)
		if f[0] == strings.Repeat("0", 32) && f[1] == "00" {
			return a, true
		}
		if !other.IsValid() {
			other = a
		}
	}
	return other, other.IsValid()
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
	if err := n.put(); err != nil {
		_ = ln.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	rctx, renew := context.WithCancel(context.Background())
	n.ln, n.cancel, n.renew = ln, cancel, renew
	go n.announce(ctx)
	go n.keep(rctx)
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
	here, err := n.present()
	if err != nil || here {
		return false, err
	}
	if err := n.put(); err != nil {
		return false, err
	}
	n.cancel()
	ctx, cancel := context.WithCancel(context.Background())
	n.cancel = cancel
	go n.announce(ctx)
	return true, nil
}

// Announce announces the control VIP again, while this director holds it: the
// network's caches pointed back here, away from another machine answering for
// it.
func (n *Net) Announce() {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.ln == nil {
		return
	}
	n.cancel()
	ctx, cancel := context.WithCancel(context.Background())
	n.cancel = cancel
	go n.announce(ctx)
}

// Release removes the control VIP, then closes its listener.
func (n *Net) Release() {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.ln == nil {
		return
	}
	n.cancel()
	n.renew()
	if err := netlink.DelAddr(n.ifc.Index, n.prefix()); err != nil && !errors.Is(err, syscall.EADDRNOTAVAIL) && !errors.Is(err, syscall.ENOENT) {
		n.log.Error("removing the control VIP failed", "vip", n.vip, "interface", n.ifc.Name, "error", err)
	}
	_ = n.ln.Close()
	n.ln, n.cancel, n.renew = nil, nil, nil
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
