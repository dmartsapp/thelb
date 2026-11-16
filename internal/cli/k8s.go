package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"time"

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

// generateK8s prints the manifests the directors need; with director and the
// flags it runs with, the DaemonSet too, that runs directors on the nodes
// labelled for them.
func generateK8s(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	from, args, err := takeFromFlags(args)
	if err != nil {
		return err
	}
	head := fmt.Sprintf("# theLB %s, as `thelb generate k8s` printed it: the directors' identity and\n# permissions in the cluster. Apply it: kubectl apply -f <this file>\n", version.Version)
	if len(args) == 0 {
		if from.kubeconfig != "" {
			v, err := from.open(ctx, "thelb generate k8s", []string{"the API server's version"}, stderr)
			if err != nil {
				return err
			}
			head += v.fitted()
		}
		_, _ = fmt.Fprint(stdout, head+k8sIdentity)
		return nil
	}
	switch args[0] {
	case "kubeconfig":
		if len(args) > 1 {
			return usagef("thelb generate k8s kubeconfig: unexpected %q", args[1])
		}
		if from.kubeconfig != "" {
			v, err := from.open(ctx, "thelb generate k8s kubeconfig", []string{"the API server's version",
				"the Secret thelb/thelb-director-token - its token and CA go into the kubeconfig", "what that token may do, as a director would ask"}, stderr)
			if err != nil {
				return err
			}
			return kubeconfigFromCluster(ctx, v, version.Version, stdout, stderr)
		}
		_, _ = fmt.Fprintf(stdout, kubeconfigScript, version.Version)
		return nil
	case "service":
		return generateK8sService(ctx, args[1:], stdout, stderr, from)
	case "node":
		return generateK8sNode(ctx, args[1:], stdout, stderr, from)
	case "director":
	default:
		return usagef("thelb generate k8s [kubeconfig | director <the flags it runs with> | node <name> | service <name> [flags]]")
	}
	flags := args[1:]
	cmd := "generate k8s director"
	cfg, err := directorConfig(ctx, cmd, flags)
	if err != nil {
		return err
	}
	given := func(name string) bool {
		return slices.ContainsFunc(flags, func(f string) bool {
			return f == "--"+name || strings.HasPrefix(f, "--"+name+"=") || f == "-"+name || strings.HasPrefix(f, "-"+name+"=")
		})
	}
	switch {
	case cfg.Kubeconfig != kube.InCluster:
		return usagef("thelb %s: a director in a pod serves Kubernetes with its pod's ServiceAccount: give --kubeconfig %s", cmd, kube.InCluster)
	case given("name"):
		return usagef("thelb %s: each director is named after its node: leave out --name", cmd)
	case cfg.Metrics == "":
		return usagef("thelb %s: give --metrics 127.0.0.1:<port>: the pod's readiness probe asks it, at /readyz", cmd)
	case filepath.Base(cfg.SecretFile) == filepath.Base(cfg.BackendSecretFile):
		return usagef("thelb %s: the two secret files are two keys of one Secret, named after the files: give them different names", cmd)
	case cfg.Bootstrap && cfg.BootstrapWait < 31*time.Second:
		return usagef("thelb %s: a crashed pod is restarted after 10 seconds, sooner than the other of two directors stands alone: with --bootstrap, give --bootstrap-wait 31s (the control VIP's lease plus 25 seconds) or more", cmd)
	}
	env := ""
	if from.kubeconfig != "" {
		v, err := from.open(ctx, "thelb "+cmd, []string{"the API server's version", "the nodes labelled dmarts.app/thelb-director", "whether kube-proxy runs"}, stderr)
		if err != nil {
			return err
		}
		if env, err = directorFromCluster(ctx, v, stderr); err != nil {
			return err
		}
		head += v.fitted()
	}
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
	_, _ = fmt.Fprint(stdout, head+k8sIdentity+ds)
	return nil
}
