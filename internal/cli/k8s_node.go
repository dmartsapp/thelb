package cli

import (
	"context"
	"io"
	"regexp"
	"strings"

	"github.com/dmartsapp/thelb/internal/version"
)

// k8sNodeRE is a Node's name: a DNS subdomain.
var k8sNodeRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)

// nodeFacts is what the node's script knows of the cluster: read from it, or
// - read false - found out by the script itself when it runs.
type nodeFacts struct {
	read                            bool
	server, version                 string
	controlPlane, kubeProxy, cilium bool
}

// The node's script, in its parts; {{node}} and {{version}} are filled in.
// The order matters: the node is kept for theLB and kube-proxy kept off it
// before the label, which starts the DaemonSet's pod; Cilium's Services are
// touched after it, which is when Cilium reads it.
const (
	nodeHead = `#!/bin/sh
# theLB {{version}}, as ` + "`thelb generate k8s node {{node}}`" + ` printed it. Run it where kubectl
# reaches the cluster, before a director runs on the node {{node}} - in the
# DaemonSet, or under systemd: it makes {{node}} a director's node.
#   sh thelb-node.sh
# A node that only receives traffic needs none of this: no label, and nothing
# of theLB - the directors read it from the API, and check it themselves.
`
	nodeStart = `set -eu
node={{node}}
selector='!dmarts.app/thelb-director'

kubectl get node "$node" >/dev/null

`
	nodeKeepAbout = `# 1. A worker is kept for theLB: tainted, so that nothing else is scheduled
#    there, and drained of the pods it runs - a pod on a director's node cannot
#    reach theLB's Services. A control-plane node runs no workloads already.
`
	nodeKeep = `	kubectl taint node "$node" dmarts.app/thelb-director=true:NoSchedule --overwrite
	kubectl drain "$node" --ignore-daemonsets --delete-emptydir-data
	kubectl uncordon "$node"
`
	nodeProxyAbout = `# 2. With kube-proxy: kube-proxy cannot leave some Services to theLB, so it
#    must not run on the node. Its DaemonSet is kept off the labelled nodes -
#    patch it again after a kubeadm upgrade - and the node is rebooted, to
#    clear the rules kube-proxy left. A director in a pod there reaches the API
#    server only at the server's own address: see the guide.
`
	nodeProxy = `	kubectl -n kube-system patch daemonset kube-proxy --type=strategic -p \
		'{"spec":{"template":{"spec":{"affinity":{"nodeAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":{"nodeSelectorTerms":[{"matchExpressions":[{"key":"dmarts.app/thelb-director","operator":"DoesNotExist"}]}]}}}}}}}'
`
	nodeLabel = `# 3. The label: the DaemonSet runs a director on the nodes that have it, a
#    director refuses to start on a node of the cluster without it, and theLB
#    sends no Service's traffic to the node.
kubectl label node "$node" dmarts.app/thelb-director=true --overwrite

`
	nodeTouchAbout = `# 4. With Cilium: Cilium reads a node's labels only when a Service changes, so
#    every theLB Service is touched - then Cilium on the node leaves them to
#    theLB. Each needs the annotation service.cilium.io/node-selector:
#    "!dmarts.app/thelb-director", as thelb generate k8s service writes it.
`
	nodeTouch = `	kubectl get services -A -o jsonpath='{range .items[?(@.spec.loadBalancerClass=="dmarts.app/thelb")]}{.metadata.namespace}{" "}{.metadata.name}{" "}{.metadata.annotations.service\.cilium\.io/node-selector}{"\n"}{end}' |
		while read -r ns name sel; do
			if [ "$sel" != "$selector" ]; then
				echo "warning: the Service $ns/$name lacks the annotation service.cilium.io/node-selector: \"$selector\" - Cilium on $node takes its packets before theLB" >&2
			fi
			kubectl -n "$ns" annotate service "$name" --overwrite dmarts.app/thelb-touched="$(date +%s)"
		done
`
	nodeDoneRebooted = `"$node is a director's node once rebooted - kube-proxy left its rules there. Then start its director, and approve it on a director: sudo thelb status pending"`
	nodeDone         = `"$node is a director's node. Start its director, and approve it on a director: sudo thelb status pending"`
	nodeTail         = `
# To give the node back:
#   kubectl label node {{node}} dmarts.app/thelb-director-
#   kubectl taint node {{node}} dmarts.app/thelb-director-    (a worker)
# then touch theLB's Services, as step 4 does, and remove its director from the
# cluster - sudo thelb admin remove, with the name sudo thelb status directors
# shows - so that the node gets traffic again.
`
)

// nodeScript is the script that makes the node a director's node: with
// every step's test in it, or - read from the cluster - just the steps it
// needs.
func nodeScript(node string, f nodeFacts) string {
	var b strings.Builder
	b.WriteString(nodeHead)
	if f.read {
		b.WriteString("# Fitted to the cluster at " + clean(f.server) + ", Kubernetes " + clean(f.version) + ":\n")
		kind := "a worker"
		if f.controlPlane {
			kind = "a control-plane node"
		}
		var runs []string
		if f.cilium {
			runs = append(runs, "Cilium runs on it")
		}
		if f.kubeProxy {
			runs = append(runs, "kube-proxy runs in the cluster")
		}
		if len(runs) == 0 {
			runs = append(runs, "neither Cilium nor kube-proxy runs")
		}
		b.WriteString("# {{node}} is " + kind + "; " + strings.Join(runs, ", ") + ".\n")
	}
	b.WriteString(nodeStart)
	b.WriteString(nodeKeepAbout)
	switch {
	case !f.read:
		b.WriteString("if kubectl get nodes -l node-role.kubernetes.io/control-plane -o name | grep -qx \"node/$node\"; then\n")
		b.WriteString("\techo \"$node is a control-plane node: not tainted, not drained\"\nelse\n" + nodeKeep + "fi\n\n")
	case f.controlPlane:
		b.WriteString("#    {{node}} is a control-plane node: left as it is.\n\n")
	default:
		b.WriteString(unindent(nodeKeep) + "\n")
	}
	b.WriteString(nodeProxyAbout)
	switch {
	case !f.read:
		b.WriteString("kubeproxy=no\nif kubectl -n kube-system get daemonset kube-proxy >/dev/null 2>&1; then\n\tkubeproxy=yes\n" + nodeProxy + "fi\n\n")
	case f.kubeProxy:
		b.WriteString(unindent(nodeProxy) + "\n")
	default:
		b.WriteString("#    kube-proxy does not run in this cluster: nothing to do.\n\n")
	}
	b.WriteString(nodeLabel)
	b.WriteString(nodeTouchAbout)
	switch {
	case !f.read:
		b.WriteString("if kubectl get ciliumnodes.cilium.io \"$node\" >/dev/null 2>&1; then\n" + nodeTouch + "fi\n\n")
		b.WriteString("if [ \"$kubeproxy\" = yes ]; then\n\techo " + nodeDoneRebooted + "\nelse\n\techo " + nodeDone + "\nfi\n")
	case f.cilium:
		b.WriteString(unindent(nodeTouch) + "\n")
	default:
		b.WriteString("#    Cilium does not run on {{node}}: no Service to touch.\n\n")
	}
	if f.read {
		if f.kubeProxy {
			b.WriteString("echo " + nodeDoneRebooted + "\n")
		} else {
			b.WriteString("echo " + nodeDone + "\n")
		}
	}
	b.WriteString(nodeTail)
	return b.String()
}

// unindent takes one tab off each line: a step that runs as it is, not
// inside its test.
func unindent(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimPrefix(l, "\t")
	}
	return strings.Join(lines, "\n")
}

// generateK8sNode prints the script that makes a node a director's node -
// fitted to the cluster, with from.
func generateK8sNode(ctx context.Context, node string, stdout, stderr io.Writer, from fromCluster) error {
	const cmd = "thelb generate k8s node"
	if len(node) > 253 || !k8sNodeRE.MatchString(node) {
		return usagef("%s: %q is not a node's name", cmd, node)
	}
	var f nodeFacts
	if from.kubeconfig != "" {
		v, err := from.open(ctx, cmd+" "+node, []string{"the API server's version", "the node " + node, "whether Cilium runs on it", "whether kube-proxy runs", "theLB's Services, with Cilium"}, stderr)
		if err != nil {
			return err
		}
		if f, err = nodeFromCluster(ctx, v, node, stderr); err != nil {
			return err
		}
	}
	r := strings.NewReplacer("{{node}}", node, "{{version}}", version.Version)
	_, _ = io.WriteString(stdout, r.Replace(nodeScript(node, f)))
	return nil
}
