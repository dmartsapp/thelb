#!/bin/sh
# theLB 0.1.0-alpha.1, as `thelb generate k8s node cp1` printed it. Run it where kubectl
# reaches the cluster, before a director runs on the node cp1 - in the
# DaemonSet, or under systemd: it makes cp1 a director's node.
#   sh thelb-node.sh
# A node that only receives traffic needs none of this: no label, and nothing
# of theLB - the directors read it from the API, and check it themselves.
set -eu
node=cp1
selector='!dmarts.app/thelb-director'

kubectl get node "$node" >/dev/null

# 1. A worker is kept for theLB: tainted, so that nothing else is scheduled
#    there, and drained of the pods it runs - a pod on a director's node cannot
#    reach theLB's Services. A control-plane node runs no workloads already.
if kubectl get nodes -l node-role.kubernetes.io/control-plane -o name | grep -qx "node/$node"; then
	echo "$node is a control-plane node: not tainted, not drained"
else
	kubectl taint node "$node" dmarts.app/thelb-director=true:NoSchedule --overwrite
	kubectl drain "$node" --ignore-daemonsets --delete-emptydir-data
	kubectl uncordon "$node"
fi

# 2. With kube-proxy: kube-proxy cannot leave some Services to theLB, so it
#    must not run on the node. Its DaemonSet is kept off the labelled nodes -
#    patch it again after a kubeadm upgrade - and the node is rebooted, to
#    clear the rules kube-proxy left. A director in a pod there reaches the API
#    server only at the server's own address: see the guide.
kubeproxy=no
if kubectl -n kube-system get daemonset kube-proxy >/dev/null 2>&1; then
	kubeproxy=yes
	kubectl -n kube-system patch daemonset kube-proxy --type=strategic -p \
		'{"spec":{"template":{"spec":{"affinity":{"nodeAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":{"nodeSelectorTerms":[{"matchExpressions":[{"key":"dmarts.app/thelb-director","operator":"DoesNotExist"}]}]}}}}}}}'
fi

# 3. The label: the DaemonSet runs a director on the nodes that have it, a
#    director refuses to start on a node of the cluster without it, and theLB
#    sends no Service's traffic to the node.
kubectl label node "$node" dmarts.app/thelb-director=true --overwrite

# 4. With Cilium: Cilium reads a node's labels only when a Service changes, so
#    every theLB Service is touched - then Cilium on the node leaves them to
#    theLB. Each needs the annotation service.cilium.io/node-selector:
#    "!dmarts.app/thelb-director", as thelb generate k8s service writes it.
if kubectl get ciliumnodes.cilium.io "$node" >/dev/null 2>&1; then
	kubectl get services -A -o jsonpath='{range .items[?(@.spec.loadBalancerClass=="dmarts.app/thelb")]}{.metadata.namespace}{" "}{.metadata.name}{" "}{.metadata.annotations.service\.cilium\.io/node-selector}{"\n"}{end}' |
		while read -r ns name sel; do
			if [ "$sel" != "$selector" ]; then
				echo "warning: the Service $ns/$name lacks the annotation service.cilium.io/node-selector: \"$selector\" - Cilium on $node takes its packets before theLB" >&2
			fi
			kubectl -n "$ns" annotate service "$name" --overwrite dmarts.app/thelb-touched="$(date +%s)"
		done
fi

if [ "$kubeproxy" = yes ]; then
	echo "$node is a director's node once rebooted - kube-proxy left its rules there. Then start its director, and approve it on a director: sudo thelb status pending"
else
	echo "$node is a director's node. Start its director, and approve it on a director: sudo thelb status pending"
fi

# To give the node back:
#   kubectl label node cp1 dmarts.app/thelb-director-
#   kubectl taint node cp1 dmarts.app/thelb-director-    (a worker)
# then touch theLB's Services, as step 4 does, and remove its director from the
# cluster - sudo thelb admin remove, with the name sudo thelb status directors
# shows - so that the node gets traffic again.
