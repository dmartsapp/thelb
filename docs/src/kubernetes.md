---
title: Kubernetes
lead: theLB gives Services of type LoadBalancer their VIPs, and sends their traffic straight to the nodes that run their pods. Nothing of theLB runs on the nodes.
description: Run theLB as the load balancer of a Kubernetes cluster - the manifests thelb generate k8s prints, directors outside the cluster or on its nodes, Services of class dmarts.app/thelb, and how nodes are chosen.
section: Guides
order: 7
nav: Kubernetes
---

## How it works

A Service asks for theLB with **`loadBalancerClass: dmarts.app/thelb`**. Then:

1. The leader gives the Service a VIP from a theLB [pool](services.md), and writes it in the Service's status, with `ipMode: VIP`. `kubectl get service` shows it as the `EXTERNAL-IP`.
2. The leader holds the VIP and sends each connection to a **node that runs a ready pod** of the Service, by direct routing - the packet is unchanged.
3. The node's own service proxy - kube-proxy, or Cilium - already knows the VIP from the Service's status. It hands the packet to a pod on that node.
4. The pod answers. The reply goes from the node straight to the client, with the VIP as its source. It never comes back through the director.

**Nothing runs on the nodes**: no agent, no approval. theLB reads the cluster's nodes, Services and endpoints from the Kubernetes API, and checks each node at the Service's health port. A new node gets traffic as soon as it runs a ready pod.

What you need:

- **Kubernetes 1.35** or later.
- **Directors on the nodes' network**: direct routing sends packets to a node's MAC address. IP-in-IP is not used for Kubernetes.
- **Cilium** as kube-proxy replacement, or **kube-proxy**. kube-proxy in IPVS mode needs `strictARP: true` - see [With kube-proxy](#with-kube-proxy).

## Where the directors run

You choose. A cluster can mix them.

| Directors | How | What it takes |
|---|---|---|
| On machines outside the cluster | under systemd | a kubeconfig - [step 2a](#2a-directors-outside-the-cluster) |
| On nodes of the cluster | a DaemonSet, or systemd on the node | the node labelled, and kept for theLB - [step 2b](#2b-directors-on-nodes-as-a-daemonset) |

**Why a director's node is special.** The node's own proxy takes every packet for a Service's VIP - on every node, before theLB's IPVS sees it. On a director's node, it must leave theLB's Services alone:

- **With Cilium**, the node is labelled `dmarts.app/thelb-director`, and each theLB Service has the annotation `service.cilium.io/node-selector: "!dmarts.app/thelb-director"`. Cilium then skips those Services on the labelled nodes only. `thelb generate k8s service` writes the annotation for you.
- **With kube-proxy**, kube-proxy must not run on the director's node at all - see [With kube-proxy](#with-kube-proxy).

On a director's node, pods cannot reach theLB's Services by their cluster IP either. So keep these nodes for theLB: a control-plane node is a good choice, because it runs no workloads already.

:::note A director checks its node
A director that finds its own address on a node of the cluster refuses to start unless the node has the label `dmarts.app/thelb-director`.
:::

## Step by step

The example: directors `lb1` (outside the cluster) and `cp1` (the control-plane node), control VIP `10.0.0.10:7443`, Service VIPs from `10.0.0.50`. Every file below is what `thelb generate k8s` prints - copy it, or download it.

### 1. The directors' identity

On a director, print the manifests. Apply them wherever `kubectl` works:

```bash
sudo thelb generate k8s > thelb-k8s.yaml
kubectl apply -f thelb-k8s.yaml
```

:::example thelb-k8s.yaml
A namespace, a ServiceAccount, the permissions - reading, and a Service's status, never a Service itself - and a token for directors outside the cluster.
:::

### 2a. Directors outside the cluster

A director outside the cluster needs a kubeconfig with the token from step 1. theLB prints a script that builds it - run it where `kubectl` works:

```bash
sudo thelb generate k8s kubeconfig > thelb-kubeconfig.sh
sh thelb-kubeconfig.sh > thelb.kubeconfig
```

:::example thelb-kubeconfig.sh
The script: it reads the API server's address and the token, and prints the kubeconfig.
:::

Copy `thelb.kubeconfig` to the director, readable by root only, and add `--kubeconfig` to its flags:

```bash
sudo install -D -m 600 thelb.kubeconfig /etc/thelb/kubeconfig
sudo thelb generate systemd director --vip 10.0.0.10 --port 7443 --bootstrap \
  --secret-file /etc/thelb/director.secret --backend-secret-file /etc/thelb/backend.secret \
  --kubeconfig /etc/thelb/kubeconfig | sudo tee /etc/systemd/system/thelb-director.service
sudo systemctl daemon-reload
sudo systemctl enable --now thelb-director
```

A director under systemd **on a node** of the cluster is set up the same way - and its node is labelled, as in step 2b.

### 2b. Directors on nodes, as a DaemonSet

Label each node that runs a director. A node other than a control-plane node is also tainted, so that nothing else runs there:

```bash
kubectl label node cp1 dmarts.app/thelb-director=true
# a worker kept for theLB:
# kubectl taint node <node> dmarts.app/thelb-director=true:NoSchedule
```

The pods read the cluster's two secret files from a Secret. Copy them from a director (`lb1` here), then:

```bash
kubectl -n thelb create secret generic thelb-secrets \
  --from-file=director.secret=./director.secret \
  --from-file=backend.secret=./backend.secret
rm director.secret backend.secret
```

Print the DaemonSet with the flags the directors run with, and apply it:

```bash
sudo thelb generate k8s director --vip 10.0.0.10 --port 7443 \
  --secret-file /etc/thelb/director.secret --backend-secret-file /etc/thelb/backend.secret \
  --kubeconfig in-cluster --metrics 127.0.0.1:9180 > thelb-director.yaml
kubectl apply -f thelb-director.yaml
```

:::example thelb-director.yaml
The manifests of step 1, and the DaemonSet. `generate` checks the flags the way the director would.
:::

What the pod needs, and why:

- **`hostNetwork`**: the VIPs, IPVS and the control port are the node's.
- **`privileged`**: theLB writes kernel settings in `/proc/sys/net`, which is read-only in a container otherwise.
- **The secret files, each mounted as itself** (`subPath`): theLB reads a secret file only if it is a regular file, and a Secret's files are symbolic links otherwise.
- **The node's `/etc/machine-id`**: it is part of the [node fingerprint](approval.md), so a restarted pod rejoins without approval.
- **The node's `/run/thelb`**: `sudo thelb status` works on the node, as on any director.
- **A readiness probe** on `/readyz`, served with `--metrics`: a pod is ready once it votes and has caught up with the leader. A rolling update then replaces the next director only after that.
- **`--kubeconfig in-cluster`**: the pod uses its own ServiceAccount; no kubeconfig file.

The new director waits for approval. On a director:

```bash
sudo thelb status pending
sudo thelb admin approve cp1
```

:::warning Bootstrapping from a pod
The DaemonSet above has no `--bootstrap`: here, `lb1` creates the cluster. If your pods must bootstrap, add `--bootstrap --bootstrap-wait 31s`. Kubernetes restarts a crashed pod after 10 seconds - sooner than the other of two directors stands alone - so the pod must look for the cluster longer before it creates one. `generate` refuses `--bootstrap` without it.
:::

### 3. A pool

On any director:

```bash
sudo thelb admin pool add k8s --vip 10.0.0.50/24 --vippool-count 10 --default
```

A Service takes its VIP from the default pool, unless it names another.

### 4. A Service

Print a Service of theLB's class, and add your Deployment's pods to it:

```bash
sudo thelb generate k8s service web > web-service.yaml
kubectl apply -f web-service.yaml
```

:::example web-service.yaml
`thelb generate k8s service web`: port 80, the pods labelled `app: web`.
:::

`generate k8s service` takes:

| Flag | What it sets |
|---|---|
| `--namespace <ns>` | the Service's namespace (default `default`) |
| `--port <port>[/tcp|udp|sctp][:<pods' port>]` | a port, more than once for more; `80:8080` sends port 80 to the pods' 8080 (default `80`) |
| `--selector <key=value>` | the pods it sends to (default `app=<name>`) |
| `--policy Local|Cluster` | its `externalTrafficPolicy` (default `Local`) |
| `--pool <pool>` | the annotation `dmarts.app/thelb-pool` |
| `--ip <address>` | the annotation `dmarts.app/thelb-ip` |
| `--shared-vip <key>` | the annotation `dmarts.app/thelb-shared-vip` |

Then:

```bash
kubectl get service web           # EXTERNAL-IP: 10.0.0.50
sudo thelb status vips            # on a director: the VIP, held by the leader
curl http://10.0.0.50/            # from a client
```

`thelb status services` and `thelb status backends` list each port of the Service on its VIP, and the nodes serving it, with their weights and checks.

## Which nodes get traffic

| Policy | The nodes that get traffic | Their weight | The client's address |
|---|---|---|---|
| `Local` (recommended) | those whose health port - the Service's `healthCheckNodePort` - answers `200`: they run a ready pod of the Service | the node's number of ready pods | reaches the pod |
| `Cluster` | every ready node; checked at the port's node port, for TCP | 1 | lost: the node passes traffic to a pod on any node, through source NAT |

Never a node that is not `Ready`, a director's node, or a node labelled `node.kubernetes.io/exclude-from-external-load-balancers` - the label Kubernetes defines for this:

```bash
kubectl label node worker1 node.kubernetes.io/exclude-from-external-load-balancers=true
```

With `Local`, draining a node is enough: once it has no ready pod, its health port says `503`, and it gets no new connections.

## Annotations

| Annotation | What it does |
|---|---|
| `dmarts.app/thelb-pool: <pool>` | takes the VIP from that pool, not the default. A dual-stack Service names one pool for each family, separated by commas. |
| `dmarts.app/thelb-ip: <address>` | asks for that address of the pool. One for each family, separated by commas. |
| `dmarts.app/thelb-shared-vip: <key>` | Services with the same key share one VIP, as long as their ports are different. |
| `service.cilium.io/node-selector: "!dmarts.app/thelb-director"` | with Cilium and directors on nodes: Cilium leaves the Service to those nodes. |

A dual-stack Service (`spec.ipFamilies`) gets one VIP for each family, from that family's pool.

## Changing the director nodes

**Cilium reads a node's labels only when a Service changes.** After you label a new director node - or take the label off one - touch every theLB Service, so that Cilium on that node takes the change:

```bash
kubectl get services -A -o jsonpath='{range .items[?(@.spec.loadBalancerClass=="dmarts.app/thelb")]}{.metadata.namespace}{" "}{.metadata.name}{"\n"}{end}' |
  while read -r ns name; do
    kubectl -n "$ns" annotate service "$name" --overwrite dmarts.app/thelb-touched="$(date +%s)"
  done
```

Do it **before** you approve the new director, so that it never leads while Cilium still takes its packets.

To make a worker a director node: drain it, taint it, label it, touch the Services, approve the director:

```bash
kubectl drain worker2 --ignore-daemonsets --delete-emptydir-data
kubectl taint node worker2 dmarts.app/thelb-director=true:NoSchedule
kubectl uncordon worker2
kubectl label node worker2 dmarts.app/thelb-director=true
# touch the Services, as above
sudo thelb admin approve worker2
```

To give it back: take off the label (the DaemonSet stops its director) and the taint, then touch the Services:

```bash
kubectl label node worker2 dmarts.app/thelb-director-
kubectl taint node worker2 dmarts.app/thelb-director-
```

## With kube-proxy

kube-proxy has no way to skip a Service on some nodes only. So:

**In IPVS mode**, kube-proxy puts every load-balancer IP on an interface of every node. Set `strictARP`, so that the nodes do not answer ARP for the VIPs:

```bash
kubectl -n kube-system get configmap kube-proxy -o yaml \
  | sed -e 's/strictARP: false/strictARP: true/' \
  | kubectl apply -f -
kubectl -n kube-system rollout restart daemonset kube-proxy
```

**A director on a node** needs kube-proxy kept off that node, and the node rebooted to clear the rules kube-proxy left. `kubeadm upgrade` rewrites kube-proxy's DaemonSet: patch it again after an upgrade.

```bash
kubectl -n kube-system patch daemonset kube-proxy --type=strategic -p \
  '{"spec":{"template":{"spec":{"affinity":{"nodeAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":{"nodeSelectorTerms":[{"matchExpressions":[{"key":"dmarts.app/thelb-director","operator":"DoesNotExist"}]}]}}}}}}}'
```

Pods on a node without kube-proxy reach no Service at all - one more reason to keep these nodes for theLB. That includes the API server's own Service, which a director in a pod finds with `--kubeconfig in-cluster`: give the DaemonSet's container the API server's real address instead, which its certificate names:

```yaml
          env:
            - name: KUBERNETES_SERVICE_HOST
              value: "10.0.0.21"     # the API server's own address
            - name: KUBERNETES_SERVICE_PORT
              value: "6443"
```

Or run the directors on these nodes under systemd, with a kubeconfig.

## When something is wrong

- **The director does not start.** It says why: the API server does not answer, it runs a Kubernetes release older than 1.35, the credential lacks a permission (each one is named - apply step 1 again), or its node is not labelled.
- **A Service has no `EXTERNAL-IP`.** `kubectl describe service <name>` shows theLB's events: `AllocationFailed` says why - no default pool, an address in no pool or taken, a port another Service on a shared VIP has.
- **A warning `NoDirectorExclusion`.** Directors run on labelled nodes, and the Service lacks the Cilium annotation.
- **Nodes get no traffic.** `sudo thelb status backends` shows each node's check and weight. With `Local`, a node without a ready pod has weight 0 - by design.
- **`thelb status`** shows, in the cluster table, how the director's watch of Kubernetes goes: the number of theLB Services, or what is wrong.

The design behind all this - the choices and their reasons - is in [docs/design.md](https://github.com/dmartsapp/thelb/blob/main/docs/design.md#kubernetes).
