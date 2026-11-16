package kube

import (
	"fmt"
	"regexp"
)

// What the API server sends is checked before theLB keeps it: the names,
// ports and words it reads go into Raft, IPVS, status, logs and events. An API
// server validates its objects; one that is broken, or not what it claims,
// could send anything. An object that fails is left out, as if it did not
// exist, and the director says so.

var (
	dns1035Label     = regexp.MustCompile(`^[a-z]([-a-z0-9]{0,61}[a-z0-9])?$`)
	dns1123Label     = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	dns1123Subdomain = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
	uidRE            = regexp.MustCompile(`^[0-9A-Za-z._-]{0,128}$`)
)

// SubdomainOK is whether s is a DNS subdomain name, as Kubernetes names nodes
// and most objects: up to 253 characters.
func SubdomainOK(s string) bool { return len(s) <= 253 && dns1123Subdomain.MatchString(s) }

// Checked is an object that can say what is wrong with it.
type Checked interface {
	Problem() string
}

func (m Meta) problem(namespaced bool, name *regexp.Regexp) string {
	switch {
	case !name.MatchString(m.Name) || len(m.Name) > 253:
		return "its name is not a Kubernetes name"
	case namespaced && !dns1123Label.MatchString(m.Namespace):
		return "its namespace is not a Kubernetes namespace"
	case !namespaced && m.Namespace != "":
		return "it has a namespace, and none should"
	case !uidRE.MatchString(m.UID):
		return "its uid is not one"
	}
	return ""
}

// Problem is what is wrong with the Service, or "".
func (s *Service) Problem() string {
	if p := s.Meta.problem(true, dns1035Label); p != "" {
		return p
	}
	sp := s.Spec
	switch {
	case len(sp.Ports) > 1000:
		return "it has more than 1000 ports"
	case sp.HealthCheckNodePort < 0 || sp.HealthCheckNodePort > 65535:
		return "its healthCheckNodePort is not a port"
	case sp.ExternalTrafficPolicy != "" && sp.ExternalTrafficPolicy != "Local" && sp.ExternalTrafficPolicy != "Cluster":
		return "its externalTrafficPolicy is neither Local nor Cluster"
	case len(sp.IPFamilies) > 2:
		return "it has more than two IP families"
	}
	for _, f := range sp.IPFamilies {
		if f != "IPv4" && f != "IPv6" {
			return "an IP family of it is neither IPv4 nor IPv6"
		}
	}
	for _, p := range sp.Ports {
		switch {
		case p.Port < 1 || p.Port > 65535:
			return fmt.Sprintf("its port %d is not a port", p.Port)
		case p.NodePort < 0 || p.NodePort > 65535:
			return fmt.Sprintf("its nodePort %d is not a port", p.NodePort)
		case p.Protocol != "" && p.Protocol != "TCP" && p.Protocol != "UDP" && p.Protocol != "SCTP":
			return "a port's protocol is not TCP, UDP or SCTP"
		case p.Name != "" && !dns1123Label.MatchString(p.Name):
			return "a port's name is not a Kubernetes name"
		}
	}
	return ""
}

// Problem is what is wrong with the Node, or "".
func (n *Node) Problem() string {
	if !SubdomainOK(n.Meta.Name) {
		return "its name is not a Kubernetes node's name"
	}
	if p := n.Meta.problem(false, dns1123Subdomain); p != "" {
		return p
	}
	if len(n.Status.Addresses) > 100 {
		return "it has more than 100 addresses"
	}
	return ""
}

// Problem is what is wrong with the EndpointSlice, or "".
func (e *EndpointSlice) Problem() string {
	if p := e.Meta.problem(true, dns1123Subdomain); p != "" {
		return p
	}
	switch {
	case e.ServiceName() != "" && !dns1035Label.MatchString(e.ServiceName()):
		return "its service-name label is not a Service's name"
	case e.AddressType != "IPv4" && e.AddressType != "IPv6" && e.AddressType != "FQDN":
		return "its addressType is not IPv4, IPv6 or FQDN"
	case len(e.Endpoints) > 1000:
		return "it has more than 1000 endpoints"
	}
	for _, ep := range e.Endpoints {
		if ep.NodeName != nil && !SubdomainOK(*ep.NodeName) {
			return "an endpoint's nodeName is not a node's name"
		}
	}
	return ""
}
