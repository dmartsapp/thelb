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

func socket() (int, error) {
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, syscall.NETLINK_ROUTE)
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
	fd, err := socket()
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
	buf := make([]byte, 1<<16)
	for {
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
