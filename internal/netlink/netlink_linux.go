//go:build linux

// Package netlink is the little of rtnetlink theLB needs, over the standard
// library: adding, removing and listing addresses, and reading the neighbour
// table.
package netlink

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync/atomic"
	"syscall"
	"time"
)

// Attributes and flags the standard library does not name.
const (
	ifaFlags  = 8  // IFA_FLAGS
	ifaProto  = 11 // IFA_PROTO, upstream since Linux 6.1
	ndaDst    = 1  // NDA_DST
	ndaLLAddr = 2  // NDA_LLADDR

	// IFANoDAD is IFA_F_NODAD: an IPv6 address usable at once.
	IFANoDAD = 0x02
)

// Neighbour states (NUD_*).
const (
	NUDIncomplete = 0x01
	NUDReachable  = 0x02
	NUDStale      = 0x04
	NUDDelay      = 0x08
	NUDProbe      = 0x10
	NUDFailed     = 0x20
	NUDNoARP      = 0x40
	NUDPermanent  = 0x80
)

var native = binary.NativeEndian

var seq atomic.Uint32

func init() { seq.Store(uint32(time.Now().UnixNano())) }

// Addr is an address on an interface.
type Addr struct {
	Index    int
	Prefix   netip.Prefix
	Label    string
	Proto    uint8
	HasProto bool // the kernel reported IFA_PROTO: it has the field
	Flags    uint32
}

// Neigh is a neighbour table entry.
type Neigh struct {
	Index int
	IP    netip.Addr
	MAC   net.HardwareAddr
	State uint16
}

type attr struct {
	typ  uint16
	data []byte
}

func appendAttr(b []byte, typ uint16, data []byte) []byte {
	l := 4 + len(data)
	h := make([]byte, 4)
	native.PutUint16(h[0:2], uint16(l))
	native.PutUint16(h[2:4], typ)
	b = append(b, h...)
	b = append(b, data...)
	for len(b)%4 != 0 {
		b = append(b, 0)
	}
	return b
}

func parseAttrs(b []byte) []attr {
	var out []attr
	for len(b) >= 4 {
		l := int(native.Uint16(b[0:2]))
		if l < 4 || l > len(b) {
			break
		}
		out = append(out, attr{typ: native.Uint16(b[2:4]) &^ (1 << 15), data: b[4:l]})
		l = (l + 3) &^ 3
		if l > len(b) {
			break
		}
		b = b[l:]
	}
	return out
}

func socket(proto int) (int, error) {
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, proto)
	if err != nil {
		return -1, fmt.Errorf("netlink: %w", err)
	}
	if err := syscall.Bind(fd, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		_ = syscall.Close(fd)
		return -1, fmt.Errorf("netlink: %w", err)
	}
	tv := syscall.Timeval{Sec: 5}
	_ = syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv)
	return fd, nil
}

// do sends one request and returns the messages answering it: the dump's, or
// nothing for an acknowledged change.
func do(typ, flags uint16, body []byte) ([]syscall.NetlinkMessage, error) {
	return doProto(syscall.NETLINK_ROUTE, typ, flags, body)
}

func doProto(proto int, typ, flags uint16, body []byte) ([]syscall.NetlinkMessage, error) {
	fd, err := socket(proto)
	if err != nil {
		return nil, err
	}
	defer func() { _ = syscall.Close(fd) }()
	s := seq.Add(1)
	msg := make([]byte, syscall.NLMSG_HDRLEN, syscall.NLMSG_HDRLEN+len(body))
	msg = append(msg, body...)
	native.PutUint32(msg[0:4], uint32(len(msg)))
	native.PutUint16(msg[4:6], typ)
	native.PutUint16(msg[6:8], flags|syscall.NLM_F_REQUEST)
	native.PutUint32(msg[8:12], s)
	if err := syscall.Sendto(fd, msg, 0, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return nil, fmt.Errorf("netlink: %w", err)
	}
	var out []syscall.NetlinkMessage
	for {
		// A new buffer for every read: the messages kept from one read point
		// into it, and the next read must not overwrite them.
		buf := make([]byte, 1<<16)
		n, _, err := syscall.Recvfrom(fd, buf, 0)
		if err != nil {
			return nil, fmt.Errorf("netlink: %w", err)
		}
		msgs, err := syscall.ParseNetlinkMessage(buf[:n])
		if err != nil {
			return nil, fmt.Errorf("netlink: %w", err)
		}
		for _, m := range msgs {
			if m.Header.Seq != s {
				continue
			}
			switch m.Header.Type {
			case syscall.NLMSG_DONE:
				return out, nil
			case syscall.NLMSG_ERROR:
				if len(m.Data) < 4 {
					return nil, errors.New("netlink: a short error")
				}
				if e := int32(native.Uint32(m.Data[0:4])); e != 0 {
					return nil, syscall.Errno(-e)
				}
				return out, nil
			default:
				out = append(out, m)
			}
		}
	}
}

func ifaddrmsg(p netip.Prefix, index int) []byte {
	b := make([]byte, syscall.SizeofIfAddrmsg)
	if p.Addr().Is4() {
		b[0] = syscall.AF_INET
	} else {
		b[0] = syscall.AF_INET6
	}
	b[1] = uint8(p.Bits())
	native.PutUint32(b[4:8], uint32(index))
	return b
}

// AddAddr adds an address - failing if it is there already - with a label
// (IPv4 only), theLB's address protocol, and flags.
func AddAddr(index int, p netip.Prefix, label string, proto uint8, flags uint32) error {
	a := p.Addr().Unmap()
	b := ifaddrmsg(netip.PrefixFrom(a, p.Bits()), index)
	if a.Is4() {
		b = appendAttr(b, syscall.IFA_LOCAL, a.AsSlice())
		if label != "" {
			b = appendAttr(b, syscall.IFA_LABEL, append([]byte(label), 0))
		}
	}
	b = appendAttr(b, syscall.IFA_ADDRESS, a.AsSlice())
	if flags != 0 {
		f := make([]byte, 4)
		native.PutUint32(f, flags)
		b = appendAttr(b, ifaFlags, f)
		b[2] = uint8(flags) // ifa_flags, for kernels that read only that
	}
	if proto != 0 {
		b = appendAttr(b, ifaProto, []byte{proto})
	}
	_, err := do(syscall.RTM_NEWADDR, syscall.NLM_F_ACK|syscall.NLM_F_CREATE|syscall.NLM_F_EXCL, b)
	return err
}

// DelAddr removes an address.
func DelAddr(index int, p netip.Prefix) error {
	a := p.Addr().Unmap()
	b := ifaddrmsg(netip.PrefixFrom(a, p.Bits()), index)
	if a.Is4() {
		b = appendAttr(b, syscall.IFA_LOCAL, a.AsSlice())
	}
	b = appendAttr(b, syscall.IFA_ADDRESS, a.AsSlice())
	_, err := do(syscall.RTM_DELADDR, syscall.NLM_F_ACK, b)
	return err
}

// Addrs lists every address of every interface.
func Addrs() ([]Addr, error) {
	msgs, err := do(syscall.RTM_GETADDR, syscall.NLM_F_DUMP, []byte{syscall.AF_UNSPEC, 0, 0, 0, 0, 0, 0, 0})
	if err != nil {
		return nil, err
	}
	var out []Addr
	for _, m := range msgs {
		if m.Header.Type != syscall.RTM_NEWADDR || len(m.Data) < syscall.SizeofIfAddrmsg {
			continue
		}
		fam, bits, flags, index := m.Data[0], int(m.Data[1]), uint32(m.Data[2]), int(native.Uint32(m.Data[4:8]))
		a := Addr{Index: index, Flags: flags}
		var local, address []byte
		for _, at := range parseAttrs(m.Data[syscall.SizeofIfAddrmsg:]) {
			switch at.typ {
			case syscall.IFA_LOCAL:
				local = at.data
			case syscall.IFA_ADDRESS:
				address = at.data
			case syscall.IFA_LABEL:
				a.Label = cstring(at.data)
			case ifaFlags:
				if len(at.data) >= 4 {
					a.Flags = native.Uint32(at.data)
				}
			case ifaProto:
				if len(at.data) >= 1 {
					a.Proto, a.HasProto = at.data[0], true
				}
			}
		}
		raw := address
		if fam == syscall.AF_INET && local != nil {
			raw = local
		}
		ip, ok := netip.AddrFromSlice(raw)
		if !ok {
			continue
		}
		a.Prefix = netip.PrefixFrom(ip.Unmap(), bits)
		out = append(out, a)
	}
	return out, nil
}

// Neighbour is the neighbour table's entry for ip on the interface, if there is
// one.
func Neighbour(index int, ip netip.Addr) (Neigh, bool, error) {
	msgs, err := do(syscall.RTM_GETNEIGH, syscall.NLM_F_DUMP, make([]byte, 12))
	if err != nil {
		return Neigh{}, false, err
	}
	ip = ip.Unmap()
	for _, m := range msgs {
		if m.Header.Type != syscall.RTM_NEWNEIGH || len(m.Data) < 12 {
			continue
		}
		n := Neigh{Index: int(int32(native.Uint32(m.Data[4:8]))), State: native.Uint16(m.Data[8:10])}
		if n.Index != index {
			continue
		}
		for _, at := range parseAttrs(m.Data[12:]) {
			switch at.typ {
			case ndaDst:
				n.IP, _ = netip.AddrFromSlice(at.data)
				n.IP = n.IP.Unmap()
			case ndaLLAddr:
				n.MAC = net.HardwareAddr(append([]byte(nil), at.data...))
			}
		}
		if n.IP == ip {
			return n, true, nil
		}
	}
	return Neigh{}, false, nil
}

func cstring(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

// Link attributes and flags the standard library does not name.
const (
	iflaLinkInfo = 18 // IFLA_LINKINFO
	iflaInfoKind = 1  // IFLA_INFO_KIND
	nlaNested    = 1 << 15
)

func ifinfomsg(index int, flags, change uint32) []byte {
	b := make([]byte, syscall.SizeofIfInfomsg)
	b[0] = syscall.AF_UNSPEC
	native.PutUint32(b[4:8], uint32(int32(index)))
	native.PutUint32(b[8:12], flags)
	native.PutUint32(b[12:16], change)
	return b
}

// AddDummy creates a dummy interface, up and without ARP.
func AddDummy(name string) error {
	b := ifinfomsg(0, syscall.IFF_UP|syscall.IFF_NOARP, syscall.IFF_UP|syscall.IFF_NOARP)
	b = appendAttr(b, syscall.IFLA_IFNAME, append([]byte(name), 0))
	info := appendAttr(nil, iflaInfoKind, []byte("dummy"))
	b = appendAttr(b, iflaLinkInfo|nlaNested, info)
	_, err := do(syscall.RTM_NEWLINK, syscall.NLM_F_ACK|syscall.NLM_F_CREATE|syscall.NLM_F_EXCL, b)
	return err
}

// DelLink removes an interface.
func DelLink(index int) error {
	_, err := do(syscall.RTM_DELLINK, syscall.NLM_F_ACK, ifinfomsg(index, 0, 0))
	return err
}

func ndmsg(index int, ip netip.Addr, state uint16) []byte {
	b := make([]byte, 12)
	if ip.Is4() {
		b[0] = syscall.AF_INET
	} else {
		b[0] = syscall.AF_INET6
	}
	native.PutUint32(b[4:8], uint32(int32(index)))
	native.PutUint16(b[8:10], state)
	return b
}

// PinNeighbour makes ip's neighbour entry on the interface mac, permanently:
// nothing on the network can change it by answering ARP or neighbour
// discovery.
func PinNeighbour(index int, ip netip.Addr, mac net.HardwareAddr) error {
	ip = ip.Unmap()
	b := ndmsg(index, ip, NUDPermanent)
	b = appendAttr(b, ndaDst, ip.AsSlice())
	b = appendAttr(b, ndaLLAddr, mac)
	_, err := do(syscall.RTM_NEWNEIGH, syscall.NLM_F_ACK|syscall.NLM_F_CREATE|syscall.NLM_F_REPLACE, b)
	return err
}

// UnpinNeighbour removes ip's neighbour entry on the interface; the kernel
// learns it again as it needs it.
func UnpinNeighbour(index int, ip netip.Addr) error {
	ip = ip.Unmap()
	b := ndmsg(index, ip, 0)
	b = appendAttr(b, ndaDst, ip.AsSlice())
	_, err := do(syscall.RTM_DELNEIGH, syscall.NLM_F_ACK, b)
	return err
}

// Generic netlink: the families IPVS and others use.

const (
	genlIDCtrl         = 0x10
	ctrlCmdGetFamily   = 3
	ctrlAttrFamilyID   = 1
	ctrlAttrFamilyName = 2
	netlinkGeneric     = 16
	genlHdrLen         = 4
)

// Attr is a netlink attribute, for callers that build or read their own.
type Attr struct {
	Type uint16
	Data []byte
}

// AppendAttr appends an attribute to b.
func AppendAttr(b []byte, typ uint16, data []byte) []byte { return appendAttr(b, typ, data) }

// AppendNested appends a nested attribute holding attrs.
func AppendNested(b []byte, typ uint16, attrs []byte) []byte {
	return appendAttr(b, typ|nlaNested, attrs)
}

// ParseAttrs reads attributes.
func ParseAttrs(b []byte) []Attr {
	var out []Attr
	for _, a := range parseAttrs(b) {
		out = append(out, Attr{Type: a.typ, Data: a.data})
	}
	return out
}

// Genl is a generic netlink family.
type Genl struct {
	ID      uint16
	Version uint8
}

// GenlFamily resolves a generic netlink family by name.
func GenlFamily(name string, version uint8) (Genl, error) {
	b := []byte{ctrlCmdGetFamily, 1, 0, 0}
	b = appendAttr(b, ctrlAttrFamilyName, append([]byte(name), 0))
	msgs, err := doProto(netlinkGeneric, genlIDCtrl, syscall.NLM_F_ACK, b)
	if err != nil {
		return Genl{}, fmt.Errorf("generic netlink family %s: %w", name, err)
	}
	for _, m := range msgs {
		if len(m.Data) < genlHdrLen {
			continue
		}
		for _, a := range parseAttrs(m.Data[genlHdrLen:]) {
			if a.typ == ctrlAttrFamilyID && len(a.data) >= 2 {
				return Genl{ID: native.Uint16(a.data), Version: version}, nil
			}
		}
	}
	return Genl{}, fmt.Errorf("generic netlink family %s: not found", name)
}

// Do sends cmd with attrs; dump asks for every object. It returns each answer's
// attributes.
func (g Genl) Do(cmd uint8, dump bool, attrs []byte) ([][]Attr, error) {
	b := append([]byte{cmd, g.Version, 0, 0}, attrs...)
	flags := uint16(syscall.NLM_F_ACK)
	if dump {
		flags = syscall.NLM_F_DUMP
	}
	msgs, err := doProto(netlinkGeneric, g.ID, flags, b)
	if err != nil {
		return nil, err
	}
	var out [][]Attr
	for _, m := range msgs {
		if len(m.Data) < genlHdrLen {
			continue
		}
		out = append(out, ParseAttrs(m.Data[genlHdrLen:]))
	}
	return out, nil
}
