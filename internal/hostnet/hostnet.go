// Package hostnet answers questions about this machine's own interfaces and
// addresses - read from the kernel, without sending anything on the network.
package hostnet

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
)

// Address is one address of one interface.
type Address struct {
	Interface string
	MAC       string
	Prefix    netip.Prefix
}

// List returns every address of every interface that is up. It is a variable so
// that tests can describe a machine of their own.
var List = func() ([]Address, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("listing interfaces: %w", err)
	}
	var out []Address
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			return nil, fmt.Errorf("listing the addresses of %s: %w", ifc.Name, err)
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip, ok := netip.AddrFromSlice(ipn.IP)
			if !ok {
				continue
			}
			ones, _ := ipn.Mask.Size()
			out = append(out, Address{Interface: ifc.Name, MAC: ifc.HardwareAddr.String(), Prefix: netip.PrefixFrom(ip.Unmap(), ones)})
		}
	}
	return out, nil
}

// For returns the interface address whose network contains ip - on the
// interface named, if one is. An address equal to ip itself is skipped: that
// is the VIP, left on the interface by an earlier run, not this node's own.
func For(ip netip.Addr, name string) (Address, error) {
	ip = ip.Unmap()
	addrs, err := List()
	if err != nil {
		return Address{}, err
	}
	sawName := false
	for _, a := range addrs {
		if name != "" && a.Interface != name {
			continue
		}
		sawName = true
		if a.Prefix.Addr() == ip || a.Prefix.Bits() >= ip.BitLen() {
			continue
		}
		if a.Prefix.Contains(ip) {
			return a, nil
		}
	}
	switch {
	case name != "" && !sawName:
		return Address{}, fmt.Errorf("interface %s does not exist, is down, or has no address", name)
	case name != "":
		return Address{}, fmt.Errorf("interface %s has no address in the network of %s", name, ip)
	default:
		return Address{}, errors.New("no interface has an address in the network of " + ip.String() + ": name one with --interface")
	}
}

// Route is the address this node would send from to reach dst - through a
// router, when dst is in no network of the node - with its interface. Nothing
// is sent: a UDP socket's connect only looks the route up.
var Route = func(dst netip.Addr) (Address, error) {
	c, err := net.Dial("udp", netip.AddrPortFrom(dst.Unmap(), 9).String())
	if err != nil {
		return Address{}, fmt.Errorf("no route to %s: %w", dst, err)
	}
	la, _ := c.LocalAddr().(*net.UDPAddr)
	_ = c.Close()
	if la == nil {
		return Address{}, fmt.Errorf("no route to %s", dst)
	}
	src, _ := netip.AddrFromSlice(la.IP)
	addrs, err := List()
	if err != nil {
		return Address{}, err
	}
	for _, a := range addrs {
		if a.Prefix.Addr() == src.Unmap() {
			return a, nil
		}
	}
	return Address{}, fmt.Errorf("the route to %s leaves from %s, which no interface that is up has", dst, src)
}
