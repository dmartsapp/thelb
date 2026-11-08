# Design

How theLB works, and why. The principles behind every choice here are in [Philosophy](philosophy.md); where this document and the philosophy disagree, the philosophy wins and this document is wrong.

Status: **design**, before any code. Decisions still open are listed at the end, in [Open questions](#open-questions).

## Contents

1. [What theLB does](#what-thelb-does)
2. [Words](#words)
3. [Where it works](#where-it-works)
4. [Roles](#roles)
5. [The data path: IPVS direct routing](#the-data-path-ipvs-direct-routing)
6. [Kernel state theLB manages](#kernel-state-thelb-manages)
7. [Startup, the watchdog loop, and stopping](#startup-the-watchdog-loop-and-stopping)
8. [VIPs and announcing them](#vips-and-announcing-them)
9. [The cluster](#the-cluster)
10. [Health checks](#health-checks)
11. [Kubernetes](#kubernetes)
12. [Command line and configuration](#command-line-and-configuration)
13. [What `thelb generate` prints](#what-thelb-generate-prints)
14. [Observability](#observability)
15. [Security](#security)
16. [Failure modes](#failure-modes)
17. [Testing](#testing)
18. [Project, tooling and releases](#project-tooling-and-releases)
19. [Phases](#phases)
20. [Open questions](#open-questions)

## What theLB does

- **L4 load balancing** of TCP and UDP to a virtual IP (VIP), IPv4 and IPv6, by the kernel's IPVS in **direct-routing** mode: replies go from the backends straight to the clients.
- **VIP ownership on the network**: it adds the VIP to the right interface and announces it on the L2 network (gratuitous ARP for IPv4, unsolicited Neighbor Advertisements for IPv6).
- **High availability**: a cluster of directors agrees, through Raft, which one holds each VIP; when it fails, another takes the VIP over.
- **Backend health checks**: a failed backend stops receiving new connections.
- **Both sides of direct routing**: the same binary configures the backends, which must hold the VIP without announcing it.
- **A Kubernetes `LoadBalancer` provider** (Kubernetes 1.35.0 or later): VIPs for Services of type `LoadBalancer`, with the cluster's nodes as backends.
- **Explanations and file contents, never files**: `doctor`, `plan`, `status`, and `generate` for systemd units, sysctl configuration and Kubernetes manifests.

Not in scope, for now: proxying (a later capability - see [Phases](#phases)), NAT, L7, routed networks (BGP), and cloud platforms' load-balancer APIs.

## Words

| Word | Meaning |
|---|---|
| **VIP** | The virtual IP clients connect to. One VIP can carry several services (ports). |
| **Service** | A VIP, a protocol (TCP or UDP) and a port, with its backends. In IPVS terms, a virtual service. |
| **Director** | A node that can hold VIPs and forwards their traffic with IPVS. |
| **Backend** | A node that serves the traffic: it holds the VIP without announcing it, and answers clients directly. IPVS calls it a real server. |
| **Agent** | theLB running in the backend role. On a Kubernetes node it is a pod of the agent DaemonSet. |
| **Voter**, **non-voter** | Raft members that do, and do not, count toward the cluster's majority. Directors vote; agents do not. |

## Where it works

Direct routing has hard requirements, and theLB checks them rather than hoping:

- **Directors and backends share an L2 network** (one broadcast domain, per address family). The director forwards a packet by sending it to the backend's MAC address, so the backend must be on the same link.
- **No port translation.** A backend receives the packet exactly as the client sent it: VIP and port unchanged. The backend must listen on the service's port.
- **Backends must accept traffic to the VIP and must not announce it** - see [Kernel state](#kernel-state-thelb-manages).
- **The network must let a VIP move between machines.** On-premises switches and most hypervisors do (some need settings such as accepting forged transmits); public clouds generally do not. `thelb doctor` detects the major clouds and says so.
- **Linux only**, with IPVS in the kernel. Minimum kernel: see [Open questions](#open-questions).

## Roles

Every node runs the same binary. Its role comes from its configuration:

| Role | Does |
|---|---|
| `director` | Joins the cluster as a voter. When it holds a VIP: adds it to the interface, announces it, programs IPVS, health-checks the backends. When it does not: holds nothing and announces nothing. |
| `backend` | Joins the cluster as a non-voter. Holds every VIP it serves on its own dummy interface, without announcing it; sets the kernel parameters that keep it quiet; reports its health. |
| `director,backend` | Both on one node (small setups). A director that is also a backend forwards to itself through the local stack. |

On a Kubernetes node, the backend role mostly checks rather than configures: kube-proxy already accepts traffic for a Service's load-balancer IP (see [Kubernetes](#kubernetes)).

## The data path: IPVS direct routing

```text
client ──▶ switch ──▶ director (holds VIP, IPVS: rewrite dst MAC) ──▶ backend (VIP on thelb0)
   ▲                                                                       │
   └──────────────────────── reply, from the VIP, straight to the client ◀─┘
```

1. The client sends to `VIP:port`. The switch delivers it to the director, which answers ARP/NDP for the VIP.
2. IPVS on the director picks a backend and sends the **unchanged** packet to that backend's MAC address.
3. The backend holds the VIP locally, so its kernel accepts the packet and the service answers - from the VIP, to the client, directly.

theLB programs IPVS over its netlink interface (no `ipvsadm`):

- **One IPVS service per VIP, protocol and port**; each backend a destination with forwarding method *direct routing* and a weight.
- **Scheduler**: Maglev hashing (`mh`) by default. It maps a flow to the same backend on every director with the same backend set, so a failover keeps most flows on the backend they were using. Round robin, weighted round robin and least connections are options.
- **Unhealthy backend**: weight 0 - no new connections, existing ones finish - not removed. A backend taken out of the configuration is removed, and its connections expire (`expire_nodest_conn`).
- **Failover without losing connections**: `sloppy_tcp` lets the new director accept a flow it never saw start; Maglev sends it to the same backend. IPVS's own connection synchronisation (the kernel's sync daemon, master to backups) is an option on top, off by default.
- **Session persistence** (one client to one backend for a time) is a per-service option.

## Kernel state theLB manages

Everything here is **runtime state**. theLB sets it at runtime - kernel parameters by writing `/proc/sys`, which is what the `sysctl` command does (the old `sysctl(2)` system call was removed from Linux in 5.5) - and never writes a configuration file. What is already right is left alone and never reverted; what theLB changes is recorded and restored when it stops cleanly ([Philosophy](philosophy.md#2-the-running-kernel-is-the-truth)).

### Director

| State | Value | Why |
|---|---|---|
| Kernel modules | `ip_vs`, and the schedulers in use (`ip_vs_mh` by default) | IPVS itself. Loaded only if missing; needs `CAP_SYS_MODULE` only then. |
| `net.ipv4.vs.sloppy_tcp` | `1` | A director that takes over accepts flows already in progress. |
| `net.ipv4.vs.expire_nodest_conn` | `1` | Connections to a removed backend end at once instead of hanging. |
| `net.ipv4.vs.expire_quiescent_template` | `1` | Persistence templates of a weight-0 backend expire, so its clients move on. |
| VIP addresses | on the chosen interface, `/32` or `/128` | Only while this director holds the VIP. |
| IPVS services | one per VIP, protocol and port | Only while this director holds the VIP. |

The `net.ipv4.vs.*` parameters apply to IPv6 services too; IPVS keeps them all under `net.ipv4`. Direct routing needs no IP forwarding (`ip_forward`) on the director: IPVS transmits the packets itself.

### Backend (plain Linux)

| State | Value | Why |
|---|---|---|
| Dummy interface `thelb0` | created by theLB, up, no ARP | Holds the VIPs. theLB's own interface, so it never changes `lo`, and removing it removes everything theLB added. |
| VIP addresses on `thelb0` | `/32`, `/128` (IPv6 without duplicate address detection) | The kernel accepts the packets the director forwards. |
| `net.ipv4.conf.all.arp_ignore` | `1` | Answer ARP only for addresses on the interface the request came in on - never for the VIP on `thelb0`. |
| `net.ipv4.conf.all.arp_announce` | `2` | Never use the VIP as the source of the backend's own ARP requests. |

**IPv6** has no `arp_ignore`. Linux answers neighbour solicitations only for addresses on the receiving interface, so a VIP on `thelb0` is not answered on `eth0`; phase 0 must prove this on every kernel theLB supports, before anything relies on it.

### Telling what theLB owns

theLB keeps nothing on disk, so after a restart it recognises its own state from the kernel:

- **Addresses** carry theLB's own address protocol value (`IFA_PROTO`, the "who added this address" field Linux has since 6.1), and IPv4 addresses also a label (`eth0:thelb`).
- **IPVS services** are theLB's when they match a configured service; any other IPVS service is left alone, and `doctor` reports it.
- **`thelb0`** is theLB's by name.
- **Kernel parameters** cannot be marked. After a crash theLB cannot know a parameter's value from before it started, so it treats the current value as the one to leave; this is logged.

## Startup, the watchdog loop, and stopping

```text
start ─▶ preflight ─▶ read the kernel ─▶ plan ─▶ apply ─▶ join the cluster ─▶ watchdog loop ─▶ stop
           │                               │
           └─ missing authority: exit 1    └─ `thelb plan` prints this and exits
```

1. **Preflight - full authority or nothing.** Effective capabilities (`CAP_NET_ADMIN`, `CAP_NET_RAW`; `CAP_SYS_MODULE` if a module is missing); `/proc/sys/net` writable; IPVS present (netlink family, schedulers); IPv6 enabled if any VIP is IPv6; the interface exists; in Kubernetes, the API server reachable and every permission held (checked with `SelfSubjectAccessReview`). Anything missing: theLB changes nothing, exits `1`, and prints what is missing and how to grant it.
2. **Read the kernel**: every parameter, address, interface and IPVS entry it cares about.
3. **Plan**: the difference between what it needs and what is there.
4. **Apply**: kernel parameters and modules now; VIPs and IPVS services only when the cluster gives this node a VIP. Each change is logged with its old value.
5. **Watchdog loop**: netlink notifications (addresses, links) and a periodic full comparison (every 10 s by default). Anything that drifted - an address removed, `ipvsadm -C`, a parameter changed - is put back and logged. Under systemd, theLB reports to the service manager's watchdog only while this loop is healthy, so a hung process is restarted.
6. **Stop** (`SIGTERM`): hand leadership to another director first if this one leads (so a planned restart is a short, clean failover), then give up its VIPs, remove its IPVS services, addresses and `thelb0`, and restore the parameters it changed. After a crash nothing is cleaned up; the next start finds the state in the kernel and adopts or removes it.

## VIPs and announcing them

- **Interface**: the one given on the command line or in the configuration, or else the interface the route to the VIP's network leaves through. A VIP that no interface's network contains is an error.
- **Adding**: `/32` or `/128`, with theLB's address protocol and label. IPv6 VIPs skip duplicate address detection, so a takeover is immediate; the cluster, not DAD, prevents duplicates.
- **Announcing**, when a director takes a VIP: gratuitous ARP (request and reply forms) for IPv4, unsolicited Neighbor Advertisements with the override flag to all-nodes for IPv6 - three of each over two seconds, because switches and hosts miss single packets. A periodic re-announcement is an option, off by default.
- **Conflict detection**: a director listens for ARP and NDP traffic claiming one of its VIPs from another MAC address. That means another system holds it - a second cluster, a forgotten static address - and is logged as an error and counted in the metrics. theLB does not fight over it.
- **Giving up**: the address is removed. Nothing is announced; the next director's announcement moves the clients.

## The cluster

- **[Raft](https://raft.github.io/)** ([hashicorp/raft](https://github.com/hashicorp/raft)) among the directors. One director is a working cluster, without failover; **three are recommended**; five for more tolerance. `doctor` warns about two voters: losing either loses the majority, so two are less available than one.
- **Agents are non-voters**: they receive the replicated state and report their health, but never count toward the majority, however many backends there are.
- **What is replicated**: the desired services (from the configuration file or Kubernetes), which director holds which VIP, cluster membership, and backend health as the agents and directors see it.
- **Who holds the VIPs**: at first the Raft leader holds them all - one active director, the others standing by. Spreading VIPs across directors (active-active, assigned by the leader) comes later.
- **Fail closed**: holding a VIP requires a valid lease from the majority. A director that loses it - a network partition, a hung leader - gives its VIPs up within the lease time, even if it seems healthy to itself.
- **Failover time**: Raft's election timeout (about a second by default, a few hundred milliseconds on a LAN) plus the announcement. To be measured, not promised.
- **Transport**: TCP with mutual TLS between members.
- **Raft's own state** - its log and snapshots - is a decision still open: see [Open questions](#open-questions).

## Health checks

- **Kinds**: TCP connect; HTTP or HTTPS `GET` with an expected status; none (always healthy).
- **Defaults**: every 2 s, 1 s timeout, healthy after 2 successes, unhealthy after 3 failures.
- **Run by the director holding the VIP**, to each backend's own address - never through the VIP, which on a director is local.
- **Result**: an unhealthy backend gets weight 0; when every backend of a service is unhealthy, the service stays (connections are refused by the backends, not black-holed) and the state is reported.
- IPv4 and IPv6 backends alike.

## Kubernetes

theLB is a provider for Services of type `LoadBalancer`, for **Kubernetes 1.35.0 and later**. It refuses to start against an older API server.

### How traffic reaches the pods

With direct routing, a packet reaches a node with the VIP as its destination. **kube-proxy already handles that**: it programs every Service's load-balancer IP, so the packet goes on to a pod, and the reply leaves from the node, with the VIP as its source, straight to the client. theLB therefore:

- uses the **nodes as backends**, at the **Service's port** (not its node port): the packet keeps `VIP:port`, which kube-proxy matches;
- writes `status.loadBalancer.ingress` with **`ipMode: VIP`**: the load balancer passes packets through unchanged, so kube-proxy may also send in-cluster traffic for the VIP straight to the pods;
- needs nodes **not to answer ARP for the VIP**. kube-proxy in IPVS mode puts load-balancer IPs on its own interface, so in that mode `strictARP` must be on; the agent checks it on every node.

### Which Services, and which backends

- Services of type `LoadBalancer` with **`spec.loadBalancerClass`** set to theLB's class (name: see [Open questions](#open-questions)), so theLB never takes Services meant for another provider.
- **VIPs from address pools** per family; dual-stack Services (`spec.ipFamilies`) get one VIP per family.
- **`externalTrafficPolicy: Local`**: backends are the nodes with ready endpoints, checked at the Service's `healthCheckNodePort`, which kube-proxy answers per node; the client's address reaches the pod.
- **`externalTrafficPolicy: Cluster`**: every ready node is a backend, checked through kube-proxy's own health endpoint.
- **Directors are not Kubernetes nodes running kube-proxy** in the first releases: kube-proxy would itself take the VIP's traffic on that node, and in IPVS mode the two would share the kernel's IPVS table. `doctor` refuses that combination.

### Where theLB runs

| Component | Runs as | Talks to the API with |
|---|---|---|
| Directors | systemd services on their own machines, outside the cluster | a **kubeconfig** for the `thelb-director` ServiceAccount - required, and checked at startup |
| Agents | the `thelb-agent` **DaemonSet**, one pod per node, host network | the pod's **ServiceAccount token**, which Kubernetes mounts into the pod; no kubeconfig file is needed inside the cluster. `--kubeconfig` and `$KUBECONFIG` are used only outside it |

Both check at startup that the API server answers and that they hold every permission they need (`SelfSubjectAccessReview`, which every authenticated user may call); a missing one stops theLB with its name.

### The manifests

`thelb generate k8s` prints everything, for namespace **`thelb`** (Kubernetes namespace names are lowercase):

| Object | What for |
|---|---|
| `Namespace` `thelb` | Everything theLB runs, labelled `pod-security.kubernetes.io/enforce: privileged`: the agent needs host networking and writable kernel parameters, which no other Pod Security level allows. |
| `ServiceAccount` `thelb-agent`, `thelb-director` | One identity per component. |
| `ClusterRole` + `ClusterRoleBinding` per component | The cluster-wide permissions below. |
| `Role` + `RoleBinding` per component, in `thelb` | Reading theLB's own configuration. |
| `ConfigMap` `thelb-config` | Pools and settings for the provider. |
| `PriorityClass` `thelb-node-critical` | The agent is not evicted before the workloads it carries traffic for. |
| `DaemonSet` `thelb-agent` | One agent per node: host network, privileged, `/lib/modules` read-only, tolerates every taint. |
| `Secret` `thelb-director-token` (type `kubernetes.io/service-account-token`) | The directors' credential, and the kubeconfig built from it, printed for the user to install on the directors. |

**RBAC, the least each component needs:**

| Component | API group | Resource | Verbs |
|---|---|---|---|
| director | core | `services` | `get`, `list`, `watch` |
| director | core | `services/status` | `update`, `patch` |
| director | `discovery.k8s.io` | `endpointslices` | `get`, `list`, `watch` |
| director | core | `nodes` | `get`, `list`, `watch` |
| director | `events.k8s.io` | `events` | `create`, `patch` |
| director, in `thelb` | core | `configmaps` | `get`, `list`, `watch` |
| agent | core | `nodes` | `get`, `list`, `watch` |
| agent | core | `services` | `get`, `list`, `watch` |
| agent | `events.k8s.io` | `events` | `create`, `patch` |
| agent, in `thelb` | core | `configmaps` | `get`, `list`, `watch` |

## Command line and configuration

| Command | Does |
|---|---|
| `thelb run` | The daemon, in the roles its configuration gives it. |
| `thelb plan` | Prints every change `run` would make on this node, and changes nothing. |
| `thelb doctor` | Checks this node: kernel and modules, capabilities, kernel parameters (and whether the system's configuration would keep them after a reboot), interfaces, other software in the way (keepalived, another IPVS user, kube-proxy's mode and `strictARP`), the platform (clouds where VIPs cannot move), the cluster's voter count. Explains each finding; changes nothing. |
| `thelb status` | Who holds which VIP; each backend's health; IPVS counters; the cluster's members. |
| `thelb generate systemd\|sysctl\|modules\|k8s\|certs` | Prints file content; writes nothing. See below. |
| `thelb version` | Version, commit, Go version and platform. |

Configuration is one YAML file that the user writes (theLB never writes it), with command-line flags for a quick single-node setup:

```yaml
node:
  name: lb-1
  roles: [director]
cluster:
  listen: "[fd00::11]:7946"
  voters: ["lb-1=[fd00::11]:7946", "lb-2=[fd00::12]:7946", "lb-3=[fd00::13]:7946"]
  tls: { ca: /etc/thelb/ca.pem, cert: /etc/thelb/lb-1.pem, key: /etc/thelb/lb-1-key.pem }
vips:
  - address: 192.0.2.10
    interface: eth0          # optional: found from the routing table otherwise
    services:
      - { protocol: tcp, port: 443, scheduler: mh, backends: [10.0.0.21, 10.0.0.22], check: { https: /healthz } }
  - address: 2001:db8::10
    services:
      - { protocol: tcp, port: 443, backends: ["2001:db8::21", "2001:db8::22"], check: { tcp: {} } }
```

## What `thelb generate` prints

Nothing here is written by theLB. The user decides whether to install it, and where.

| Command | Prints |
|---|---|
| `generate systemd` | A unit file: `Type=notify`, `WatchdogSec=`, `Restart=always`, the capabilities it needs as `AmbientCapabilities=` and `CapabilityBoundingSet=`, `NoNewPrivileges=yes` and the other hardening settings that do not stop it working - but not `ProtectKernelTunables=`, which would make `/proc/sys` read-only. |
| `generate sysctl` | A `sysctl.d` file with the parameters this node's roles need, for the user who wants them set from boot. |
| `generate modules` | A `modules-load.d` file with the modules this node needs. |
| `generate k8s` | The manifests in [Kubernetes](#the-manifests), as one YAML stream. |
| `generate certs` | A certificate authority and one certificate per cluster member, for a small cluster without its own PKI. |

## Observability

- **Logs**: one line per event, saying what happened, what changed and from what - in the style of shint's log lines.
- **Metrics**: Prometheus, on a configurable address: VIP ownership, backend health, IPVS counters per service and backend (connections, packets, bytes), cluster state, failovers, drift corrections, conflicts.
- **`thelb status`** reads a local API on a Unix socket under `/run/thelb/` (runtime state, cleared at boot, like everything else theLB creates).

## Security

- **Cluster traffic**: mutual TLS, always; a member without a certificate the cluster's CA signed cannot join.
- **Privileges**: capabilities, not root, under systemd. In Kubernetes the agent must be privileged (kernel parameters are writable only so); the `thelb` namespace isolates it.
- **No secrets in logs or `status`.**
- **A security policy from the first release**: supported versions, private reporting, how dependencies' fixes are taken - as shint's `SECURITY.md` does - and `govulncheck` gating every release.

## Failure modes

| Failure | What theLB does |
|---|---|
| The director holding the VIPs dies | The others elect a leader within the election timeout; it adds the VIPs, announces them, programs IPVS. Flows continue where `sloppy_tcp` and Maglev allow. |
| Network partition | The side with the majority keeps or takes the VIPs; the other side gives them up when its lease expires. With no majority anywhere, no one holds them. |
| A backend fails | Weight 0 after the failure threshold; back when healthy. |
| Every backend of a service fails | The service stays, reported unhealthy. |
| Someone flushes IPVS or removes a VIP | The watchdog loop puts it back within its interval, and logs it. |
| theLB crashes | The VIPs stay on the interface until it restarts; the cluster's lease expiry makes another director take over meanwhile; on restart, theLB adopts or removes what it finds. |
| The process hangs | Under systemd, the watchdog restarts it; its lease expires either way. |
| Another system claims a VIP | Logged and counted as a conflict; theLB does not fight over it. |
| The Kubernetes API is unreachable | Current VIPs and backends keep working; no new allocations until it is back. |
| A kernel module is missing and cannot be loaded | Preflight fails; theLB does not start. |
| A switch ignores gratuitous ARP | The announcements repeat; `doctor` can test a link's behaviour. |

## Testing

- **Unit tests** for everything that is not the kernel: planning, the Raft state machine, VIP assignment, configuration, health-check logic.
- **A network lab in network namespaces**: a bridge, veth pairs, a client, three directors and several backends, IPv4 and IPv6 - a real L2 cluster on one machine, in CI too (GitHub's runners allow `sudo`).
- **Chaos tests in the lab**: kill the leader, partition the directors, flap a backend, flush IPVS, remove a VIP; measure failover time and how many flows survive.
- **Kubernetes** in [kind](https://kind.sigs.k8s.io/), with kube-proxy in each of its modes.
- **Kernels**: the lab under QEMU on each supported kernel series.
- **`make check`** is the one command to run them all, as in shint.

## Project, tooling and releases

Like shint, simpler:

- **One repository**, `github.com/dmartsapp/thelb`, MIT. Go, one static binary (`CGO_ENABLED=0`), Linux only (`amd64`, `arm64`).
- **`Makefile` is the single entry point** for building and checking: `make check` (format, vet, tests, lint, `govulncheck`, docs), `make lab` (the network lab, needs root).
- **Branches**: `main` is development. Each minor release gets a branch, `release/vX.Y`, cut when the release is ready; its releases are tagged there (`vX.Y.0`, then patches `vX.Y.1`...), and fixes are cherry-picked onto it from `main`. No fast-forwarding `main`.
- **Release workflows run only for a tag `vX.Y.Z` on `release/vX.Y`**: binaries, SHA-256 checksums, signed build attestations, a container image; `govulncheck` must pass.
- **Changelog** in `CHANGELOG.md`; **documentation** in Markdown under `docs/`, published as a site later.
- **Issues and milestones** on GitHub, one milestone per release.

## Phases

| Phase | Delivers |
|---|---|
| 0 - spike | On one machine, by hand-written code: an IPVS direct-routing service over netlink, a VIP added and announced, backends prepared - IPv4 and IPv6. Settles the kernel questions: IPv6 neighbour solicitations for a VIP on `thelb0`, `IFA_PROTO`, Maglev behaviour on failover. |
| 1 - one director | `run`, `plan`, `doctor`, `status`, `generate`; director and backend roles; the watchdog loop; health checks; IPv4 and IPv6; the network lab in CI. |
| 2 - the cluster | Raft among directors, agents as non-voters; failover; fail closed; leadership hand-off on stop; chaos tests. |
| 3 - Kubernetes | The provider; the agent DaemonSet; `generate k8s`; kind tests. |
| 4 - active-active | VIPs spread across directors; optional IPVS connection synchronisation. |
| 5 - proxy | A proxying mode, as a separate capability, for networks where direct routing is not possible. |

## Open questions

1. **Raft's state.** Raft needs its log and its vote to survive a restart, or a restarted voter could vote twice in one term. Keeping them on disk is a persistent entry theLB would make on its own. Proposal: keep them **in memory by default**, and give every process start a **new member identity** - a restarted director joins as a new member, catches up, and is promoted to voter, and the leader removes its old identity - so no safety rule is broken. The desired services come from the user's configuration or Kubernetes, so nothing that matters is lost; a cluster whose directors all restart at once forms again from the configured voters (`bootstrap-expect`, as Consul does). A `--data-dir` would be the opt-in for keeping Raft's state on disk.
2. **Minimum kernel.** 6.1 gives `IFA_PROTO` for marking addresses and covers Debian 12, Ubuntu 22.04 (HWE) and 24.04, and every current Kubernetes node image; it leaves out RHEL 9 (5.14), which would need address marking by label only (IPv4) and in-memory tracking (IPv6).
3. **The `loadBalancerClass` name**, and the prefix for theLB's annotations (pool, requested address): `dmarts.app/thelb`, or a domain of theLB's own.
4. **Directors inside the cluster** - on dedicated nodes without kube-proxy, as a second DaemonSet with a node selector - or only outside it, as above.
5. **Cluster membership security**: certificates the user provides (with `generate certs` for small clusters), or a join token theLB exchanges for a certificate.
6. **SCTP**, which IPVS supports: in scope, or later.
7. **Where Kubernetes pools are defined**: the `thelb-config` ConfigMap for a start; a custom resource later, if pools need their own lifecycle.
