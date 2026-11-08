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

// Status is a director's view of the cluster.
type Status struct {
	View         string        `json:"view"`
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
}

// Node is a director or a backend.
type Node struct {
	Node               string     `json:"node"`
	Role               string     `json:"role"`
	Address            string     `json:"address"`
	State              string     `json:"state"`
	Version            string     `json:"version"`
	NodeFingerprint    string     `json:"node_fingerprint"`
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

// Offer is what a backend offers to serve.
type Offer struct {
	Node         string  `json:"node"`
	Address      string  `json:"address"`
	Protocol     string  `json:"protocol"`
	Port         int     `json:"port"`
	Check        string  `json:"check"`
	Service      *string `json:"service"`
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

// Tables are the tables, in the order `status` shows them; a table's name picks
// it alone (`thelb status nodes`).
var Tables = []string{"cluster", "nodes", "certificates", "pending", "pools", "offers", "vips", "services", "backends"}

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

// Text renders the tables named in only (all of them when it is empty), with a
// blank line between them. now dates the "ago" columns.
func (s Status) Text(now time.Time, only ...string) (string, error) {
	want := map[string]bool{}
	for _, name := range only {
		if !isTable(name) {
			return "", fmt.Errorf("no table %q: the tables are %s", name, strings.Join(Tables, ", "))
		}
		want[name] = true
	}
	var out []string
	for _, name := range Tables {
		if len(want) == 0 || want[name] {
			out = append(out, s.render(name, now))
		}
	}
	return strings.Join(out, "\n"), nil
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
	case "cluster":
		var rows [][]string
		if c := s.Cluster; c != nil {
			rows = append(rows, []string{c.Name, dash(c.ID), stamp(c.Created), dash(c.Leader), term(c.Term), stamp(c.LeaderSince)})
		}
		return table.Render([]string{"cluster", "id", "created", "leader", "term", "leader since"}, rows, 4)
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
			rows = append(rows, []string{o.Node, o.Address, o.Protocol + "/" + strconv.Itoa(o.Port), o.Check, svc, seconds(o.SinceSeconds)})
		}
		return table.Render([]string{"node", "address", "offer", "check", "service", "since"}, rows)
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
				bytesText(b.Bytes), seconds(b.LastCheckInSeconds) + " ago"})
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
