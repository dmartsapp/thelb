// Package status is what `thelb status` shows: the cluster as one director sees
// it, as tables in the style of a SQL client's results, or as JSON. It holds no
// secret material - no secret, no key - by construction: none of its types has
// a field for one.
package status

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/dmartsapp/thelb/internal/table"
)

// Status is a director's view of the cluster - or, on a backend, Mine alone.
type Status struct {
	View         string        `json:"view"`
	Mine         *Mine         `json:"mine,omitempty"`
	Cluster      *Cluster      `json:"cluster"`
	Nodes        []Node        `json:"nodes"`
	Certificates []Certificate `json:"certificates"`
	Pending      []Pending     `json:"pending"`
	Pools        []Pool        `json:"pools"`
	Offers       []Offer       `json:"offers"`
	VIPs         []VIP         `json:"vips"`
	Services     []Service     `json:"services"`
	Backends     []Backend     `json:"backends"`
}

// Cluster is the cluster's own row. Term is nil until the cluster elects.
type Cluster struct {
	Name        string    `json:"name"`
	ID          string    `json:"id"`
	Created     time.Time `json:"created"`
	Leader      string    `json:"leader"`
	Term        *uint64   `json:"term"`
	LeaderSince time.Time `json:"leader_since"`
	Lease       int       `json:"vip_lease_seconds,omitempty"` // the control VIP's lease
}

// Node is a director or a backend.
type Node struct {
	Node               string     `json:"node"`
	Role               string     `json:"role"`
	Address            string     `json:"address"`
	State              string     `json:"state"`
	Version            string     `json:"version"`
	NodeFingerprint    string     `json:"node_fingerprint"`
	MAC                string     `json:"mac,omitempty"`
	LastCheckIn        *time.Time `json:"last_check_in"`
	LastCheckInSeconds *float64   `json:"last_check_in_seconds"`
}

// Certificate is a certificate the cluster holds: the CA's, and each node's.
type Certificate struct {
	Certificate string    `json:"certificate"`
	Kind        string    `json:"kind"`
	Fingerprint string    `json:"fingerprint"`
	NotAfter    time.Time `json:"not_after"`
}

// Pending is a node waiting for approval.
type Pending struct {
	Pending         string  `json:"pending"`
	Role            string  `json:"role"`
	Address         string  `json:"address"`
	MAC             string  `json:"mac"`
	NodeFingerprint string  `json:"node_fingerprint"`
	WaitingSeconds  float64 `json:"waiting_seconds"`
}

// Pool is an address pool.
type Pool struct {
	Pool      string `json:"pool"`
	Network   string `json:"network"`
	First     string `json:"first"`
	Count     int    `json:"count"`
	Interface string `json:"interface"`
	Used      int    `json:"used"`
}

// Offer is what a backend offers to serve: an application, by the name the
// backend gave it, assigned to a service or not yet.
type Offer struct {
	Node         string  `json:"node"`
	Offer        string  `json:"offer"`
	Address      string  `json:"address"`
	Protocol     string  `json:"protocol"`
	Port         int     `json:"port"`
	Check        string  `json:"check"`
	OwnCheck     string  `json:"own_check"` // the backend's own, as its last heartbeat said: ok, fail or "-"
	Service      *string `json:"service"`   // nil: unassigned
	SinceSeconds float64 `json:"since_seconds"`
}

// VIP is a VIP, and who holds it.
type VIP struct {
	VIP       string    `json:"vip"`
	Purpose   string    `json:"purpose"`
	Pool      string    `json:"pool"`
	Interface string    `json:"interface"`
	HeldBy    string    `json:"held_by"`
	Since     time.Time `json:"since"`
	Port      int       `json:"port,omitempty"` // the control VIP's: the control port
}

// Service is a service.
type Service struct {
	Service    string `json:"service"`
	VIP        string `json:"vip"`
	Protocol   string `json:"protocol"`
	Port       int    `json:"port"`
	Scheduler  string `json:"scheduler"`
	Method     string `json:"method"`
	BackendsUp int    `json:"backends_up"`
	Backends   int    `json:"backends"`
}

// Backend is a backend serving a service.
type Backend struct {
	Service            string  `json:"service"`
	VIP                string  `json:"vip"`
	Port               int     `json:"port"`
	Backend            string  `json:"backend"`
	Address            string  `json:"address"`
	AppPort            int     `json:"app_port"`
	OwnCheck           string  `json:"own_check"`
	DirectorCheck      string  `json:"director_check"`
	Weight             int     `json:"weight"`
	Connections        uint64  `json:"connections"`
	Packets            uint64  `json:"packets"`
	Bytes              uint64  `json:"bytes"`
	LastCheckInSeconds float64 `json:"last_check_in_seconds"`
}

// Mine is a backend's view of itself: what it is, the cluster's name, its
// offers with the service each is assigned to - never the service's VIP - and
// its node fingerprint.
type Mine struct {
	Role            string      `json:"role"`
	Cluster         string      `json:"cluster"`
	State           string      `json:"state"`
	Offers          []MineOffer `json:"offers"`
	NodeFingerprint string      `json:"node_fingerprint"`
}

// MineOffer is one of a backend's offers.
type MineOffer struct {
	Offer   string `json:"offer"` // its name
	Port    string `json:"port"`  // proto/port
	Check   string `json:"check"`
	Own     string `json:"own_check"`
	Offered string `json:"offered"` // yes: the cluster holds it; or why not yet
	Service string `json:"service"`
}

// Tables are the tables, in the order `status all` shows them.
var Tables = []string{"cluster", "nodes", "certificates", "pending", "pools", "offers", "vips", "services", "backends"}

// Views are what `thelb status` shows by name: a part of the cluster, or all
// of it. Without a name, it shows the summary. Any table's name shows that
// table alone - except vips and backends, which are views holding those
// tables and the ones that go with them.
var Views = map[string][]string{
	"summary":   {"cluster", "summary"},
	"directors": {"cluster", "nodes:director", "pending:director", "certificates:director"},
	"backends":  {"nodes:backend", "pending:backend", "offers", "backends", "certificates:backend"},
	"vips":      {"vips", "services", "pools"},
	"all":       Tables,
}

// Names are every name status takes, for its help and its errors.
var Names = []string{"directors", "backends", "vips", "all", "cluster", "nodes", "certificates", "pending", "pools", "offers", "services"}

// Normalize turns missing tables into empty ones, so JSON always has a list.
func (s *Status) Normalize() {
	if s.Nodes == nil {
		s.Nodes = []Node{}
	}
	if s.Certificates == nil {
		s.Certificates = []Certificate{}
	}
	if s.Pending == nil {
		s.Pending = []Pending{}
	}
	if s.Pools == nil {
		s.Pools = []Pool{}
	}
	if s.Offers == nil {
		s.Offers = []Offer{}
	}
	if s.VIPs == nil {
		s.VIPs = []VIP{}
	}
	if s.Services == nil {
		s.Services = []Service{}
	}
	if s.Backends == nil {
		s.Backends = []Backend{}
	}
}

// parts are the tables a selection shows, each with its filter: "nodes:backend"
// is the nodes table with backends only.
func parts(only []string) ([]string, error) {
	if len(only) == 0 {
		only = []string{"summary"}
	}
	var out []string
	seen := map[string]bool{}
	for _, name := range only {
		ps, ok := Views[name]
		if !ok {
			if !isTable(name) {
				return nil, fmt.Errorf("no view or table %q: name one of %s", name, strings.Join(Names, ", "))
			}
			ps = []string{name}
		}
		for _, p := range ps {
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out, nil
}

// Counters is whether a selection shows IPVS's counters - only the backends
// table does: a status without them is quicker to make, as the leader reads
// them service by service. JSON without a name is everything.
func Counters(asJSON bool, only ...string) bool {
	if asJSON && len(only) == 0 {
		return true
	}
	ps, err := parts(only)
	if err != nil {
		return true
	}
	for _, p := range ps {
		if strings.SplitN(p, ":", 2)[0] == "backends" {
			return true
		}
	}
	return false
}

// Text renders a selection - views, or tables, by name; the summary when none is
// named - with a blank line between tables. now dates the "ago" columns.
func (s Status) Text(now time.Time, only ...string) (string, error) {
	if s.Mine != nil {
		return s.Mine.text(), nil
	}
	ps, err := parts(only)
	if err != nil {
		return "", err
	}
	var out []string
	for _, p := range ps {
		out = append(out, s.filter(p).render(strings.SplitN(p, ":", 2)[0], now))
	}
	text := strings.Join(out, "\n")
	if len(only) == 0 {
		text += "\nMore: thelb status directors | backends | vips | all\n"
	}
	return text, nil
}

// Pick is a selection as JSON: one key per table.
func (s Status) Pick(only ...string) (any, error) {
	if s.Mine != nil {
		return s.Mine, nil
	}
	if len(only) == 0 {
		only = []string{"all"}
	}
	ps, err := parts(only)
	if err != nil {
		return nil, err
	}
	s.Normalize()
	full := map[string]any{
		"view": s.View, "cluster": s.Cluster, "summary": s.summary(), "nodes": s.Nodes, "certificates": s.Certificates, "pending": s.Pending,
		"pools": s.Pools, "offers": s.Offers, "vips": s.VIPs, "services": s.Services, "backends": s.Backends,
	}
	out := map[string]any{"view": s.View}
	for _, p := range ps {
		name := strings.SplitN(p, ":", 2)[0]
		f := s.filter(p)
		f.Normalize()
		switch name {
		case "nodes":
			out[name] = f.Nodes
		case "pending":
			out[name] = f.Pending
		case "certificates":
			out[name] = f.Certificates
		default:
			out[name] = full[name]
		}
	}
	return out, nil
}

// filter is the status with a part's filter applied: a role, for nodes, pending
// nodes and certificates.
func (s Status) filter(part string) Status {
	name, role, ok := strings.Cut(part, ":")
	if !ok {
		return s
	}
	f := s
	switch name {
	case "nodes":
		f.Nodes = nil
		for _, n := range s.Nodes {
			if n.Role == role || (role == "director" && n.Role == "leader") {
				f.Nodes = append(f.Nodes, n)
			}
		}
	case "pending":
		f.Pending = nil
		for _, p := range s.Pending {
			if p.Role == role {
				f.Pending = append(f.Pending, p)
			}
		}
	case "certificates":
		f.Certificates = nil
		for _, c := range s.Certificates {
			if c.Kind == role || (role == "director" && c.Kind == "ca") {
				f.Certificates = append(f.Certificates, c)
			}
		}
	}
	return f
}

// SummaryRow is one line of the summary.
type SummaryRow struct {
	What    string `json:"what"`
	Total   int    `json:"total"`
	Up      int    `json:"up"`
	Down    int    `json:"down"`
	Pending *int   `json:"pending"`
}

func (s Status) summary() []SummaryRow {
	rows := []SummaryRow{{What: "directors"}, {What: "backends"}, {What: "services"}, {What: "vips"}}
	for i := range 2 {
		zero := 0
		rows[i].Pending = &zero
	}
	for _, n := range s.Nodes {
		r := &rows[0]
		if n.Role == "backend" {
			r = &rows[1]
		}
		r.Total++
		if n.State == "up" {
			r.Up++
		} else {
			r.Down++
		}
	}
	for _, p := range s.Pending {
		if p.Role == "backend" {
			*rows[1].Pending++
		} else {
			*rows[0].Pending++
		}
	}
	for _, v := range s.Services {
		rows[2].Total++
		if v.BackendsUp > 0 {
			rows[2].Up++
		} else {
			rows[2].Down++
		}
	}
	for _, v := range s.VIPs {
		rows[3].Total++
		if v.HeldBy != "" && v.HeldBy != "-" {
			rows[3].Up++
		} else {
			rows[3].Down++
		}
	}
	return rows
}

func (m *Mine) text() string {
	headers := []string{"role", "cluster", "state", "offer", "port", "check", "own check", "offered", "service", "node fingerprint"}
	var rows [][]string
	for _, o := range m.Offers {
		rows = append(rows, []string{m.Role, dash(m.Cluster), m.State, o.Offer, o.Port, o.Check, o.Own, dash(o.Offered), o.Service, m.NodeFingerprint})
	}
	if len(rows) == 0 {
		rows = append(rows, []string{m.Role, dash(m.Cluster), m.State, "-", "-", "-", "-", "-", "-", m.NodeFingerprint})
	}
	return table.Render(headers, rows)
}

func isTable(name string) bool {
	for _, t := range Tables {
		if t == name {
			return true
		}
	}
	return false
}

func (s Status) render(name string, now time.Time) string {
	switch name {
	case "summary":
		var rows [][]string
		for _, r := range s.summary() {
			pending := "-"
			if r.Pending != nil {
				pending = strconv.Itoa(*r.Pending)
			}
			rows = append(rows, []string{r.What, strconv.Itoa(r.Total), strconv.Itoa(r.Up), strconv.Itoa(r.Down), pending})
		}
		return table.Render([]string{"what", "total", "up", "down", "pending"}, rows, 1, 2, 3, 4)
	case "cluster":
		var rows [][]string
		if c := s.Cluster; c != nil {
			lease := "-"
			if c.Lease > 0 {
				lease = strconv.Itoa(c.Lease) + "s"
			}
			rows = append(rows, []string{c.Name, dash(c.ID), stamp(c.Created), dash(c.Leader), term(c.Term), stamp(c.LeaderSince), lease})
		}
		return table.Render([]string{"cluster", "id", "created", "leader", "term", "leader since", "vip lease"}, rows, 4, 6)
	case "nodes":
		rows := make([][]string, 0, len(s.Nodes))
		for _, n := range s.Nodes {
			rows = append(rows, []string{n.Node, n.Role, n.Address, n.State, n.Version, n.NodeFingerprint, ago(n.LastCheckIn, now)})
		}
		return table.Render([]string{"node", "role", "address", "state", "version", "node fingerprint", "last check-in"}, rows)
	case "certificates":
		rows := make([][]string, 0, len(s.Certificates))
		for _, c := range s.Certificates {
			rows = append(rows, []string{c.Certificate, c.Kind, short(c.Fingerprint), stamp(c.NotAfter)})
		}
		return table.Render([]string{"certificate", "kind", "fingerprint", "not after"}, rows)
	case "pending":
		rows := make([][]string, 0, len(s.Pending))
		for _, p := range s.Pending {
			rows = append(rows, []string{p.Pending, p.Role, p.Address, p.MAC, p.NodeFingerprint, seconds(p.WaitingSeconds)})
		}
		return table.Render([]string{"pending", "role", "address", "mac", "node fingerprint", "waiting"}, rows)
	case "pools":
		rows := make([][]string, 0, len(s.Pools))
		for _, p := range s.Pools {
			rows = append(rows, []string{p.Pool, p.Network, span(p.First, p.Count), p.Interface, fmt.Sprintf("%d of %d", p.Used, p.Count)})
		}
		return table.Render([]string{"pool", "network", "addresses", "interface", "used"}, rows)
	case "offers":
		rows := make([][]string, 0, len(s.Offers))
		for _, o := range s.Offers {
			svc := "-"
			if o.Service != nil {
				svc = *o.Service
			}
			rows = append(rows, []string{o.Node, dash(o.Offer), o.Address, o.Protocol + "/" + strconv.Itoa(o.Port), o.Check, dash(o.OwnCheck), svc, seconds(o.SinceSeconds)})
		}
		return table.Render([]string{"node", "offer", "address", "port", "check", "own check", "service", "since"}, rows)
	case "vips":
		rows := make([][]string, 0, len(s.VIPs))
		for _, v := range s.VIPs {
			rows = append(rows, []string{v.VIP, v.Purpose, dash(v.Pool), v.Interface, v.HeldBy, stamp(v.Since)})
		}
		return table.Render([]string{"vip", "purpose", "pool", "interface", "held by", "since"}, rows)
	case "services":
		rows := make([][]string, 0, len(s.Services))
		for _, v := range s.Services {
			rows = append(rows, []string{v.Service, v.VIP, v.Protocol + "/" + strconv.Itoa(v.Port), v.Scheduler, v.Method, fmt.Sprintf("%d of %d", v.BackendsUp, v.Backends)})
		}
		return table.Render([]string{"service", "vip", "port", "scheduler", "method", "backends up"}, rows)
	default: // backends
		rows := make([][]string, 0, len(s.Backends))
		for _, b := range s.Backends {
			rows = append(rows, []string{b.Service, b.VIP + ":" + strconv.Itoa(b.Port), b.Backend, b.Address + ":" + strconv.Itoa(b.AppPort),
				b.OwnCheck + " / " + b.DirectorCheck, strconv.Itoa(b.Weight), strconv.FormatUint(b.Connections, 10), strconv.FormatUint(b.Packets, 10),
				bytesText(b.Bytes), agoSeconds(b.LastCheckInSeconds)})
		}
		return table.Render([]string{"service", "vip", "backend", "at", "checks", "weight", "connections", "packets", "bytes", "last check-in"}, rows, 5, 6, 7)
	}
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format("2006-01-02 15:04:05")
}

func term(t *uint64) string {
	if t == nil {
		return "-"
	}
	return strconv.FormatUint(*t, 10)
}

func ago(t *time.Time, now time.Time) string {
	if t == nil {
		return "-"
	}
	return seconds(now.Sub(*t).Seconds()) + " ago"
}

func seconds(s float64) string {
	switch {
	case s < 60:
		return strconv.FormatFloat(s, 'f', 1, 64) + " s"
	case s < 3600:
		return strconv.Itoa(int(s/60)) + " min"
	default:
		return strconv.Itoa(int(s/3600)) + " h"
	}
}

func agoSeconds(s float64) string {
	if s < 0 {
		return "-"
	}
	return seconds(s) + " ago"
}

func short(fp string) string {
	if len(fp) > 16 {
		return fp[:16]
	}
	return fp
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func bytesText(n uint64) string {
	const unit = 1000
	if n < unit {
		return strconv.FormatUint(n, 10) + " B"
	}
	v, i := float64(n), 0
	for v >= unit && i < 4 {
		v /= unit
		i++
	}
	return strconv.FormatFloat(v, 'f', 1, 64) + " " + []string{"B", "kB", "MB", "GB", "TB"}[i]
}

// span is a pool's addresses: "10.10.1.20 - 10.10.1.29".
func span(first string, count int) string {
	a, err := netip.ParseAddr(first)
	if err != nil || count < 1 {
		return first
	}
	last := a
	for i := 1; i < count; i++ {
		last = last.Next()
	}
	return a.String() + " - " + last.String()
}
