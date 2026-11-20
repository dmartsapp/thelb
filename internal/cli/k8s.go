package cli

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/dmartsapp/thelb/internal/director"
	"github.com/dmartsapp/thelb/internal/kube"
	"github.com/dmartsapp/thelb/internal/version"
)

// k8sIdentity is what every director serving Kubernetes needs in the cluster:
// its identity, the least permissions - reading, and a Service's status - and
// a token for directors outside the cluster or under systemd.
const k8sIdentity = `apiVersion: v1
kind: Namespace
metadata:
  name: thelb
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: thelb-director
  namespace: thelb
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: thelb-director
rules:
  - apiGroups: [""]
    resources: ["services", "nodes"]
    verbs: ["get", "list", "watch"]
  - apiGroups: [""]
    resources: ["services/status"]
    verbs: ["update", "patch"]
  - apiGroups: ["discovery.k8s.io"]
    resources: ["endpointslices"]
    verbs: ["get", "list", "watch"]
  - apiGroups: ["events.k8s.io"]
    resources: ["events"]
    verbs: ["create", "patch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: thelb-director
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: thelb-director
subjects:
  - kind: ServiceAccount
    name: thelb-director
    namespace: thelb
---
apiVersion: v1
kind: Secret
metadata:
  name: thelb-director-token
  namespace: thelb
  annotations:
    kubernetes.io/service-account.name: thelb-director
type: kubernetes.io/service-account-token
`

// image is the container image of this build.
func image() string {
	return "ghcr.io/dmartsapp/thelb:" + strings.TrimPrefix(version.Version, "v")
}

// k8sCmd is generate k8s: alone, the directors' identity in the cluster; its
// forms print the rest. Every form may read the cluster first, to fit what it
// prints to it: --from-kubeconfig, --from-context and --yes are the flags of
// generate k8s, and every form under it takes them.
func (a *app) k8sCmd() *cobra.Command {
	var from fromCluster
	c := &cobra.Command{
		Use:   "k8s [flags]",
		Short: "The Kubernetes manifests and scripts theLB needs",
		Long: `Print what theLB needs in a Kubernetes cluster. It writes nothing - to disk or
to the cluster: apply it with kubectl, or run it. The nodes that run the
Services' pods need nothing; only a node that runs a director is prepared.

Alone, it prints the directors' identity: namespace, ServiceAccount, RBAC and
token Secret. Its commands print the rest.`,
		Example: `  thelb generate k8s > thelb-k8s.yaml && kubectl apply -f thelb-k8s.yaml
  thelb generate k8s service web --port 80:8080 > web.yaml`,
		Annotations: map[string]string{annDocs: docsURL + "docs/kubernetes.html"},
		Args:        noArgs,
	}
	pf := c.PersistentFlags()
	pf.StringVar(&from.kubeconfig, "from-kubeconfig", "", "Read the cluster with this kubeconfig (a `path`), to fit the output to it - it asks first, and only reads")
	pf.StringVar(&from.context, "from-context", "", "The kubeconfig's `context` (default: its current one)")
	pf.BoolVar(&from.yes, "yes", false, "Read it without asking, for scripts")
	// Both say how to read the cluster: without --from-kubeconfig, there is
	// none to read. Checked before any form runs.
	c.PersistentPreRunE = func(c *cobra.Command, args []string) error {
		if err := c.Root().PersistentPreRunE(c, args); err != nil {
			return err
		}
		switch {
		case from.kubeconfig == "" && from.context != "":
			return usagef("--from-context names a context of --from-kubeconfig: give that too")
		case from.kubeconfig == "" && from.yes:
			return usagef("--yes says to read the cluster without asking: give --from-kubeconfig too")
		}
		return nil
	}
	c.RunE = a.act(func(*cobra.Command, []string) error {
		head := k8sHead()
		if from.kubeconfig != "" {
			v, err := from.open(a.ctx, "thelb generate k8s", []string{"the API server's version"}, a.stderr)
			if err != nil {
				return err
			}
			head += v.fitted()
		}
		_, _ = fmt.Fprint(a.stdout, head+k8sIdentity)
		return nil
	})
	kubeconfig := &cobra.Command{
		Use:   "kubeconfig [--from-kubeconfig <path> [--from-context <name>] [--yes]]",
		Short: "A script that prints the kubeconfig of directors outside the cluster",
		Long: `Print a script that prints the kubeconfig of directors outside the cluster:
kubectl's API server, and the token of the Secret thelb-director-token. Run it
where kubectl reaches the cluster, once the identity is applied.

With --from-kubeconfig, it prints the kubeconfig itself, and checks that the
token has every permission a director needs.

Then give the directors --kubeconfig /etc/thelb/kubeconfig.`,
		Example: `  thelb generate k8s kubeconfig > thelb-kubeconfig.sh
  sh thelb-kubeconfig.sh > thelb.kubeconfig
  sudo install -D -m 600 thelb.kubeconfig /etc/thelb/kubeconfig`,
		Args: noArgs,
		RunE: a.act(func(*cobra.Command, []string) error {
			if from.kubeconfig != "" {
				v, err := from.open(a.ctx, "thelb generate k8s kubeconfig", []string{"the API server's version",
					"the Secret thelb/thelb-director-token - its token and CA go into the kubeconfig", "what that token may do, as a director would ask"}, a.stderr)
				if err != nil {
					return err
				}
				return kubeconfigFromCluster(a.ctx, v, version.Version, a.stdout, a.stderr)
			}
			_, _ = fmt.Fprintf(a.stdout, kubeconfigScript, version.Version)
			return nil
		}),
	}
	node := &cobra.Command{
		Use:   "node <name> [--from-kubeconfig <path> [--from-context <name>] [--yes]]",
		Short: "A script that makes a node a director's node",
		Long: `Print a script that makes a node a director's node, before a director runs
there (in the DaemonSet, or under systemd). It taints and drains a worker (a
control-plane node is left as it is), keeps kube-proxy off the node, labels it
dmarts.app/thelb-director=true, and with Cilium touches theLB's Services so
Cilium leaves them alone there. Its last lines say how to undo it.

Without --from-kubeconfig, the script finds out at run time what the node
needs; with it, it holds only those steps.`,
		Example: "  thelb generate k8s node cp1 > thelb-node.sh && sh thelb-node.sh",
		Args:    nArgs(1, "the node's name: node <name>"),
		RunE: a.act(func(_ *cobra.Command, args []string) error {
			return generateK8sNode(a.ctx, args[0], a.stdout, a.stderr, from)
		}),
	}
	addTo(c, "", kubeconfig, a.k8sDirectorCmd(&from), node, a.k8sServiceCmd(&from))
	return c
}

// k8sHead is the first lines of what generate k8s prints.
func k8sHead() string {
	return fmt.Sprintf("# theLB %s, as `thelb generate k8s` printed it: the directors' identity and\n# permissions in the cluster. Apply it: kubectl apply -f <this file>\n", version.Version)
}

// k8sDirectorCmd is generate k8s director: the identity, and a DaemonSet that
// runs directors on the labelled nodes with the flags given - thelb
// director's, checked as it checks them, and as a pod needs them.
func (a *app) k8sDirectorCmd(from *fromCluster) *cobra.Command {
	var f directorFlags
	c := &cobra.Command{
		Use:   "director --kubeconfig in-cluster --metrics <address:port> [flags]",
		Short: "A DaemonSet that runs directors on labelled nodes",
		Long: `Print the identity, and a DaemonSet that runs a director on each node labelled
dmarts.app/thelb-director, with these flags - the flags of thelb director,
checked as it checks them, and these too:

  --kubeconfig in-cluster     The pod's own ServiceAccount
  --metrics <address:port>    The readiness probe asks /readyz there
  no --name                   Each director is named after its node
  two secret files            Of different names: the two keys of the Secret
                              thelb-secrets, made from a director's files
  --bootstrap                 Only with --bootstrap-wait 31s or more

With --from-kubeconfig, it also reads whether kube-proxy runs, and then points
the pods at the API server's address.`,
		Example: `  thelb generate k8s director --vip 10.0.0.10 --port 7443 \
    --secret-file /etc/thelb/director.secret \
    --backend-secret-file /etc/thelb/backend.secret \
    --kubeconfig in-cluster --metrics 127.0.0.1:9180 > thelb-director.yaml`,
		Annotations: map[string]string{annDocs: docsURL + "docs/kubernetes.html"},
		Args:        noArgs,
	}
	f.add(c)
	c.RunE = a.act(func(c *cobra.Command, _ []string) error {
		cfg, err := f.config(a.ctx, a.debug)
		if err != nil {
			return err
		}
		switch {
		case cfg.Kubeconfig != kube.InCluster:
			return usagef("a director in a pod serves Kubernetes with its pod's ServiceAccount: give --kubeconfig %s", kube.InCluster)
		case c.Flags().Changed("name"):
			return usagef("each director is named after its node: leave out --name")
		case cfg.Metrics == "":
			return usagef("give --metrics 127.0.0.1:<port>: the pod's readiness probe asks it, at /readyz")
		case filepath.Base(cfg.SecretFile) == filepath.Base(cfg.BackendSecretFile):
			return usagef("the two secret files are two keys of one Secret, named after the files: give them different names")
		case cfg.Bootstrap && cfg.BootstrapWait < 31*time.Second:
			return usagef("a crashed pod is restarted after 10 seconds, sooner than the other of two directors stands alone: with --bootstrap, give --bootstrap-wait 31s (the control VIP's lease plus 25 seconds) or more")
		}
		head, env := k8sHead(), ""
		if from.kubeconfig != "" {
			v, err := from.open(a.ctx, "thelb generate k8s director", []string{"the API server's version", "the nodes labelled dmarts.app/thelb-director", "whether kube-proxy runs"}, a.stderr)
			if err != nil {
				return err
			}
			if env, err = directorFromCluster(a.ctx, v, a.stderr); err != nil {
				return err
			}
			head += v.fitted()
		}
		_, _ = fmt.Fprint(a.stdout, head+k8sIdentity+daemonSet(cfg, given(c, a.debug), env))
		return nil
	})
	return c
}

// daemonSet is the DaemonSet that runs directors with cfg - given as flags -
// and env, the environment fitted to the cluster.
func daemonSet(cfg director.Config, flags []string, env string) string {
	q := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	var argLines strings.Builder
	argLines.WriteString("            - director\n")
	for _, f := range flags {
		fmt.Fprintf(&argLines, "            - %s\n", q(f))
	}
	metricsAt := netip.MustParseAddrPort(cfg.Metrics)
	host, port := metricsAt.Addr().String(), metricsAt.Port()
	ds := fmt.Sprintf(`---
# The directors, on the nodes labelled for them. thelb generate k8s node <node>
# prints the script that makes a node one: its label, and the rest.
# Its secret files come from the Secret thelb-secrets - the cluster's two
# secret files, copied from a director:
#   kubectl -n thelb create secret generic thelb-secrets \
#     --from-file=%s=<the directors' secret file> \
#     --from-file=%s=<the backends' secret file>
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: thelb-director
  namespace: thelb
  labels:
    app.kubernetes.io/name: thelb-director
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: thelb-director
  minReadySeconds: 10
  updateStrategy:
    type: RollingUpdate
    rollingUpdate:
      maxUnavailable: 1
  template:
    metadata:
      labels:
        app.kubernetes.io/name: thelb-director
    spec:
      serviceAccountName: thelb-director
      hostNetwork: true
      nodeSelector:
        dmarts.app/thelb-director: "true"
      tolerations:
        - key: node-role.kubernetes.io/control-plane
          operator: Exists
          effect: NoSchedule
        - key: dmarts.app/thelb-director
          operator: Exists
          effect: NoSchedule
      terminationGracePeriodSeconds: 30
      containers:
        - name: director
          image: %s
          args:
%s%s          securityContext:
            # theLB writes /proc/sys/net, which a container runtime mounts
            # read-only otherwise, and may load the kernel's modules.
            privileged: true
          readinessProbe:
            httpGet:
              host: %s
              port: %d
              path: /readyz
            periodSeconds: 2
          volumeMounts:
            # Each secret file mounted as itself (subPath): a Secret's files are
            # symbolic links otherwise, which theLB refuses to read.
            - name: secrets
              mountPath: %s
              subPath: %s
              readOnly: true
            - name: secrets
              mountPath: %s
              subPath: %s
              readOnly: true
            - name: machine-id
              mountPath: /etc/machine-id
              readOnly: true
            - name: modules
              mountPath: /lib/modules
              readOnly: true
            - name: run
              mountPath: /run/thelb
      volumes:
        - name: secrets
          secret:
            secretName: thelb-secrets
            defaultMode: 0400
        # The node's machine ID, for its node fingerprint: a restarted pod
        # rejoins without approval.
        - name: machine-id
          hostPath:
            path: /etc/machine-id
            type: File
        - name: modules
          hostPath:
            path: /lib/modules
            type: Directory
        # thelb status and thelb admin work on the node, as on any director.
        - name: run
          hostPath:
            path: /run/thelb
            type: DirectoryOrCreate
`, filepath.Base(cfg.SecretFile), filepath.Base(cfg.BackendSecretFile), image(), argLines.String(), env, q(host), port,
		q(cfg.SecretFile), q(filepath.Base(cfg.SecretFile)), q(cfg.BackendSecretFile), q(filepath.Base(cfg.BackendSecretFile)))
	return ds
}
