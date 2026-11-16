---
title: Log messages
lead: The messages that need you to do something - what each one means, and what to do.
description: theLB's log and error messages that need action - preflight, secrets, the control VIP, offers, assignments, failover, drift, Kubernetes - with their fixes.
section: Reference
order: 6
nav: Log messages
---

Search this page for the words in your message. Values that change - names, addresses - are shown as `<...>` or `...`.

## Starting up

| Message | What it means, and what to do |
|---|---|
| `this node lacks what theLB needs, and theLB changed nothing: ...` | Preflight: before changing anything, theLB checks its capabilities (`CAP_NET_ADMIN`, `CAP_NET_RAW`), that it can set `/proc/sys/net`, IPVS on a director, and IPv6 for an IPv6 control VIP. Each line says what is missing and how to fix it. `thelb doctor` says more. |
| `WARN this node lacks a feature theLB can do without feature=sctp\|nftables` | The kernel lacks something only some services need. `sctp`: this director, while leading, refuses SCTP services. `nftables`: this backend cannot map ports or serve tunnelled services, and the leader refuses such assignments. Everything else works. |
| `IPVS is not available (is the ip_vs module loaded?)` | Load it - `sudo modprobe ip_vs` - or install the kernel's modules. |
| `theLB is already running on this node` | One director and one backend can run on a machine, each once. |
| `thelb ... runs only as root` | Use `sudo`. |
| `the control VIP ... is already on eth0, and theLB did not add it` | Someone configured the address on this machine. Remove it, or choose another `--vip`. |
| `no interface has an address in the network of ...: name one with --interface` | The VIP is in none of this machine's networks. Use a VIP in its network, or name the interface. |
| `--vip ... resolves to ..., and none of them is in a network of this node` | A VIP given as a name must resolve to exactly one address in this machine's networks. Fix `/etc/hosts` or DNS, or give the address. |

## Joining

| Message | What it means, and what to do |
|---|---|
| `waiting for approval` | Approve the node on a director: `sudo thelb admin approve <name>`. |
| `the cluster at ... did not accept the secret in ...` | The node's secret file is out of date, or belongs to another cluster. On a director: `sudo thelb admin secret show --role director` (or `backend`); put what it prints in the file; start the node again. |
| `something that is not theLB answers on ...: the control VIP or port is taken` | Another machine or program has the control VIP or port. Choose another, or remove it there. |
| `<MAC> holds <VIP>, but nothing accepts a connection on port <port>` | The VIP answers but its port does not. Usually a firewall on the leader: open the control port to the directors (`sudo ufw allow from 10.0.0.0/24 to any port 7443 proto tcp`). Otherwise another machine has the address: choose another VIP. |
| `the name ... is a director's: give this backend a name of its own, with --name` | On a director's machine, give the backend another name. |
| `the cluster no longer accepts this backend's certificate` | The backend was removed, or the cluster was created again. It asks to join again, and waits for approval. |

## Offers and assignments

| Message | What it means, and what to do |
|---|---|
| `the leader's check of <offer> (...) at <address:port> fails: ...` | The leader cannot reach the application. Start it, make it listen on that address, or open the firewall to the directors. An `--offer` is offered again once its check passes. |
| `<node> offers tcp/8080 already, as <name>` | One offer per protocol and port on a backend. Withdraw the other first: `thelb offer remove <name>`. |
| `this backend is not a member of a cluster yet` | `thelb offer` works once the backend is approved. |
| `<node> does not offer <name>: it offers ...` | Assign one of the offers listed: see `sudo thelb status offers`. |
| `the forward path to ...: this director has no interface in ...'s network` | That backend is in another network. Use a `--method tunnel` service for it. |
| `... runs on a director, so its offer must be on the service's port` | A backend on a director's machine cannot map ports. Offer the service's port. |
| `<node> has no nftables in its kernel, so it can neither map the service's port ... nor clamp ...` | Offer the service's own port, or load `nf_tables` on that backend (`sudo modprobe nf_tables`) and start its theLB again. |
| `the leader cannot serve SCTP: ...` | The leader's IPVS has no SCTP. Lead with a director that has it (`thelb doctor` shows it), or use TCP or UDP. |
| `backend not serving: weight 0 ... reason="..."` | The reason says which condition failed - see [A backend gets no traffic](troubleshooting.md#a-backend-gets-no-traffic). |
| `creating thelbt0: ... (is the ipip module available?)` | A backend of a tunnelled service needs `ipip`: `sudo modprobe ipip`. |

## Failover and the VIPs

| Message | What it means, and what to do |
|---|---|
| `standing alone: the other director is gone; ...` | The other of two directors crashed. This one leads alone. Start the crashed one again and approve it: `thelb admin approve <name>`. |
| `no leader, and the other director may be alive: not standing alone reason=...` | Two directors without a leader, and this one cannot tell the other is gone - see [Two directors, and no leader](troubleshooting.md#two-directors-and-no-leader). |
| `standing down vip=... reason="two leaders: ..."` | Both of two directors stood alone while they could not see each other. Now they can, and this one starts over and waits for approval. **What changed on its side meanwhile is lost**: check pools, services and assignments on the leader. |
| `another machine answers for the control VIP: this leader waits reason=...` | A new leader found the control VIP still in use, and waits until it is free. Within the lease, that is a stopped theLB's VIP running out. `past the ... lease`: it was set by hand, or another theLB holds it - remove it there (`sudo ip address del <VIP>/32 dev <interface>`). |
| `another machine answers for a service VIP: this leader waits vip=...` | The same, for one service VIP. |
| `another machine answers for the control VIP by=<MAC> reason=...` | The leader saw another machine answer for its VIP, and announced the VIP again. `what a theLB killed leaves`: a crashed director's machine, until its theLB starts again. `an address conflict`: a machine that is not a director has the address - remove it there. |
| `renewing the control VIP's lease failed: ...` / `renewing service VIPs' leases failed: ...` | The leader could not renew a VIP's lifetime. If it keeps failing, the VIP runs out. The error says why. |
| `the last entries of Raft's log replayed: ... entries=N` | A director standing alone kept N changes that the crashed leader may have confirmed. Nothing to do. |
| `director gone: out of the vote, so the others keep their majority ...` | One of three or more directors crashed, and the leader took it out of the vote. Start it again: it rejoins without approval. |
| `a director has not checked in, but may be alive: it keeps its vote ...` | The leader cannot reach that director, but its machine answers: a firewall or a partition between them. |
| `IPVS connection synchronisation failed: ...` | The kernel refused the sync daemon - often because the control VIP's interface has no IPv4 address. theLB tries again every 10 seconds. Services work, but a failover would lose open connections. |
| `removed an IPVS connection sync daemon an earlier run left role=master` | A director that crashed while leading left its sync daemon running. Removed. Nothing to do. |

## While running

| Message | What it means, and what to do |
|---|---|
| `WARN drift repaired: ...` | Something else changed theLB's kernel setup, and theLB put it back. Find what did it: the two will undo each other every 10 seconds. |
| `ERROR a loop has stopped ...` | A part of theLB stopped working. Under systemd, theLB is restarted. Please [report it](https://github.com/dmartsapp/thelb/issues) with the log around it. |
| `WARN an order the leader sent is not one: skipped service=... vip=...` | On a backend: the leader sent an assignment with a VIP, protocol or port that is not one, and the backend did not plumb it. A leader of the same version never does: please [report it](https://github.com/dmartsapp/thelb/issues). |
| `thelb: argument N holds the control character ...` | An argument held a newline, a terminal's escape or another control character - often from a copy and paste. Type it again. See [What theLB checks](security.md#what-thelb-checks). |

## Kubernetes

| Message | What it means, and what to do |
|---|---|
| `OK connected to Kubernetes: serving Services of class dmarts.app/thelb api=... version=... credential=... permissions="all 13"` | A director started with `--kubeconfig` reached the API server, found a release it serves, and holds every permission it needs. Nothing to do. |
| `OK watching Kubernetes: the cluster read services=N nodes=N ready=N` | Its watch has read the cluster: `services` is the number of Services of theLB's class, `nodes` and `ready` the cluster's nodes. Nothing to do. |
| `WARN Kubernetes watch lost: retrying - meanwhile the leader serves what it last read reason=...` | The director lost the API server. It tries again, and says `OK Kubernetes watch back` once it reads again. Meanwhile the VIPs keep working, from what it read last; changes in the cluster wait. `thelb status` shows how the watch goes. |
| `the Kubernetes API server ... does not answer: ...` | At start. Check the kubeconfig's `server:`, and that this machine reaches it - a firewall, or a name it cannot resolve. |
| `the Kubernetes API server runs ...: theLB serves Kubernetes 1.35 and later` | Upgrade the cluster, or run directors without `--kubeconfig`. |
| `the Kubernetes credential (...) lacks permissions theLB needs: ...` | Each one is named. Apply what `thelb generate k8s` prints again. |
| `this director runs on the Kubernetes node ..., which is not labelled as a director: ...` | Prepare the node: `thelb generate k8s node <node>` prints the script - see [Kubernetes](kubernetes.md#3-one-director-on-a-node). |
| `WARN Kubernetes Service: ... reason=AllocationFailed` | The Service got no VIP: no default pool, an address in no pool or taken, a port another Service on a shared VIP has. `kubectl describe service` shows it too. |
| `WARN Kubernetes Service: ready pods of this Service on nodes theLB sends no traffic to ... reason=NodesExcluded` | Pods on a node theLB leaves out get no traffic from outside. The message says why for each node, and what to do - most often kubeadm's label on a control-plane node: see [Which nodes get traffic](kubernetes.md#which-nodes-get-traffic). |
| `WARN Kubernetes Service: directors run on nodes labelled ... reason=NoDirectorExclusion` | With Cilium, director nodes take the Service's packets before theLB. Add the annotation `service.cilium.io/node-selector: "!dmarts.app/thelb-director"` to the Service. |
| `WARN node not serving: weight 0 ... reason=...` | A node gets no new connections: with `Local`, it runs no ready pod of the Service - by design; with `Cluster`, its node port does not answer. |
| `WARN a Kubernetes object left out: it is not one an API server sends kind=... object=... reason=...` | The API server sent a Service, Node or EndpointSlice with a name, port or field no Kubernetes API server allows, and theLB ignores it. Look at what answers at the kubeconfig's `server:` - a proxy, or not an API server. |
