package cli

import (
	"bufio"
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/dmartsapp/thelb/internal/director"
	"github.com/dmartsapp/thelb/internal/kube"
	"github.com/dmartsapp/thelb/internal/safe"
)

// fromCluster is how thelb generate k8s may read the cluster, to fit what it
// prints to it: with the user's own kubeconfig and context, read-only, and
// only once the user says yes.
type fromCluster struct {
	kubeconfig string // "": read nothing
	context    string // "": the kubeconfig's current context
	yes        bool   // read without asking
}

// takeFromFlags takes generate k8s's own flags out of args - --from-kubeconfig,
// --from-context and --yes - and leaves the form's.
func takeFromFlags(args []string) (fromCluster, []string, error) {
	var f fromCluster
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, val, hasVal := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if !strings.HasPrefix(a, "-") {
			rest = append(rest, a)
			continue
		}
		switch name {
		case "from-kubeconfig", "from-context":
			if !hasVal {
				if i+1 >= len(args) {
					return f, nil, usagef("thelb generate k8s: --%s needs a value", name)
				}
				i++
				val = args[i]
			}
			if name == "from-kubeconfig" {
				f.kubeconfig = val
			} else {
				f.context = val
			}
		case "yes":
			if hasVal {
				return f, nil, usagef("thelb generate k8s: --yes takes no value")
			}
			f.yes = true
		default:
			rest = append(rest, a)
		}
	}
	switch {
	case f.kubeconfig == "" && f.context != "":
		return f, nil, usagef("thelb generate k8s: --from-context names a context of --from-kubeconfig: give that too")
	case f.kubeconfig == "" && f.yes:
		return f, nil, usagef("thelb generate k8s: --yes says to read the cluster without asking: give --from-kubeconfig too")
	}
	return f, rest, nil
}

// What the cluster says goes into files people apply and scripts they run, so
// it is checked first: a broken or hostile API server must not be able to add
// a line to them. Text only shown is cleaned of control characters.
var (
	k8sVersionRE    = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+[0-9A-Za-z.+_-]{0,64}$`)
	k8sTokenRE      = regexp.MustCompile(`^[A-Za-z0-9._~+/=-]+$`)
	k8sLabelKeyRE   = regexp.MustCompile(`^([a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?/)?[A-Za-z0-9]([A-Za-z0-9._-]{0,61}[A-Za-z0-9])?$`)
	k8sLabelValueRE = regexp.MustCompile(`^([A-Za-z0-9]([A-Za-z0-9._-]{0,61}[A-Za-z0-9])?)?$`)
)

// clean is text from the cluster, fit to show: control characters - a
// newline, a terminal's escape - each a "?", and at most 200 characters.
func clean(s string) string { return safe.Short(s, 200) }

// yamlString is s as a YAML string, quoted: JSON's quoting, which YAML reads.
func yamlString(s string) string { b, _ := json.Marshal(s); return string(b) }

// clusterView is the cluster, read with the user's credential.
type clusterView struct {
	c       *kube.Client
	cfg     *kube.Config
	version string // the API server's own words: v1.36.4
}

// open asks the user to let theLB read the cluster - saying with what, and
// what it reads - and connects. Nothing is read before the yes; without a
// terminal, only --yes says it.
func (f fromCluster) open(ctx context.Context, cmd string, reads []string, stderr io.Writer) (*clusterView, error) {
	cfg, err := kube.LoadContext(f.kubeconfig, f.context)
	if err != nil {
		var ae *kube.AuthError
		if errors.As(err, &ae) {
			return nil, fmt.Errorf("%s: the context's user %s signs in with %s, which theLB does not do - leave out --from-kubeconfig: then generate prints what works with kubectl", cmd, ae.User, ae.Method)
		}
		return nil, fmt.Errorf("%s: --from-kubeconfig: %w", cmd, err)
	}
	_, _ = fmt.Fprintf(stderr, "%s reads the Kubernetes cluster, to fit what it prints to it - read-only, with this:\n", cmd)
	_, _ = fmt.Fprintf(stderr, "  kubeconfig   %s\n  context      %s\n  API server   %s\n  user         %s\n", f.kubeconfig, cfg.Context, cfg.Server, cfg.User)
	_, _ = fmt.Fprintf(stderr, "  it reads     %s\n", strings.Join(reads, "; "))
	if !f.yes {
		if !isTerminal() {
			return nil, fmt.Errorf("%s: reading the cluster needs your yes: answer at a terminal, or give --yes", cmd)
		}
		_, _ = fmt.Fprint(stderr, "Read the cluster? [y/N] ")
		line, _ := bufio.NewReader(stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			return nil, fmt.Errorf("%s: the cluster was not read, and nothing was printed", cmd)
		}
	}
	v := &clusterView{c: kube.New(cfg), cfg: cfg}
	minor, git, err := v.c.Version(ctx)
	if err != nil {
		return nil, fmt.Errorf("%s: the API server %s: %w", cmd, cfg.Server, err)
	}
	if minor < kube.MinMinor {
		return nil, fmt.Errorf("%s: the cluster runs Kubernetes %s: theLB serves 1.%d and later", cmd, git, kube.MinMinor)
	}
	v.version = git
	if !k8sVersionRE.MatchString(git) {
		v.version = "of a version it names oddly"
	}
	return v, nil
}

// get reads the object at path into out; found is false for the API's 404.
func (v *clusterView) get(ctx context.Context, path string, query url.Values, out any) (found bool, err error) {
	err = v.c.Get(ctx, path, query, out)
	if kube.IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

// hostPort is the API server's host and port, as the kubeconfig names them.
func (v *clusterView) hostPort() (string, int) {
	port := 443
	if p, err := strconv.Atoi(v.cfg.Server.Port()); err == nil {
		port = p
	}
	return v.cfg.Server.Hostname(), port
}

// fitted is the comment line that says what output was fitted to.
func (v *clusterView) fitted() string {
	return fmt.Sprintf("# Fitted to the cluster at %s, Kubernetes %s.\n", clean(v.cfg.Server.String()), clean(v.version))
}

// kubeProxy is whether kube-proxy runs in the cluster - as kubeadm installs it.
func (v *clusterView) kubeProxy(ctx context.Context) (bool, error) {
	var ds struct{}
	return v.get(ctx, "/apis/apps/v1/namespaces/kube-system/daemonsets/kube-proxy", nil, &ds)
}

// kubeconfigFromCluster prints the kubeconfig of directors outside the
// cluster: the API server the user's context names, the CA and the token of
// the Secret thelb-director-token - and checks that the token has every
// permission theLB needs, through that very kubeconfig.
func kubeconfigFromCluster(ctx context.Context, v *clusterView, version string, stdout, stderr io.Writer) error {
	const cmd = "thelb generate k8s kubeconfig"
	var sec struct {
		Data map[string]string `json:"data"`
	}
	found, err := v.get(ctx, "/api/v1/namespaces/thelb/secrets/thelb-director-token", nil, &sec)
	if err != nil {
		return fmt.Errorf("%s: reading the Secret thelb/thelb-director-token: %w", cmd, err)
	}
	if !found {
		return fmt.Errorf("%s: the cluster has no Secret thelb/thelb-director-token: apply what thelb generate k8s prints first", cmd)
	}
	token, err := base64.StdEncoding.DecodeString(sec.Data["token"])
	if err != nil || len(token) == 0 {
		return fmt.Errorf("%s: the Secret thelb/thelb-director-token has no token yet - Kubernetes adds it a moment after the Secret is applied: try again", cmd)
	}
	if len(token) > 16384 || !k8sTokenRE.Match(token) {
		return fmt.Errorf("%s: the Secret thelb/thelb-director-token holds something that is not a token", cmd)
	}
	caPEM, err := base64.StdEncoding.DecodeString(sec.Data["ca.crt"])
	if err != nil || len(caPEM) == 0 {
		return fmt.Errorf("%s: the Secret thelb/thelb-director-token has no ca.crt it can read", cmd)
	}
	// The kubeconfig, checked as a director would use it.
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return fmt.Errorf("%s: the Secret thelb/thelb-director-token's ca.crt holds no certificate", cmd)
	}
	theirs := kube.New(&kube.Config{Server: v.cfg.Server, RootCAs: pool, ServerName: v.cfg.ServerName, Token: string(token)})
	var lacks []string
	for _, a := range director.KubeAccess() {
		ok, err := theirs.Allowed(ctx, a)
		if err != nil {
			return fmt.Errorf("%s: the kubeconfig does not reach the API server %s with the Secret's CA and token: %w", cmd, v.cfg.Server, err)
		}
		if !ok {
			lacks = append(lacks, a.String())
		}
	}
	if len(lacks) > 0 {
		_, _ = fmt.Fprintf(stderr, "warning: the token lacks permissions theLB needs: %s - apply what thelb generate k8s prints again\n", strings.Join(lacks, ", "))
	} else {
		_, _ = fmt.Fprintln(stderr, "checked: with this kubeconfig, a director reaches the API server and has every permission it needs")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# theLB %s, as `thelb generate k8s kubeconfig` printed it from the cluster: the\n", version)
	b.WriteString("# kubeconfig of directors outside the cluster, or under systemd on its nodes -\n")
	b.WriteString("# with the token of the ServiceAccount thelb-director. On each director:\n")
	b.WriteString("#   sudo install -D -m 600 thelb.kubeconfig /etc/thelb/kubeconfig\n")
	b.WriteString("# and start it with --kubeconfig /etc/thelb/kubeconfig.\n")
	b.WriteString(v.fitted())
	b.WriteString("apiVersion: v1\nkind: Config\nclusters:\n  - name: cluster\n    cluster:\n")
	fmt.Fprintf(&b, "      server: %s\n", yamlString(v.cfg.Server.String()))
	fmt.Fprintf(&b, "      certificate-authority-data: %s\n", base64.StdEncoding.EncodeToString(caPEM))
	if v.cfg.ServerName != "" {
		fmt.Fprintf(&b, "      tls-server-name: %s\n", yamlString(v.cfg.ServerName))
	}
	b.WriteString("users:\n  - name: thelb-director\n    user:\n")
	fmt.Fprintf(&b, "      token: %s\n", yamlString(string(token)))
	b.WriteString("contexts:\n  - name: thelb\n    context:\n      cluster: cluster\n      user: thelb-director\ncurrent-context: thelb\n")
	_, _ = io.WriteString(stdout, b.String())
	return nil
}

// nodeFromCluster reads what the node's script needs to know of the cluster.
func nodeFromCluster(ctx context.Context, v *clusterView, node string, stderr io.Writer) (nodeFacts, error) {
	const cmd = "thelb generate k8s node"
	f := nodeFacts{read: true, server: v.cfg.Server.String(), version: v.version}
	var kn kube.Node
	found, err := v.get(ctx, "/api/v1/nodes/"+url.PathEscape(node), nil, &kn)
	if err != nil {
		return f, fmt.Errorf("%s: reading the node %s: %w", cmd, node, err)
	}
	if !found {
		nodes, _, err := kube.List[kube.Node](ctx, v.c, "/api/v1/nodes")
		if err != nil {
			return f, fmt.Errorf("%s: the cluster has no node %s", cmd, node)
		}
		var names []string
		for _, n := range nodes {
			names = append(names, clean(n.Meta.Name))
		}
		sort.Strings(names)
		return f, fmt.Errorf("%s: the cluster has no node %s - its nodes: %s", cmd, node, strings.Join(names, ", "))
	}
	_, f.controlPlane = kn.Meta.Labels["node-role.kubernetes.io/control-plane"]
	var cn struct{}
	if f.cilium, err = v.get(ctx, "/apis/cilium.io/v2/ciliumnodes/"+url.PathEscape(node), nil, &cn); err != nil {
		return f, fmt.Errorf("%s: asking whether Cilium runs on %s: %w", cmd, node, err)
	}
	if f.kubeProxy, err = v.kubeProxy(ctx); err != nil {
		return f, fmt.Errorf("%s: asking whether kube-proxy runs: %w", cmd, err)
	}
	if f.cilium {
		services, _, err := kube.List[kube.Service](ctx, v.c, "/api/v1/services")
		if err != nil {
			return f, fmt.Errorf("%s: listing the Services: %w", cmd, err)
		}
		for _, s := range services {
			if s.Class() == "dmarts.app/thelb" && s.Meta.Annotations["service.cilium.io/node-selector"] != "!dmarts.app/thelb-director" {
				_, _ = fmt.Fprintf(stderr, "warning: the Service %s lacks the annotation service.cilium.io/node-selector: \"!dmarts.app/thelb-director\" - Cilium on %s would take its packets before theLB\n", clean(s.Meta.Key()), node)
			}
		}
	}
	return f, nil
}

// directorFromCluster reads what the DaemonSet needs to know of the cluster:
// with kube-proxy, the API server's own address for its pods - which reach no
// Service, the API server's included - and says which nodes it runs on.
func directorFromCluster(ctx context.Context, v *clusterView, stderr io.Writer) (env string, err error) {
	const cmd = "thelb generate k8s director"
	nodes, _, err := kube.List[kube.Node](ctx, v.c, "/api/v1/nodes")
	if err != nil {
		return "", fmt.Errorf("%s: listing the nodes: %w", cmd, err)
	}
	var labelled []string
	for _, n := range nodes {
		if _, ok := n.Meta.Labels["dmarts.app/thelb-director"]; ok {
			labelled = append(labelled, clean(n.Meta.Name))
		}
	}
	sort.Strings(labelled)
	if len(labelled) == 0 {
		_, _ = fmt.Fprintln(stderr, "note: no node is labelled dmarts.app/thelb-director yet, so the DaemonSet runs no director until one is: thelb generate k8s node <node> prints the script that prepares one")
	} else {
		_, _ = fmt.Fprintf(stderr, "note: the DaemonSet runs a director on %s\n", strings.Join(labelled, ", "))
	}
	proxy, err := v.kubeProxy(ctx)
	if err != nil {
		return "", fmt.Errorf("%s: asking whether kube-proxy runs: %w", cmd, err)
	}
	if !proxy {
		return "", nil
	}
	host, port := v.hostPort()
	return fmt.Sprintf(`          env:
            # kube-proxy runs in this cluster, and not on a director's node: a pod
            # there reaches no Service - the API server's own included - so the
            # director asks the API server at the address your kubeconfig names.
            - name: KUBERNETES_SERVICE_HOST
              value: %s
            - name: KUBERNETES_SERVICE_PORT
              value: "%d"
`, yamlString(host), port), nil
}

// serviceFit is what a Service takes from the cluster: the selector and ports
// of the workload of its name, unless the flags give them.
type serviceFit struct {
	from     string // "the Deployment default/web", or ""
	selector []label
	ports    []svcPort
}

// serviceFromCluster reads the Service's namespace, the Deployment or
// StatefulSet of its name there, its pods, and a Service of the same name.
func serviceFromCluster(ctx context.Context, v *clusterView, name, ns string, haveSelector, havePorts bool, selector []label, stderr io.Writer) (serviceFit, error) {
	const cmd = "thelb generate k8s service"
	var fit serviceFit
	var nsObj struct{}
	found, err := v.get(ctx, "/api/v1/namespaces/"+url.PathEscape(ns), nil, &nsObj)
	if err != nil {
		return fit, fmt.Errorf("%s: reading the namespace %s: %w", cmd, ns, err)
	}
	if !found {
		return fit, fmt.Errorf("%s: the cluster has no namespace %s", cmd, ns)
	}
	var workload struct {
		Spec struct {
			Selector struct {
				MatchLabels map[string]string `json:"matchLabels"`
			} `json:"selector"`
			Template struct {
				Spec struct {
					Containers []struct {
						Ports []struct {
							ContainerPort int    `json:"containerPort"`
							Protocol      string `json:"protocol"`
						} `json:"ports"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	for _, kind := range []struct{ path, name string }{{"deployments", "Deployment"}, {"statefulsets", "StatefulSet"}} {
		found, err := v.get(ctx, "/apis/apps/v1/namespaces/"+url.PathEscape(ns)+"/"+kind.path+"/"+url.PathEscape(name), nil, &workload)
		if err != nil {
			return fit, fmt.Errorf("%s: reading the %s %s/%s: %w", cmd, kind.name, ns, name, err)
		}
		if found {
			fit.from = "the " + kind.name + " " + ns + "/" + name
			break
		}
	}
	if fit.from != "" {
		var took []string
		if !haveSelector && len(workload.Spec.Selector.MatchLabels) > 0 {
			for k, val := range workload.Spec.Selector.MatchLabels {
				if !k8sLabelKeyRE.MatchString(k) || !k8sLabelValueRE.MatchString(val) {
					return fit, fmt.Errorf("%s: %s selects its pods by a label theLB does not write: %s=%s - give --selector", cmd, fit.from, clean(k), clean(val))
				}
				fit.selector = append(fit.selector, label{k, val})
			}
			sort.Slice(fit.selector, func(i, j int) bool { return fit.selector[i].key < fit.selector[j].key })
			selector = fit.selector
			took = append(took, "the selector "+labelsString(fit.selector))
		}
		if !havePorts {
			seen := map[string]bool{}
			for _, c := range workload.Spec.Template.Spec.Containers {
				for _, p := range c.Ports {
					proto := strings.ToLower(p.Protocol)
					if proto == "" {
						proto = "tcp"
					}
					if (proto != "tcp" && proto != "udp" && proto != "sctp") || p.ContainerPort > 65535 {
						return fit, fmt.Errorf("%s: %s has a port theLB does not write: %s/%d - give --port", cmd, fit.from, clean(p.Protocol), p.ContainerPort)
					}
					k := fmt.Sprintf("%s/%d", proto, p.ContainerPort)
					if p.ContainerPort > 0 && !seen[k] {
						seen[k] = true
						fit.ports = append(fit.ports, svcPort{proto: proto, port: p.ContainerPort, pods: p.ContainerPort})
					}
				}
			}
			if len(fit.ports) > 0 {
				var ps []string
				for _, p := range fit.ports {
					ps = append(ps, fmt.Sprintf("%s/%d", p.proto, p.port))
				}
				took = append(took, "the ports "+strings.Join(ps, ", "))
			}
		}
		if len(took) > 0 {
			_, _ = fmt.Fprintf(stderr, "from %s: %s\n", fit.from, strings.Join(took, "; "))
		}
	} else if !haveSelector {
		_, _ = fmt.Fprintf(stderr, "note: no Deployment or StatefulSet %s/%s: the selector is %s - give --selector if the pods are labelled otherwise\n", ns, name, labelsString(selector))
	}
	var pods struct {
		Items []struct{} `json:"items"`
	}
	if _, err := v.get(ctx, "/api/v1/namespaces/"+url.PathEscape(ns)+"/pods", url.Values{"labelSelector": {labelsSelector(selector)}}, &pods); err != nil {
		return fit, fmt.Errorf("%s: listing the pods: %w", cmd, err)
	}
	if len(pods.Items) == 0 {
		_, _ = fmt.Fprintf(stderr, "warning: no pod in %s has the labels %s: the Service would have no endpoints\n", ns, labelsString(selector))
	}
	var existing kube.Service
	found, err = v.get(ctx, "/api/v1/namespaces/"+url.PathEscape(ns)+"/services/"+url.PathEscape(name), nil, &existing)
	if err != nil {
		return fit, fmt.Errorf("%s: reading the Service %s/%s: %w", cmd, ns, name, err)
	}
	if found {
		switch class := existing.Class(); {
		case existing.Spec.Type == "LoadBalancer" && class != "dmarts.app/thelb":
			if class == "" {
				class = "none: the cluster's default load balancer"
			}
			class = clean(class)
			return fit, fmt.Errorf("%s: the Service %s/%s exists, of type LoadBalancer and class %s - a class Kubernetes does not let change: delete it first (kubectl -n %s delete service %s), or name this one otherwise", cmd, ns, name, class, ns, name)
		default:
			_, _ = fmt.Fprintf(stderr, "note: the Service %s/%s exists: kubectl apply changes it to this\n", ns, name)
		}
	}
	return fit, nil
}
