package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"regexp"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dmartsapp/thelb/internal/version"
)

// kubeconfigScript builds, where kubectl reaches the cluster, the kubeconfig of
// directors outside it - from the thelb-director ServiceAccount's token, which
// only the cluster knows. %s: the version.
const kubeconfigScript = `#!/bin/sh
# theLB %s, as ` + "`thelb generate k8s kubeconfig`" + ` printed it. Run it where kubectl
# reaches the cluster, after applying what ` + "`thelb generate k8s`" + ` prints: it prints
# the kubeconfig of directors outside the cluster, or under systemd on its
# nodes - with the token of the ServiceAccount thelb-director.
#   sh thelb-kubeconfig.sh > thelb.kubeconfig
# Then, on each director:
#   sudo install -D -m 600 thelb.kubeconfig /etc/thelb/kubeconfig
# and start it with --kubeconfig /etc/thelb/kubeconfig.
set -eu
server=$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')
ca=$(kubectl -n thelb get secret thelb-director-token -o jsonpath='{.data.ca\.crt}')
token=$(kubectl -n thelb get secret thelb-director-token -o jsonpath='{.data.token}' | base64 -d)
if [ -z "$token" ]; then
	echo "the Secret thelb/thelb-director-token has no token yet: apply what thelb generate k8s prints, then try again" >&2
	exit 1
fi
cat <<EOF
apiVersion: v1
kind: Config
clusters:
  - name: cluster
    cluster:
      server: $server
      certificate-authority-data: $ca
users:
  - name: thelb-director
    user:
      token: $token
contexts:
  - name: thelb
    context:
      cluster: cluster
      user: thelb-director
current-context: thelb
EOF
`

var (
	k8sNameRE      = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)
	k8sNamespaceRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	k8sLabelRE     = regexp.MustCompile(`^([a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?/)?[A-Za-z0-9]([A-Za-z0-9._-]{0,61}[A-Za-z0-9])?=([A-Za-z0-9]([A-Za-z0-9._-]{0,61}[A-Za-z0-9])?)?$`)
	k8sPortRE      = regexp.MustCompile(`^(\d+)(/(tcp|udp|sctp))?(:(\d+))?$`)
)

// label is one label of a selector.
type label struct{ key, val string }

// labelsString is a selector as people write it: app=web, tier=front.
func labelsString(ls []label) string {
	var out []string
	for _, l := range ls {
		out = append(out, l.key+"="+l.val)
	}
	return strings.Join(out, ", ")
}

// labelsSelector is a selector as the API takes it: app=web,tier=front.
func labelsSelector(ls []label) string { return strings.ReplaceAll(labelsString(ls), ", ", ",") }

// svcPort is one port of a Service: its protocol, its number, the pods'.
type svcPort struct {
	proto      string
	port, pods int
}

// serviceFlags are generate k8s service's flags.
type serviceFlags struct {
	ns, selector, pool, ip, share, policy string
	ports                                 []string
}

// k8sServiceCmd is generate k8s service: a Service of theLB's class.
func (a *app) k8sServiceCmd(from *fromCluster) *cobra.Command {
	var f serviceFlags
	c := &cobra.Command{
		Use:   "service <name> [flags]",
		Short: "A Service of class dmarts.app/thelb",
		Long: `Print a Service of type LoadBalancer and class dmarts.app/thelb, with the
annotation that keeps Cilium off directors' nodes.

With --from-kubeconfig, it takes the selector and ports from the Deployment or
StatefulSet of the same name.`,
		Example: `  thelb generate k8s service web --port 80:8080 > web.yaml
  kubectl apply -f web.yaml`,
		Annotations: map[string]string{annDocs: docsURL + "docs/kubernetes.html"},
		Args:        nArgs(1, "the Service's name: service <name> [flags]"),
	}
	fs := c.Flags()
	fs.StringVar(&f.ns, "namespace", "default", "Its `namespace`")
	fs.StringArrayVar(&f.ports, "port", nil, "A `port[/proto][:pods-port]`, as often as needed: 80, 53/udp, 80:8080, 443/tcp:8443 (default 80)")
	fs.StringVar(&f.selector, "selector", "", "The pods it sends to, as one label `key=value` (default app=<name>)")
	fs.StringVar(&f.policy, "policy", "Local", "Its externalTrafficPolicy, `Local|Cluster`: Local sends only to nodes with a ready pod, and keeps the client's address")
	fs.StringVar(&f.pool, "pool", "", "Its VIP from this `pool`, not the default")
	fs.StringVar(&f.ip, "ip", "", "This `address` of the pool")
	fs.StringVar(&f.share, "shared-vip", "", "One VIP for the Services with this `key`")
	_ = c.RegisterFlagCompletionFunc("policy", cobra.FixedCompletions([]cobra.Completion{"Local", "Cluster"}, cobra.ShellCompDirectiveNoFileComp))
	c.RunE = a.act(func(_ *cobra.Command, args []string) error {
		return generateK8sService(a.ctx, args[0], f, a.stdout, a.stderr, *from)
	})
	return c
}

// generateK8sService prints a Service of theLB's class, for the user to apply:
// its name, namespace, ports, the pods it selects, and theLB's annotations.
// With from, it fits them to the cluster first.
func generateK8sService(ctx context.Context, name string, f serviceFlags, stdout, stderr io.Writer, from fromCluster) error {
	const cmd = "thelb generate k8s service"
	ns, selector, pool, ip, share, policy := &f.ns, &f.selector, &f.pool, &f.ip, &f.share, &f.policy
	ports := f.ports
	switch {
	case !k8sNameRE.MatchString(name):
		return usagef("%s: a Service's name is lowercase letters, digits and hyphens, beginning with a letter, up to 63: %q", cmd, name)
	case !k8sNamespaceRE.MatchString(*ns):
		return usagef("%s: --namespace %q is not a namespace's name", cmd, *ns)
	case *policy != "Local" && *policy != "Cluster":
		return usagef("%s: --policy is Local or Cluster, not %q", cmd, *policy)
	case *pool != "" && !nameRE.MatchString(*pool):
		return usagef("%s: --pool %q is not a pool's name", cmd, *pool)
	case *share != "" && !k8sNamespaceRE.MatchString(*share):
		return usagef("%s: --shared-vip is lowercase letters, digits and hyphens: %q", cmd, *share)
	}
	haveSelector := *selector != ""
	if !haveSelector {
		*selector = "app=" + name
	}
	if !k8sLabelRE.MatchString(*selector) {
		return usagef("%s: --selector is one label, key=value: %q", cmd, *selector)
	}
	key, val, _ := strings.Cut(*selector, "=")
	sel := []label{{key, val}}
	if *ip != "" {
		a, err := netip.ParseAddr(*ip)
		if err != nil {
			return usagef("%s: --ip %q is not an address", cmd, *ip)
		}
		*ip = a.Unmap().String()
	}
	havePorts := len(ports) > 0
	if !havePorts {
		ports = []string{"80"}
	}
	var ps []svcPort
	seen := map[string]bool{}
	for _, p := range ports {
		m := k8sPortRE.FindStringSubmatch(p)
		if m == nil {
			return usagef("%s: --port is a port, its protocol, and the pods' port if it differs: 80, 53/udp, 80:8080 or 443/tcp:8443 - not %q", cmd, p)
		}
		proto := m[3]
		if proto == "" {
			proto = "tcp"
		}
		n, _ := strconv.Atoi(m[1])
		pods := n
		if m[5] != "" {
			pods, _ = strconv.Atoi(m[5])
		}
		if n < 1 || n > 65535 || pods < 1 || pods > 65535 {
			return usagef("%s: --port %q: ports are 1 to 65535", cmd, p)
		}
		k := fmt.Sprintf("%s/%d", proto, n)
		if seen[k] {
			return usagef("%s: --port %s twice", cmd, k)
		}
		seen[k] = true
		ps = append(ps, svcPort{proto: proto, port: n, pods: pods})
	}
	var v *clusterView
	var fit serviceFit
	if from.kubeconfig != "" {
		var err error
		reads := []string{"the API server's version", "the namespace " + *ns, "a Deployment or StatefulSet " + name + " there, for its selector and ports", "the pods the selector picks", "a Service " + name + " there"}
		if v, err = from.open(ctx, cmd+" "+name, reads, stderr); err != nil {
			return err
		}
		if fit, err = serviceFromCluster(ctx, v, name, *ns, haveSelector, havePorts, sel, stderr); err != nil {
			return err
		}
		if len(fit.selector) > 0 {
			sel = fit.selector
		}
		if len(fit.ports) > 0 {
			ps = fit.ports
		}
	}
	q := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	w("# theLB %s, as `thelb generate k8s service` printed it: a Service of theLB's", version.Version)
	w("# class. Apply it: kubectl apply -f <this file>")
	if v != nil {
		b.WriteString(v.fitted())
		if fit.from != "" {
			w("# The pods it selects, and their ports, are those of %s.", fit.from)
		}
	}
	w("apiVersion: v1")
	w("kind: Service")
	w("metadata:")
	w("  name: %s", name)
	w("  namespace: %s", *ns)
	w("  annotations:")
	w("    # With directors on nodes of the cluster, and Cilium: Cilium leaves this")
	w("    # Service to the directors there. Harmless anywhere else.")
	w(`    service.cilium.io/node-selector: "!dmarts.app/thelb-director"`)
	for _, a := range []struct{ key, val, what, about string }{
		{"dmarts.app/thelb-pool", *pool, "pool", "a pool other than the default"},
		{"dmarts.app/thelb-ip", *ip, "address", "a particular address of the pool"},
		{"dmarts.app/thelb-shared-vip", *share, "key", "one VIP for the Services that give the same key, their ports apart"},
	} {
		if a.val != "" {
			w("    # %s:", strings.ToUpper(a.about[:1])+a.about[1:])
			w("    %s: %s", a.key, q(a.val))
		} else {
			w("    # %s: <%s>   %s", a.key, a.what, a.about)
		}
	}
	w("spec:")
	w("  type: LoadBalancer")
	w("  loadBalancerClass: dmarts.app/thelb")
	if *policy == "Local" {
		w("  # Local: only the nodes with a ready pod of the Service get its traffic,")
		w("  # weighted by their pods, and the pods see each client's own address.")
	} else {
		w("  # Cluster: every ready node gets traffic, and passes it on to a pod on any")
		w("  # node - which sees a node's address, not the client's.")
	}
	w("  externalTrafficPolicy: %s", *policy)
	w("  selector:")
	for _, l := range sel {
		w("    %s: %s", l.key, q(l.val))
	}
	w("  ports:")
	for _, p := range ps {
		w("    - name: %s-%d", p.proto, p.port)
		w("      protocol: %s", strings.ToUpper(p.proto))
		w("      port: %d", p.port)
		w("      targetPort: %d", p.pods)
	}
	_, _ = fmt.Fprint(stdout, b.String())
	return nil
}
