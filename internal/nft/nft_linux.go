//go:build linux

package nft

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/dmartsapp/thelb/internal/netlink"
)

// nf_tables over netlink (include/uapi/linux/netfilter/nf_tables.h). Every
// integer attribute is big-endian.
const (
	netlinkNetfilter = 12
	subsysNFTables   = 10
	msgBatchBegin    = 0x10
	msgBatchEnd      = 0x11

	msgNewTable = 0
	msgDelTable = 2
	msgNewChain = 3
	msgNewRule  = 6

	nfprotoINET = 1
	nfprotoIPv4 = 2
	nfprotoIPv6 = 10

	tableName = 1

	chainTable = 1
	chainName  = 3
	chainHook  = 4
	chainType  = 7
	hookNum    = 1
	hookPrio   = 2

	ruleTable = 1
	ruleChain = 2
	ruleExprs = 4
	listElem  = 1
	exprName  = 1
	exprData  = 2

	metaDreg    = 1
	metaKey     = 2
	metaNFProto = 15
	metaL4Proto = 16

	cmpSreg   = 1
	cmpOp     = 2
	cmpData   = 3
	cmpEq     = 0
	dataValue = 1

	payloadDreg   = 1
	payloadBase   = 2
	payloadOffset = 3
	payloadLen    = 4
	baseNetwork   = 1
	baseTransport = 2

	immDreg = 1
	immData = 2

	natType = 1

	bitwiseSreg = 1
	bitwiseDreg = 2
	bitwiseLen  = 3
	bitwiseMask = 4
	bitwiseXor  = 5
	cmpNeq      = 1
	cmpGt       = 4

	exthdrDreg   = 1
	exthdrType   = 2
	exthdrOffset = 3
	exthdrLen    = 4
	exthdrOp     = 6
	exthdrSreg   = 7
	exthdrTCPOpt = 1 // NFT_EXTHDR_OP_TCPOPT
	tcpOptMaxseg = 2

	hookOutput        = 3
	prioMangle        = -150
	clampChain        = "mss"
	natFamily         = 2
	natRegProtoMin    = 5
	natRegProtoMax    = 6
	natFlags          = 7
	natDNAT           = 1
	natProtoSpecified = 2 // NF_NAT_RANGE_PROTO_SPECIFIED

	reg1 = 1

	hookPrerouting = 0
	prioDstNAT     = -100
)

var seq atomic.Uint32

func init() { seq.Store(uint32(time.Now().UnixNano())) }

func be32(v uint32) []byte { b := make([]byte, 4); binary.BigEndian.PutUint32(b, v); return b }
func be16(v uint16) []byte { b := make([]byte, 2); binary.BigEndian.PutUint16(b, v); return b }
func str(s string) []byte  { return append([]byte(s), 0) }

type message struct {
	typ   uint16
	flags uint16
	fam   byte
	body  []byte
}

func nfgen(fam byte, resID uint16) []byte {
	b := []byte{fam, 0, 0, 0}
	binary.BigEndian.PutUint16(b[2:], resID)
	return b
}

func expr(name string, data []byte) []byte {
	var e []byte
	e = netlink.AppendAttr(e, exprName, str(name))
	e = netlink.AppendNested(e, exprData, data)
	return netlink.AppendNested(nil, listElem, e)
}

func meta(key uint32) []byte {
	var d []byte
	d = netlink.AppendAttr(d, metaKey, be32(key))
	d = netlink.AppendAttr(d, metaDreg, be32(reg1))
	return expr("meta", d)
}

func cmp(value []byte) []byte {
	var d []byte
	d = netlink.AppendAttr(d, cmpSreg, be32(reg1))
	d = netlink.AppendAttr(d, cmpOp, be32(cmpEq))
	d = netlink.AppendNested(d, cmpData, netlink.AppendAttr(nil, dataValue, value))
	return expr("cmp", d)
}

func payload(base, offset, length uint32) []byte {
	var d []byte
	d = netlink.AppendAttr(d, payloadDreg, be32(reg1))
	d = netlink.AppendAttr(d, payloadBase, be32(base))
	d = netlink.AppendAttr(d, payloadOffset, be32(offset))
	d = netlink.AppendAttr(d, payloadLen, be32(length))
	return expr("payload", d)
}

// rule is: meta nfproto <family> <ip|ip6> daddr <vip> meta l4proto <proto>
// th dport <port> dnat to :<app port>.
func rule(m Map) ([]byte, error) {
	proto, err := protoNumber(m.Proto)
	if err != nil {
		return nil, err
	}
	vip := m.VIP.Unmap()
	fam, off, n := byte(nfprotoIPv4), uint32(16), uint32(4)
	if vip.Is6() {
		fam, off, n = nfprotoIPv6, 24, 16
	}
	var e []byte
	e = append(e, meta(metaNFProto)...)
	e = append(e, cmp([]byte{fam})...)
	e = append(e, payload(baseNetwork, off, n)...)
	e = append(e, cmp(vip.AsSlice())...)
	e = append(e, meta(metaL4Proto)...)
	e = append(e, cmp([]byte{proto})...)
	e = append(e, payload(baseTransport, 2, 2)...)
	e = append(e, cmp(be16(m.Port))...)
	var imm []byte
	imm = netlink.AppendAttr(imm, immDreg, be32(reg1))
	imm = netlink.AppendNested(imm, immData, netlink.AppendAttr(nil, dataValue, be16(m.AppPort)))
	e = append(e, expr("immediate", imm)...)
	var nat []byte
	nat = netlink.AppendAttr(nat, natType, be32(natDNAT))
	nat = netlink.AppendAttr(nat, natFamily, be32(uint32(fam)))
	nat = netlink.AppendAttr(nat, natRegProtoMin, be32(reg1))
	nat = netlink.AppendAttr(nat, natRegProtoMax, be32(reg1))
	nat = netlink.AppendAttr(nat, natFlags, be32(natProtoSpecified))
	e = append(e, expr("nat", nat)...)

	var b []byte
	b = netlink.AppendAttr(b, ruleTable, str(Table))
	b = netlink.AppendAttr(b, ruleChain, str(Chain))
	b = netlink.AppendNested(b, ruleExprs, e)
	return b, nil
}

func cmpOpValue(op uint32, value []byte) []byte {
	var d []byte
	d = netlink.AppendAttr(d, cmpSreg, be32(reg1))
	d = netlink.AppendAttr(d, cmpOp, be32(op))
	d = netlink.AppendNested(d, cmpData, netlink.AppendAttr(nil, dataValue, value))
	return expr("cmp", d)
}

func tcpMaxseg(load bool) []byte {
	var d []byte
	if load {
		d = netlink.AppendAttr(d, exthdrDreg, be32(reg1))
	} else {
		d = netlink.AppendAttr(d, exthdrSreg, be32(reg1))
	}
	d = netlink.AppendAttr(d, exthdrType, []byte{tcpOptMaxseg})
	d = netlink.AppendAttr(d, exthdrOffset, be32(2))
	d = netlink.AppendAttr(d, exthdrLen, be32(2))
	d = netlink.AppendAttr(d, exthdrOp, be32(exthdrTCPOpt))
	return expr("exthdr", d)
}

// clampRule is: meta nfproto ipv4 ip saddr <vip> meta l4proto tcp
// tcp flags & syn != 0 tcp option maxseg size > <mss>
// tcp option maxseg size set <mss>.
func clampRule(c Clamp) []byte {
	vip := c.VIP.Unmap()
	var e []byte
	e = append(e, meta(metaNFProto)...)
	e = append(e, cmp([]byte{nfprotoIPv4})...)
	e = append(e, payload(baseNetwork, 12, 4)...)
	e = append(e, cmp(vip.AsSlice())...)
	e = append(e, meta(metaL4Proto)...)
	e = append(e, cmp([]byte{6})...)
	e = append(e, payload(baseTransport, 13, 1)...)
	var bw []byte
	bw = netlink.AppendAttr(bw, bitwiseSreg, be32(reg1))
	bw = netlink.AppendAttr(bw, bitwiseDreg, be32(reg1))
	bw = netlink.AppendAttr(bw, bitwiseLen, be32(1))
	bw = netlink.AppendNested(bw, bitwiseMask, netlink.AppendAttr(nil, dataValue, []byte{0x02}))
	bw = netlink.AppendNested(bw, bitwiseXor, netlink.AppendAttr(nil, dataValue, []byte{0x00}))
	e = append(e, expr("bitwise", bw)...)
	e = append(e, cmpOpValue(cmpNeq, []byte{0})...)
	e = append(e, tcpMaxseg(true)...)
	e = append(e, cmpOpValue(cmpGt, be16(c.MSS))...)
	var imm []byte
	imm = netlink.AppendAttr(imm, immDreg, be32(reg1))
	imm = netlink.AppendNested(imm, immData, netlink.AppendAttr(nil, dataValue, be16(c.MSS)))
	e = append(e, expr("immediate", imm)...)
	e = append(e, tcpMaxseg(false)...)
	var b []byte
	b = netlink.AppendAttr(b, ruleTable, str(Table))
	b = netlink.AppendAttr(b, ruleChain, str(clampChain))
	b = netlink.AppendNested(b, ruleExprs, e)
	return b
}

func chain(name, typ string, hook uint32, prio int32) message {
	var h []byte
	h = netlink.AppendAttr(h, hookNum, be32(hook))
	h = netlink.AppendAttr(h, hookPrio, be32(uint32(prio)))
	var c []byte
	c = netlink.AppendAttr(c, chainTable, str(Table))
	c = netlink.AppendAttr(c, chainName, str(name))
	c = netlink.AppendNested(c, chainHook, h)
	c = netlink.AppendAttr(c, chainType, str(typ))
	return message{typ: msgNewChain, flags: syscall.NLM_F_CREATE, fam: nfprotoINET, body: c}
}

func table() []byte { return netlink.AppendAttr(nil, tableName, str(Table)) }

// Apply makes theLB's table hold exactly maps and clamps, in one transaction:
// the table is replaced whole - created, emptied and filled - or, with
// neither, removed.
func Apply(maps []Map, clamps ...Clamp) error {
	msgs := []message{
		// Adding the table first makes deleting it succeed whether or not it was there.
		{typ: msgNewTable, flags: syscall.NLM_F_CREATE, fam: nfprotoINET, body: table()},
		{typ: msgDelTable, fam: nfprotoINET, body: table()},
	}
	if len(maps) > 0 || len(clamps) > 0 {
		msgs = append(msgs, message{typ: msgNewTable, flags: syscall.NLM_F_CREATE, fam: nfprotoINET, body: table()})
	}
	if len(clamps) > 0 {
		msgs = append(msgs, chain(clampChain, "filter", hookOutput, prioMangle))
		for _, c := range clamps {
			if !c.VIP.Unmap().Is4() {
				return fmt.Errorf("clamping the MSS is IPv4 only in this build: %s", c.VIP)
			}
			msgs = append(msgs, message{typ: msgNewRule, flags: syscall.NLM_F_CREATE | syscall.NLM_F_APPEND, fam: nfprotoINET, body: clampRule(c)})
		}
	}
	if len(maps) > 0 {
		msgs = append(msgs, chain(Chain, "nat", hookPrerouting, prioDstNAT))
		for _, m := range maps {
			r, err := rule(m)
			if err != nil {
				return err
			}
			msgs = append(msgs, message{typ: msgNewRule, flags: syscall.NLM_F_CREATE | syscall.NLM_F_APPEND, fam: nfprotoINET, body: r})
		}
	}
	return batch(msgs)
}

// Remove removes theLB's table, if it is there.
func Remove() error { return Apply(nil) }

// Exists is whether theLB's table is there.
func Exists() (bool, error) {
	fd, err := open()
	if err != nil {
		return false, err
	}
	defer func() { _ = syscall.Close(fd) }()
	s := seq.Add(1)
	b := header(subsysNFTables<<8|1 /* NFT_MSG_GETTABLE */, syscall.NLM_F_REQUEST|syscall.NLM_F_ACK, s, nfgen(nfprotoINET, 0), table())
	if err := syscall.Sendto(fd, b, 0, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return false, err
	}
	found := false
	for {
		buf := make([]byte, 1<<16)
		n, _, err := syscall.Recvfrom(fd, buf, 0)
		if err != nil {
			return false, err
		}
		msgs, err := syscall.ParseNetlinkMessage(buf[:n])
		if err != nil {
			return false, err
		}
		for _, m := range msgs {
			if m.Header.Seq != s {
				continue
			}
			if m.Header.Type == syscall.NLMSG_ERROR {
				e := int32(binary.NativeEndian.Uint32(m.Data[0:4]))
				if e == 0 {
					return found, nil
				}
				if syscall.Errno(-e) == syscall.ENOENT {
					return false, nil
				}
				return false, syscall.Errno(-e)
			}
			found = true
		}
	}
}

// Rules is how many rules theLB's table holds, in all its chains: 0 when the
// table is not there.
func Rules() (int, error) {
	fd, err := open()
	if err != nil {
		return 0, err
	}
	defer func() { _ = syscall.Close(fd) }()
	s := seq.Add(1)
	// The rule's table attribute is the table's name attribute's number: a
	// dump of the rules of theLB's table only.
	b := header(subsysNFTables<<8|7 /* NFT_MSG_GETRULE */, syscall.NLM_F_REQUEST|syscall.NLM_F_DUMP, s, nfgen(nfprotoINET, 0), table())
	if err := syscall.Sendto(fd, b, 0, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return 0, err
	}
	n := 0
	for {
		buf := make([]byte, 1<<16)
		got, _, err := syscall.Recvfrom(fd, buf, 0)
		if err != nil {
			return 0, err
		}
		msgs, err := syscall.ParseNetlinkMessage(buf[:got])
		if err != nil {
			return 0, err
		}
		for _, m := range msgs {
			if m.Header.Seq != s {
				continue
			}
			switch m.Header.Type {
			case syscall.NLMSG_DONE:
				return n, nil
			case syscall.NLMSG_ERROR:
				e := int32(binary.NativeEndian.Uint32(m.Data[0:4]))
				if e == 0 || syscall.Errno(-e) == syscall.ENOENT {
					return n, nil
				}
				return 0, syscall.Errno(-e)
			case subsysNFTables<<8 | msgNewRule:
				n++
			}
		}
	}
}

func open() (int, error) {
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, netlinkNetfilter)
	if err != nil {
		return -1, fmt.Errorf("nftables: %w", err)
	}
	if err := syscall.Bind(fd, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		_ = syscall.Close(fd)
		return -1, fmt.Errorf("nftables: %w", err)
	}
	tv := syscall.Timeval{Sec: 5}
	_ = syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv)
	return fd, nil
}

func header(typ, flags uint16, s uint32, parts ...[]byte) []byte {
	var body []byte
	for _, p := range parts {
		body = append(body, p...)
	}
	b := make([]byte, syscall.NLMSG_HDRLEN, syscall.NLMSG_HDRLEN+len(body))
	b = append(b, body...)
	binary.NativeEndian.PutUint32(b[0:4], uint32(len(b)))
	binary.NativeEndian.PutUint16(b[4:6], typ)
	binary.NativeEndian.PutUint16(b[6:8], flags)
	binary.NativeEndian.PutUint32(b[8:12], s)
	for len(b)%4 != 0 {
		b = append(b, 0)
	}
	return b
}

// batch sends msgs as one transaction, and waits for each to be acknowledged.
func batch(msgs []message) error {
	fd, err := open()
	if err != nil {
		return err
	}
	defer func() { _ = syscall.Close(fd) }()
	var b []byte
	b = append(b, header(msgBatchBegin, syscall.NLM_F_REQUEST, seq.Add(1), nfgen(syscall.AF_UNSPEC, subsysNFTables))...)
	want := map[uint32]bool{}
	for _, m := range msgs {
		s := seq.Add(1)
		want[s] = true
		b = append(b, header(subsysNFTables<<8|m.typ, syscall.NLM_F_REQUEST|syscall.NLM_F_ACK|m.flags, s, nfgen(m.fam, 0), m.body)...)
	}
	b = append(b, header(msgBatchEnd, syscall.NLM_F_REQUEST, seq.Add(1), nfgen(syscall.AF_UNSPEC, subsysNFTables))...)
	if err := syscall.Sendto(fd, b, 0, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return fmt.Errorf("nftables: %w", err)
	}
	var errs []error
	for len(want) > 0 {
		buf := make([]byte, 1<<16)
		n, _, err := syscall.Recvfrom(fd, buf, 0)
		if err != nil {
			return fmt.Errorf("nftables: %w", err)
		}
		replies, err := syscall.ParseNetlinkMessage(buf[:n])
		if err != nil {
			return fmt.Errorf("nftables: %w", err)
		}
		for _, r := range replies {
			if r.Header.Type != syscall.NLMSG_ERROR || !want[r.Header.Seq] {
				continue
			}
			delete(want, r.Header.Seq)
			if e := int32(binary.NativeEndian.Uint32(r.Data[0:4])); e != 0 {
				errs = append(errs, syscall.Errno(-e))
				// The transaction was refused as a whole: no more answers come.
				return fmt.Errorf("nftables: %w", errors.Join(errs...))
			}
		}
	}
	return nil
}

func protoNumber(p string) (byte, error) {
	switch strings.ToLower(p) {
	case "tcp":
		return 6, nil
	case "udp":
		return 17, nil
	case "sctp":
		return 132, nil
	}
	return 0, fmt.Errorf("%q is not tcp, udp or sctp", p)
}
