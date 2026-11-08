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
// an answer. It returns the MAC address of a host that has vip, or "" if none
// answered.
func Probe(index int, mac net.HardwareAddr, src, vip netip.Addr, wait time.Duration) (string, error) {
	vip = vip.Unmap()
	deadline := time.Now().Add(wait)
	if vip.Is4() {
		fd, err := arpSocket(index)
		if err != nil {
			return "", err
		}
		defer func() { _ = syscall.Close(fd) }()
		if err := sendARP(fd, index, 1, mac, netip.IPv4Unspecified(), make([]byte, 6), vip); err != nil {
			return "", err
		}
		buf := make([]byte, 1500)
		for time.Now().Before(deadline) {
			n, _, err := syscall.Recvfrom(fd, buf, 0)
			if err != nil {
				if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EINTR) {
					continue
				}
				return "", fmt.Errorf("reading ARP: %w", err)
			}
			if n < arpLen || binary.BigEndian.Uint16(buf[2:4]) != 0x0800 || buf[4] != 6 || buf[5] != 4 {
				continue
			}
			sha := net.HardwareAddr(buf[8:14])
			spa, _ := netip.AddrFromSlice(buf[14:18])
			if spa == vip && sha.String() != mac.String() {
				return sha.String(), nil
			}
		}
		return "", nil
	}
	fd, err := icmp6Socket(index, src)
	if err != nil {
		return "", err
	}
	defer func() { _ = syscall.Close(fd) }()
	msg := make([]byte, 24, 32)
	msg[0] = icmpNS
	copy(msg[8:24], vip.AsSlice())
	msg = append(msg, optSLLA, 1)
	msg = append(msg, mac...)
	v := vip.As16()
	solicited := [16]byte{0xff, 0x02, 10: 0, 11: 0x01, 12: 0xff, 13: v[13], 14: v[14], 15: v[15]}
	if err := syscall.Sendto(fd, msg, 0, &syscall.SockaddrInet6{Addr: solicited, ZoneId: uint32(index)}); err != nil {
		return "", fmt.Errorf("sending a neighbor solicitation: %w", err)
	}
	buf := make([]byte, 1500)
	for time.Now().Before(deadline) {
		n, _, err := syscall.Recvfrom(fd, buf, 0)
		if err != nil {
			if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EINTR) {
				continue
			}
			return "", fmt.Errorf("reading ICMPv6: %w", err)
		}
		if n < 24 || buf[0] != icmpNA {
			continue
		}
		target, _ := netip.AddrFromSlice(buf[8:24])
		if target != vip {
			continue
		}
		who := "unknown"
		for o := buf[24:n]; len(o) >= 8 && o[1] > 0 && int(o[1])*8 <= len(o); o = o[int(o[1])*8:] {
			if o[0] == optTLLA {
				who = net.HardwareAddr(o[2:8]).String()
			}
		}
		if who != mac.String() {
			return who, nil
		}
	}
	return "", nil
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
	if err := syscall.Sendto(fd, p, 0, to); err != nil {
		return fmt.Errorf("sending ARP: %w", err)
	}
	return nil
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
