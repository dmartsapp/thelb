package kube

// The few fields of the API's objects theLB reads.

// Meta is an object's metadata.
type Meta struct {
	Name            string            `json:"name"`
	Namespace       string            `json:"namespace,omitempty"`
	UID             string            `json:"uid,omitempty"`
	ResourceVersion string            `json:"resourceVersion,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
	Annotations     map[string]string `json:"annotations,omitempty"`
}

// Key is namespace/name, or name for an object outside namespaces.
func (m Meta) Key() string {
	if m.Namespace == "" {
		return m.Name
	}
	return m.Namespace + "/" + m.Name
}

// Service is a Service.
type Service struct {
	Meta   Meta          `json:"metadata"`
	Spec   ServiceSpec   `json:"spec"`
	Status ServiceStatus `json:"status"`
}

// ServiceSpec is what theLB reads of a Service's spec.
type ServiceSpec struct {
	Type                          string        `json:"type,omitempty"`
	LoadBalancerClass             *string       `json:"loadBalancerClass,omitempty"`
	ExternalTrafficPolicy         string        `json:"externalTrafficPolicy,omitempty"`
	HealthCheckNodePort           int           `json:"healthCheckNodePort,omitempty"`
	IPFamilies                    []string      `json:"ipFamilies,omitempty"`
	Ports                         []ServicePort `json:"ports,omitempty"`
	AllocateLoadBalancerNodePorts *bool         `json:"allocateLoadBalancerNodePorts,omitempty"`
}

// ServicePort is one of a Service's ports.
type ServicePort struct {
	Name     string `json:"name,omitempty"`
	Protocol string `json:"protocol,omitempty"` // TCP (the default), UDP or SCTP
	Port     int    `json:"port"`
	NodePort int    `json:"nodePort,omitempty"`
}

// ServiceStatus is a Service's status.
type ServiceStatus struct {
	LoadBalancer LoadBalancerStatus `json:"loadBalancer"`
}

// LoadBalancerStatus is where a provider says the addresses it gave.
type LoadBalancerStatus struct {
	Ingress []Ingress `json:"ingress"`
}

// Ingress is one address, with how packets reach it: "VIP", passed through to
// the nodes unchanged.
type Ingress struct {
	IP     string `json:"ip,omitempty"`
	IPMode string `json:"ipMode,omitempty"`
}

// Class is the Service's load-balancer class, or "".
func (s *Service) Class() string {
	if s.Spec.LoadBalancerClass == nil {
		return ""
	}
	return *s.Spec.LoadBalancerClass
}

// Node is a Node.
type Node struct {
	Meta   Meta       `json:"metadata"`
	Spec   NodeSpec   `json:"spec"`
	Status NodeStatus `json:"status"`
}

// NodeSpec is what theLB reads of a Node's spec.
type NodeSpec struct {
	Unschedulable bool `json:"unschedulable,omitempty"`
}

// NodeStatus is what theLB reads of a Node's status.
type NodeStatus struct {
	Addresses  []NodeAddress `json:"addresses,omitempty"`
	Conditions []Condition   `json:"conditions,omitempty"`
}

// NodeAddress is one of a Node's addresses: InternalIP, ExternalIP, Hostname.
type NodeAddress struct {
	Type    string `json:"type"`
	Address string `json:"address"`
}

// Condition is one of a Node's conditions.
type Condition struct {
	Type   string `json:"type"`
	Status string `json:"status"`
}

// Ready is whether the Node's Ready condition is True.
func (n *Node) Ready() bool {
	for _, c := range n.Status.Conditions {
		if c.Type == "Ready" {
			return c.Status == "True"
		}
	}
	return false
}

// EndpointSlice is one slice of a Service's endpoints.
type EndpointSlice struct {
	Meta        Meta       `json:"metadata"`
	AddressType string     `json:"addressType,omitempty"`
	Endpoints   []Endpoint `json:"endpoints"`
}

// ServiceName is the Service the slice belongs to, by its label.
func (e *EndpointSlice) ServiceName() string { return e.Meta.Labels["kubernetes.io/service-name"] }

// Endpoint is one endpoint of a slice.
type Endpoint struct {
	Addresses  []string           `json:"addresses"`
	Conditions EndpointConditions `json:"conditions"`
	NodeName   *string            `json:"nodeName,omitempty"`
}

// EndpointConditions are an endpoint's conditions; a missing one is unknown.
type EndpointConditions struct {
	Ready       *bool `json:"ready,omitempty"`
	Serving     *bool `json:"serving,omitempty"`
	Terminating *bool `json:"terminating,omitempty"`
}

// IsReady is whether the endpoint takes new connections: ready, which the API
// says to read as true when it is missing.
func (e Endpoint) IsReady() bool { return e.Conditions.Ready == nil || *e.Conditions.Ready }

// status is the API's answer to a request that failed, and a watch's error.
type status struct {
	Kind    string `json:"kind"`
	Status  string `json:"status"`
	Message string `json:"message"`
	Reason  string `json:"reason"`
	Code    int    `json:"code"`
}

// list is a list's envelope.
type list[T any] struct {
	Metadata struct {
		ResourceVersion string `json:"resourceVersion"`
		Continue        string `json:"continue"`
	} `json:"metadata"`
	Items []T `json:"items"`
}
