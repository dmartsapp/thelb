//go:build linux

package ipvs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"syscall"

	"github.com/dmartsapp/thelb/internal/netlink"
)

// IPVS's generic netlink interface (include/uapi/linux/ip_vs.h).
const (
	cmdNewService = 1
	cmdSetService = 2
	cmdDelService = 3
	cmdGetService = 4
	cmdNewDest    = 5
	cmdSetDest    = 6
	cmdDelDest    = 7
	cmdGetDest    = 8
	cmdNewDaemon  = 9
	cmdDelDaemon  = 10
	cmdGetDaemon  = 11

	attrService = 1
	attrDest    = 2
	attrDaemon  = 3

	daemonState    = 1 // u32: stateMaster or stateBackup
	daemonMcastIfn = 2 // the interface's name
	daemonSyncID   = 3 // u32
	stateMaster    = 1 // IP_VS_STATE_MASTER
	stateBackup    = 2 // IP_VS_STATE_BACKUP

	svcAF        = 1
	svcProtocol  = 2
	svcAddr      = 3
	svcPort      = 4
	svcSchedName = 6
	svcFlags     = 7
	svcTimeout   = 8
	svcNetmask   = 9

	destAddr       = 1
	destPort       = 2
	destFwdMethod  = 3
	destWeight     = 4
	destUThresh    = 5
	destLThresh    = 6
	destActive     = 7 // IPVS_DEST_ATTR_ACTIVE_CONNS
	destInactive   = 8 // IPVS_DEST_ATTR_INACT_CONNS
	destAddrFamily = 11
	destStats64    = 12
	destTunType    = 13

	statsConns  = 1
	statsInPkts = 2
	statsInByte = 4

	fwdDirectRoute = 3 // IP_VS_CONN_F_DROUTE
	fwdTunnel      = 2 // IP_VS_CONN_F_TUNNEL
	fwdMask        = 7 // IP_VS_CONN_F_FWD_MASK
	tunTypeIPIP    = 0 // IP_VS_CONN_F_TUNNEL_TYPE_IPIP
)

var native = binary.NativeEndian

var (
	family     netlink.Genl
	familyErr  error
	familyOnce sync.Once
)

// Conn is IPVS in this network namespace.
type Conn struct{ g netlink.Genl }

// Open finds IPVS's netlink family: the ip_vs module must be loaded.
func Open() (*Conn, error) {
	familyOnce.Do(func() { family, familyErr = netlink.GenlFamily("IPVS", 1) })
	if familyErr != nil {
		return nil, fmt.Errorf("IPVS is not available (is the ip_vs module loaded?): %w", familyErr)
	}
	return &Conn{g: family}, nil
}

func af(a netip.Addr) uint16 {
	if a.Unmap().Is4() {
		return syscall.AF_INET
	}
	return syscall.AF_INET6
}

func u16(v uint16) []byte { b := make([]byte, 2); native.PutUint16(b, v); return b }
func u32(v uint32) []byte { b := make([]byte, 4); native.PutUint32(b, v); return b }
func be16(v uint16) []byte {
	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, v)
	return b
}

func addr16(a netip.Addr) []byte {
	b := make([]byte, 16)
	copy(b, a.Unmap().AsSlice())
	return b
}

// serviceAttrs identifies a service; full adds what creating one needs.
func serviceAttrs(s Service, full bool) []byte {
	var b []byte
	b = netlink.AppendAttr(b, svcAF, u16(af(s.Addr)))
	b = netlink.AppendAttr(b, svcProtocol, u16(s.Protocol))
	b = netlink.AppendAttr(b, svcAddr, addr16(s.Addr))
	b = netlink.AppendAttr(b, svcPort, be16(s.Port))
	if full {
		b = netlink.AppendAttr(b, svcSchedName, append([]byte(s.Scheduler), 0))
		b = netlink.AppendAttr(b, svcFlags, append(u32(0), u32(^uint32(0))...))
		b = netlink.AppendAttr(b, svcTimeout, u32(0))
		mask := ^uint32(0)
		if af(s.Addr) == syscall.AF_INET6 {
			mask = 128
		}
		b = netlink.AppendAttr(b, svcNetmask, u32(mask))
	}
	return netlink.AppendNested(nil, attrService, b)
}

func destAttrs(d Dest) []byte {
	var b []byte
	b = netlink.AppendAttr(b, destAddr, addr16(d.Addr))
	b = netlink.AppendAttr(b, destPort, be16(d.Port))
	if d.Tunnel {
		b = netlink.AppendAttr(b, destFwdMethod, u32(fwdTunnel))
		b = netlink.AppendAttr(b, destTunType, []byte{tunTypeIPIP})
	} else {
		b = netlink.AppendAttr(b, destFwdMethod, u32(fwdDirectRoute))
	}
	b = netlink.AppendAttr(b, destWeight, u32(d.Weight))
	b = netlink.AppendAttr(b, destUThresh, u32(0))
	b = netlink.AppendAttr(b, destLThresh, u32(0))
	b = netlink.AppendAttr(b, destAddrFamily, u16(af(d.Addr)))
	return netlink.AppendNested(nil, attrDest, b)
}

// AddService creates a virtual service.
func (c *Conn) AddService(s Service) error {
	_, err := c.g.Do(cmdNewService, false, serviceAttrs(s, true))
	return wrap("adding the IPVS service "+s.Key(), err)
}

// UpdateService changes a service's scheduler.
func (c *Conn) UpdateService(s Service) error {
	_, err := c.g.Do(cmdSetService, false, serviceAttrs(s, true))
	return wrap("changing the IPVS service "+s.Key(), err)
}

// DelService removes a virtual service, and its destinations.
func (c *Conn) DelService(s Service) error {
	_, err := c.g.Do(cmdDelService, false, serviceAttrs(s, false))
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return wrap("removing the IPVS service "+s.Key(), err)
}

// AddDest adds a destination to a service, by direct routing.
func (c *Conn) AddDest(s Service, d Dest) error {
	_, err := c.g.Do(cmdNewDest, false, append(serviceAttrs(s, false), destAttrs(d)...))
	return wrap(fmt.Sprintf("adding %s to the IPVS service %s", netip.AddrPortFrom(d.Addr, d.Port), s.Key()), err)
}

// UpdateDest changes a destination's weight.
func (c *Conn) UpdateDest(s Service, d Dest) error {
	_, err := c.g.Do(cmdSetDest, false, append(serviceAttrs(s, false), destAttrs(d)...))
	return wrap(fmt.Sprintf("changing %s in the IPVS service %s", netip.AddrPortFrom(d.Addr, d.Port), s.Key()), err)
}

// DelDest removes a destination from a service.
func (c *Conn) DelDest(s Service, d Dest) error {
	_, err := c.g.Do(cmdDelDest, false, append(serviceAttrs(s, false), destAttrs(d)...))
	if errors.Is(err, syscall.ENOENT) {
		return nil
	}
	return wrap(fmt.Sprintf("removing %s from the IPVS service %s", netip.AddrPortFrom(d.Addr, d.Port), s.Key()), err)
}

// Services lists every virtual service of this namespace.
func (c *Conn) Services() ([]Service, error) {
	msgs, err := c.g.Do(cmdGetService, true, nil)
	if err != nil {
		return nil, wrap("listing IPVS services", err)
	}
	var out []Service
	for _, m := range msgs {
		for _, a := range m {
			if a.Type&^(1<<15) != attrService {
				continue
			}
			var s Service
			var fam uint16
			var raw []byte
			ok := true
			for _, f := range netlink.ParseAttrs(a.Data) {
				switch f.Type {
				case svcAF:
					fam = native.Uint16(f.Data)
				case svcProtocol:
					s.Protocol = native.Uint16(f.Data)
				case svcAddr:
					raw = f.Data
				case svcPort:
					s.Port = binary.BigEndian.Uint16(f.Data)
				case svcSchedName:
					s.Scheduler = cstring(f.Data)
				case 5: // a firewall-mark service: not theLB's kind
					ok = false
				}
			}
			if !ok || len(raw) < 4 {
				continue
			}
			if fam == syscall.AF_INET {
				s.Addr = netip.AddrFrom4([4]byte(raw[:4]))
			} else if len(raw) >= 16 {
				s.Addr = netip.AddrFrom16([16]byte(raw[:16]))
			}
			out = append(out, s)
		}
	}
	return out, nil
}

// Dests lists a service's destinations, with their counters.
func (c *Conn) Dests(s Service) ([]Dest, error) {
	msgs, err := c.g.Do(cmdGetDest, true, serviceAttrs(s, false))
	if err != nil {
		return nil, wrap("listing the destinations of "+s.Key(), err)
	}
	var out []Dest
	for _, m := range msgs {
		for _, a := range m {
			if a.Type&^(1<<15) != attrDest {
				continue
			}
			var d Dest
			var raw []byte
			fam := af(s.Addr)
			for _, f := range netlink.ParseAttrs(a.Data) {
				switch f.Type &^ (1 << 15) {
				case destAddr:
					raw = f.Data
				case destPort:
					d.Port = binary.BigEndian.Uint16(f.Data)
				case destWeight:
					d.Weight = native.Uint32(f.Data)
				case destFwdMethod:
					d.Tunnel = native.Uint32(f.Data)&fwdMask == fwdTunnel
				case destAddrFamily:
					fam = native.Uint16(f.Data)
				case destActive:
					if len(f.Data) >= 4 {
						d.Stats.Active = native.Uint32(f.Data)
					}
				case destInactive:
					if len(f.Data) >= 4 {
						d.Stats.Inactive = native.Uint32(f.Data)
					}
				case destStats64:
					for _, st := range netlink.ParseAttrs(f.Data) {
						if len(st.Data) < 8 {
							continue
						}
						v := native.Uint64(st.Data)
						switch st.Type {
						case statsConns:
							d.Stats.Conns = v
						case statsInPkts:
							d.Stats.Packets = v
						case statsInByte:
							d.Stats.Bytes = v
						}
					}
				}
			}
			if fam == syscall.AF_INET && len(raw) >= 4 {
				d.Addr = netip.AddrFrom4([4]byte(raw[:4]))
			} else if len(raw) >= 16 {
				d.Addr = netip.AddrFrom16([16]byte(raw[:16]))
			}
			out = append(out, d)
		}
	}
	return out, nil
}

func daemonAttrs(d Daemon, full bool) []byte {
	state := uint32(stateBackup)
	if d.Master {
		state = stateMaster
	}
	b := netlink.AppendAttr(nil, daemonState, u32(state))
	if full {
		b = netlink.AppendAttr(b, daemonMcastIfn, append([]byte(d.Interface), 0))
		b = netlink.AppendAttr(b, daemonSyncID, u32(d.SyncID))
	}
	return netlink.AppendNested(nil, attrDaemon, b)
}

// AddDaemon starts a synchronisation daemon. One master and one backup may run in
// a namespace, each once: starting one that runs is an error (EEXIST).
func (c *Conn) AddDaemon(d Daemon) error {
	_, err := c.g.Do(cmdNewDaemon, false, daemonAttrs(d, true))
	return wrap(fmt.Sprintf("starting the IPVS sync %s on %s", d.Role(), d.Interface), err)
}

// DelDaemon stops the master, or the backup; one that is not running is fine.
func (c *Conn) DelDaemon(master bool) error {
	_, err := c.g.Do(cmdDelDaemon, false, daemonAttrs(Daemon{Master: master}, false))
	if errors.Is(err, syscall.ESRCH) || errors.Is(err, syscall.ENOENT) {
		return nil
	}
	return wrap("stopping the IPVS sync "+Daemon{Master: master}.Role(), err)
}

// Daemons lists the synchronisation daemons running in this namespace.
func (c *Conn) Daemons() ([]Daemon, error) {
	msgs, err := c.g.Do(cmdGetDaemon, true, nil)
	if err != nil {
		return nil, wrap("listing the IPVS sync daemons", err)
	}
	var out []Daemon
	for _, m := range msgs {
		for _, a := range m {
			if a.Type&^(1<<15) != attrDaemon {
				continue
			}
			var d Daemon
			for _, f := range netlink.ParseAttrs(a.Data) {
				switch f.Type {
				case daemonState:
					d.Master = native.Uint32(f.Data) == stateMaster
				case daemonMcastIfn:
					d.Interface = cstring(f.Data)
				case daemonSyncID:
					d.SyncID = native.Uint32(f.Data)
				}
			}
			out = append(out, d)
		}
	}
	return out, nil
}

func wrap(what string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", what, err)
}

func cstring(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}
