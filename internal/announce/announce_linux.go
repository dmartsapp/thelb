//go:build linux

// Package announce tells the link who holds a VIP, and asks who does: ARP for
// IPv4 - gratuitous requests and replies, and RFC 5227 probes - and, for IPv6,
// unsolicited Neighbor Advertisements and Neighbor Solicitations.
package announce

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"syscall"
	"time"
)

const (
	ethPARP    = 0x0806
	arpLen     = 28
	icmpNS     = 135
	icmpNA     = 136
	optSLLA    = 1
	optTLLA    = 2
	naOverride = 0x20
)

var broadcast = net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}

func htons(v uint16) uint16 {
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], v)
	return binary.NativeEndian.Uint16(b[:])
}

// Announce sends one round of announcements that vip is at mac, on the
// interface: for IPv4 a gratuitous ARP request and reply, for IPv6 an
// unsolicited Neighbor Advertisement, overriding, to all nodes. The caller
// repeats it: switches and hosts miss single packets.
func Announce(index int, mac net.HardwareAddr, vip netip.Addr) error {
	vip = vip.Unmap()
	if vip.Is4() {
		fd, err := arpSocket(index)
		if err != nil {
			return err
		}
		defer func() { _ = syscall.Close(fd) }()
		for _, op := range []uint16{1, 2} {
			tha := net.HardwareAddr(make([]byte, 6))
			if op == 2 {
				tha = mac
			}
			if err := sendARP(fd, index, op, mac, vip, tha, vip); err != nil {
				return err
			}
		}
		return nil
	}
	fd, err := icmp6Socket(index, vip)
	if err != nil {
		return err
	}
	defer func() { _ = syscall.Close(fd) }()
	msg := make([]byte, 24, 32)
	msg[0] = icmpNA
	msg[4] = naOverride
	copy(msg[8:24], vip.AsSlice())
	msg = append(msg, optTLLA, 1)
	msg = append(msg, mac...)
	all := netip.MustParseAddr("ff02::1")
	return syscall.Sendto(fd, msg, 0, &syscall.SockaddrInet6{Addr: all.As16(), ZoneId: uint32(index)})
}

// Probe asks the link who has vip, from src (this node's address on the
// interface, for IPv6; IPv4 probes come from 0.0.0.0), and waits up to wait for
// an answer - asking again a third of the way through, as one question or its
// answer can be lost, and "no one has it" decides things. It returns the MAC
// address of a host that has vip, or "" if none answered.
func Probe(index int, mac net.HardwareAddr, src, vip netip.Addr, wait time.Duration) (string, error) {
	m, err := ProbeAll(index, mac, src, []netip.Addr{vip}, wait)
	return m[vip.Unmap()], err
}

// ProbeAll asks the link who has each of vips at once, as Probe does one: every
// question sent, then the answers read for up to wait - asking again, a third
// of the way through, for those not answered yet. It returns the MAC address
// that answered for each VIP answered for; it returns early once every one has
// been.
func ProbeAll(index int, mac net.HardwareAddr, src netip.Addr, vips []netip.Addr, wait time.Duration) (map[netip.Addr]string, error) {
	out := map[netip.Addr]string{}
	var v4, v6 []netip.Addr
	for _, v := range vips {
		if v = v.Unmap(); v.Is4() {
			v4 = append(v4, v)
		} else {
			v6 = append(v6, v)
		}
	}
	deadline := time.Now().Add(wait)
	if len(v4) > 0 {
		if err := probeARP(index, mac, v4, deadline, wait, out); err != nil {
			return out, err
		}
	}
	if len(v6) > 0 {
		if err := probeNS(index, mac, src, v6, deadline, wait, out); err != nil {
			return out, err
		}
	}
	return out, nil
}

// unanswered is the VIPs of want that out has no answer for.
func unanswered(want []netip.Addr, out map[netip.Addr]string) []netip.Addr {
	var left []netip.Addr
	for _, v := range want {
		if _, ok := out[v]; !ok {
			left = append(left, v)
		}
	}
	return left
}

func probeARP(index int, mac net.HardwareAddr, vips []netip.Addr, deadline time.Time, wait time.Duration, out map[netip.Addr]string) error {
	fd, err := arpSocket(index)
	if err != nil {
		return err
	}
	defer func() { _ = syscall.Close(fd) }()
	want := map[netip.Addr]bool{}
	for _, v := range vips {
		want[v] = true
	}
	ask := func(vs []netip.Addr) error {
		for _, v := range vs {
			if err := sendARP(fd, index, 1, mac, netip.IPv4Unspecified(), make([]byte, 6), v); err != nil {
				return err
			}
		}
		return nil
	}
	if err := ask(vips); err != nil {
		return err
	}
	again := time.Now().Add(wait / 3)
	buf := make([]byte, 1500)
	for time.Now().Before(deadline) && len(unanswered(vips, out)) > 0 {
		if !again.IsZero() && time.Now().After(again) {
			again = time.Time{}
			if err := ask(unanswered(vips, out)); err != nil {
				return err
			}
		}
		n, _, err := syscall.Recvfrom(fd, buf, 0)
		if err != nil {
			if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EINTR) {
				continue
			}
			return fmt.Errorf("reading ARP: %w", err)
		}
		if n < arpLen || binary.BigEndian.Uint16(buf[2:4]) != 0x0800 || buf[4] != 6 || buf[5] != 4 {
			continue
		}
		sha := net.HardwareAddr(buf[8:14])
		spa, _ := netip.AddrFromSlice(buf[14:18])
		if want[spa] && sha.String() != mac.String() {
			if _, ok := out[spa]; !ok {
				out[spa] = sha.String()
			}
		}
	}
	return nil
}

func probeNS(index int, mac net.HardwareAddr, src netip.Addr, vips []netip.Addr, deadline time.Time, wait time.Duration, out map[netip.Addr]string) error {
	fd, err := icmp6Socket(index, src)
	if err != nil {
		return err
	}
	defer func() { _ = syscall.Close(fd) }()
	want := map[netip.Addr]bool{}
	for _, v := range vips {
		want[v] = true
	}
	ask := func(vs []netip.Addr) error {
		for _, vip := range vs {
			msg := make([]byte, 24, 32)
			msg[0] = icmpNS
			copy(msg[8:24], vip.AsSlice())
			msg = append(msg, optSLLA, 1)
			msg = append(msg, mac...)
			v := vip.As16()
			solicited := [16]byte{0xff, 0x02, 10: 0, 11: 0x01, 12: 0xff, 13: v[13], 14: v[14], 15: v[15]}
			if err := sendto(fd, msg, &syscall.SockaddrInet6{Addr: solicited, ZoneId: uint32(index)}, "sending a neighbor solicitation"); err != nil {
				return err
			}
		}
		return nil
	}
	if err := ask(vips); err != nil {
		return err
	}
	again := time.Now().Add(wait / 3)
	buf := make([]byte, 1500)
	for time.Now().Before(deadline) && len(unanswered(vips, out)) > 0 {
		if !again.IsZero() && time.Now().After(again) {
			again = time.Time{}
			if err := ask(unanswered(vips, out)); err != nil {
				return err
			}
		}
		n, _, err := syscall.Recvfrom(fd, buf, 0)
		if err != nil {
			if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EINTR) {
				continue
			}
			return fmt.Errorf("reading ICMPv6: %w", err)
		}
		if n < 24 || buf[0] != icmpNA {
			continue
		}
		target, _ := netip.AddrFromSlice(buf[8:24])
		if !want[target] {
			continue
		}
		who := "unknown"
		for o := buf[24:n]; len(o) >= 8 && o[1] > 0 && int(o[1])*8 <= len(o); o = o[int(o[1])*8:] {
			if o[0] == optTLLA {
				who = net.HardwareAddr(o[2:8]).String()
			}
		}
		if who != mac.String() {
			if _, ok := out[target]; !ok {
				out[target] = who
			}
		}
	}
	return nil
}

func arpSocket(index int) (int, error) {
	fd, err := syscall.Socket(syscall.AF_PACKET, syscall.SOCK_DGRAM|syscall.SOCK_CLOEXEC, int(htons(ethPARP)))
	if err != nil {
		return -1, fmt.Errorf("an ARP socket: %w", err)
	}
	if err := syscall.Bind(fd, &syscall.SockaddrLinklayer{Protocol: htons(ethPARP), Ifindex: index}); err != nil {
		_ = syscall.Close(fd)
		return -1, fmt.Errorf("an ARP socket: %w", err)
	}
	tv := syscall.Timeval{Usec: 100000}
	_ = syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv)
	return fd, nil
}

func sendARP(fd, index int, op uint16, sha net.HardwareAddr, spa netip.Addr, tha net.HardwareAddr, tpa netip.Addr) error {
	p := make([]byte, arpLen)
	binary.BigEndian.PutUint16(p[0:2], 1)      // Ethernet
	binary.BigEndian.PutUint16(p[2:4], 0x0800) // IPv4
	p[4], p[5] = 6, 4
	binary.BigEndian.PutUint16(p[6:8], op)
	copy(p[8:14], sha)
	copy(p[14:18], spa.AsSlice())
	copy(p[18:24], tha)
	copy(p[24:28], tpa.AsSlice())
	to := &syscall.SockaddrLinklayer{Protocol: htons(ethPARP), Ifindex: index, Halen: 6}
	copy(to.Addr[:], broadcast)
	return sendto(fd, p, to, "sending ARP")
}

// sendto sends, waiting a little while the interface's queue is full: a probe
// of thousands of VIPs sends as many packets at once.
func sendto(fd int, p []byte, to syscall.Sockaddr, what string) error {
	for i := 0; ; i++ {
		err := syscall.Sendto(fd, p, 0, to)
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.ENOBUFS) && !errors.Is(err, syscall.EAGAIN) || i == 100 {
			return fmt.Errorf("%s: %w", what, err)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// icmp6Socket is a raw ICMPv6 socket sending from src on the interface, with
// the hop limit neighbour discovery needs; the kernel computes the checksum.
func icmp6Socket(index int, src netip.Addr) (int, error) {
	fd, err := syscall.Socket(syscall.AF_INET6, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, syscall.IPPROTO_ICMPV6)
	if err != nil {
		return -1, fmt.Errorf("an ICMPv6 socket: %w", err)
	}
	for _, o := range []struct{ opt, v int }{{syscall.IPV6_MULTICAST_HOPS, 255}, {syscall.IPV6_UNICAST_HOPS, 255}, {syscall.IPV6_MULTICAST_IF, index}, {syscall.IPV6_MULTICAST_LOOP, 0}} {
		if err := syscall.SetsockoptInt(fd, syscall.IPPROTO_IPV6, o.opt, o.v); err != nil {
			_ = syscall.Close(fd)
			return -1, fmt.Errorf("an ICMPv6 socket: %w", err)
		}
	}
	sa := &syscall.SockaddrInet6{Addr: src.As16()}
	if src.IsLinkLocalUnicast() {
		sa.ZoneId = uint32(index)
	}
	if err := syscall.Bind(fd, sa); err != nil {
		_ = syscall.Close(fd)
		return -1, fmt.Errorf("an ICMPv6 socket from %s: %w", src, err)
	}
	tv := syscall.Timeval{Usec: 100000}
	_ = syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv)
	return fd, nil
}
