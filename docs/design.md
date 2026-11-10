# Design

How theLB works, and why. The principles behind every choice here are in [Philosophy](philosophy.md); where this document and the philosophy disagree, the philosophy wins and this document is wrong.

Status: **design**, before any code. Decisions still open are listed at the end, in [Open questions](#open-questions). The [diagrams](diagrams.md) show the pieces, starting a director, joining and approval, a backend's path to traffic, and a packet's.

## Contents

1. [What theLB does](#what-thelb-does)
2. [Words](#words)
3. [Where it works](#where-it-works)
4. [Roles](#roles)
5. [The data path](#the-data-path)
6. [Kernel state theLB manages](#kernel-state-thelb-manages)
7. [Startup, the watchdog loop, and stopping](#startup-the-watchdog-loop-and-stopping)
8. [VIPs and announcing them](#vips-and-announcing-them)
9. [Bootstrapping and joining directors](#bootstrapping-and-joining-directors)
10. [Backends: enrolment, trust and services](#backends-enrolment-trust-and-services)
11. [The cluster, and what Raft carries](#the-cluster-and-what-raft-carries)
12. [Health checks](#health-checks)
13. [Kubernetes](#kubernetes)
14. [Address pools](#address-pools)
15. [Command line and configuration](#command-line-and-configuration)
16. [What `thelb generate` prints](#what-thelb-generate-prints)
17. [Observability](#observability)
18. [Security](#security)
19. [Failure modes](#failure-modes)
20. [Testing](#testing)
21. [Project, tooling and releases](#project-tooling-and-releases)
22. [Phases](#phases)
23. [Open questions](#open-questions)

## What theLB does

- **L4 load balancing** of TCP, UDP and SCTP to a virtual IP (VIP), IPv4 and IPv6, by the kernel's IPVS with **direct return**: replies go from the backends straight to the clients. Direct routing first; IP-in-IP tunnelling, which keeps direct return across subnets, next.
- **VIP ownership on the network**: it adds the VIP to the right interface and announces it on the L2 network (gratuitous ARP for IPv4, unsolicited Neighbor Advertisements for IPv6).
- **High availability**: a cluster of directors agrees, through Raft, which one holds each VIP; when it fails, another takes the VIP over.
- **Backends that prove who they are**: a backend enrols with the cluster, gets a certificate, and is sent traffic only for the services it is allowed to serve, only while it keeps proving it is there.
- **Both sides of direct return**: the same binary configures the backends, which hold the VIP without announcing it, and can map a service's port to the port their application really uses.
- **A Kubernetes `LoadBalancer` provider** (Kubernetes 1.35.0 or later): VIPs for Services of type `LoadBalancer`, with the cluster's nodes as backends.
- **Explanations and file contents, never files**: `status` for the whole cluster, `doctor` for a node and what theLB would change on it, and `generate` for systemd units, sysctl configuration, Kubernetes manifests, secrets and the running configuration.

Not in scope, for now: proxying (a later capability - see [Phases](#phases)), NAT on the director, L7, routed announcement (BGP), and cloud platforms' load-balancer APIs.

## Words

| Word | Meaning |
|---|---|
| **VIP** | The virtual IP clients connect to. One VIP can carry several services, each served by its own backends. |
| **Service** | A VIP, a protocol (TCP, UDP or SCTP) and a port, with its backends. In IPVS terms, a virtual service. |
| **Director** | A node that can hold VIPs and forwards their traffic with IPVS. Directors form the Raft cluster. |
| **Leader** | The director Raft has elected. It holds the control VIP, signs certificates, and decides; the others follow. |
| **Control VIP** | The cluster's own VIP, held by the leader: the address backends and the command line talk to, on the control port. |
| **Backend** | A node that serves a service's traffic: it holds the VIP without announcing it, and answers clients directly. IPVS calls it a real server. |
| **Agent** | theLB running on a backend - under systemd, on a Kubernetes node as on any other machine. |
| **Cluster secrets** | Two random secrets - one for directors, one for backends - created by the director that first bootstraps the cluster, in the files the user names, and copied by the user to every node of that role. A node joins by proving it knows its role's secret. |
| **Pool** | Service VIPs a director registered: a first address in CIDR notation, and how many addresses follow it. The control VIP is in no pool. |
| **Offer** | What a backend registers: one of its applications, under a name the backend chooses (`shop`), the protocol and port it listens on (`tcp/8080`), and how to check it. Made when the backend starts or while it runs, and recorded only once the leader's check of it passes. No service and no VIP: the same application may listen on 8080 on one node and on 9090 on another. |
| **Assignment** | An offer given to a service by a director: the service's name, VIP and port, and the backend's weight. |

## Where it works

Direct return has hard requirements, and theLB checks them rather than hoping:

- **Directors and backends share an L2 network** (one broadcast domain, per address family), for direct routing: the director forwards a packet by sending it to the backend's MAC address. Tunnelling, the second forwarding method, lifts this - see [The data path](#the-data-path).
- **The director does not change the port.** A backend receives the packet as the client sent it, with the service's port. A backend whose application uses another port has theLB map it locally - see [Port mapping on the backend](#port-mapping-on-the-backend).
- **Backends must accept traffic to the VIP and must not announce it** - see [Kernel state](#kernel-state-thelb-manages).
- **Backends' replies must be let out with the VIP as their source.** A backend answers from the VIP through its own gateway; a router or firewall on that way that filters by source address drops the replies. An assignment checks this before any traffic is sent ([Offers, services and assignments](#offers-services-and-assignments)). A stateful firewall that the replies pass and the requests do not would drop them too, and that cannot be checked from either end: direct return needs a network without one.
- **The network must let a VIP move between machines.** On-premises switches and most hypervisors do (some need settings such as accepting forged transmits); public clouds generally do not. `thelb doctor` detects the major clouds and says so.
- **Linux only**, with IPVS in the kernel: **kernel 5.14 or later**, so that RHEL 9 and its rebuilds (Rocky, AlmaLinux), still in extended support, are covered - see [Kernels](#kernels).

### Kernels

theLB supports every kernel from 5.14, the kernel of RHEL 9. It does not decide what a kernel can do from its version number - distribution kernels, RHEL's above all, carry features backported from much newer kernels - but **probes each feature it needs** at startup and adapts:

| Feature | When the kernel has it | When it does not |
|---|---|---|
| Address protocol (`IFA_PROTO`, upstream since 6.1) | theLB's addresses carry theLB's own protocol value | IPv4 addresses are recognised by their label alone, IPv6 addresses by matching a configured VIP (see [Telling what theLB owns](#telling-what-thelb-owns)) |
| The Maglev scheduler (`ip_vs_mh`, upstream since 4.18) | an option for a service | not offered; weighted round robin, the default, is in every kernel |
| IPv6 | IPv6 VIPs | IPv6 VIPs refused at preflight, with the reason |
| nftables (for port mapping) | port mapping on backends | port mapping refused at preflight; services whose ports match still work |
| SCTP in IPVS (`CONFIG_IP_VS_PROTO_SCTP`), and the `sctp` module for health checks | SCTP services | SCTP services refused at preflight, with the reason; TCP and UDP services still work |
| Any other module or parameter theLB needs | used | preflight fails, naming it |

`thelb doctor` and the startup log print the kernel's version and every adaptation in effect.

On RHEL 9 and its rebuilds, `doctor` also reports two things theLB does not manage: **firewalld** (on by default there) must allow the VIPs' ports on the directors and the control port, because the firewall sees a packet before IPVS does - `doctor` prints the `firewall-cmd` commands for the user to run; and **SELinux**'s mode.

## Roles

Every node runs the same binary, as `thelb director` or `thelb backend`:

| Role | Does |
|---|---|
| `director` | A Raft member. The leader holds the control VIP. A director holding a service VIP adds it to the interface, announces it, programs IPVS and health-checks the backends; a director holding nothing announces nothing. |
| `backend` | **Not** a Raft member: a client of the cluster, through the control VIP. It enrols, offers what it serves, holds the VIPs of the services its offers are assigned to on its own dummy interface without announcing them, maps ports where asked, checks its applications locally, and keeps reporting. |

**A node can run both**, for small setups - decided: **two daemons**, as on any two machines. The director starts first, knowing nothing of what the host will serve; the backend is started later, with the command any backend uses - `thelb backend --join <control VIP:port> --secret-file ... --offer tcp/80` - and its own systemd unit, its own watchdog, its own socket. Neither runs inside the other. How it works:

- **One theLB per role per node**: each role has its own socket and lock, in `/run/thelb/director/` and `/run/thelb/backend/`; a second director, or a second backend, on one node still stops as already running. `thelb status` there asks the director; `thelb status --role backend` asks the backend; `thelb admin` asks the director.
- **Two names**: the backend needs a name of its own (`--name`) - a backend with its director's name is refused, and told so. The two roles share the machine's node fingerprint; the clone check refuses two live nodes with one fingerprint only within a role. Each is approved, and has its own certificate.
- **When it leads**, IPVS sees that one destination is a local address and delivers that share of the traffic to the node's own stack (IPVS's local-node forwarding) - the VIP is on the director's interface, so the application, listening on the VIP's port, gets it; the other backends are sent theirs by direct routing.
- **When it does not lead**, it is a backend like any other: the VIP on `thelb0`, `arp_ignore` and `arp_announce` set (which leave the director's own VIPs, on the receiving interface, answered), traffic sent to it by the leader.
- **Its own offers use the service's port**: a port-mapping rule at prerouting would also catch the clients' packets while it leads - before IPVS - so an offer from a director's address is assigned only on the service's port; other backends may still map.
- **Nothing pinned for itself**: the leader pins backends' MAC addresses, but not its own address - its own share never leaves the machine.
- `thelb status` there is the director's view, in which the machine appears twice: as a director, and as its backend. On a Kubernetes node, the backend role mostly checks rather than configures: kube-proxy already accepts traffic for a Service's load-balancer IP (see [Kubernetes](#kubernetes)).

## The data path

```text
client ──▶ switch ──▶ director (holds VIP, IPVS: rewrite dst MAC) ──▶ backend (VIP on thelb0)
   ▲                                                                       │
   └──────────────────────── reply, from the VIP, straight to the client ◀─┘
```

1. The client sends to `VIP:port`. The switch delivers it to the director, which answers ARP/NDP for the VIP.
2. IPVS on the director picks a backend and sends the **unchanged** packet to that backend's MAC address.
3. The backend holds the VIP locally, so its kernel accepts the packet and the service answers - from the VIP, to the client, directly.

theLB programs IPVS over its netlink interface (no `ipvsadm`):

- **One IPVS service per VIP, protocol and port**, each with its own backends: one VIP can carry, say, tcp/443 to the web nodes, tcp/5432 to the database nodes and udp/53 to a DNS node. Every backend of every service holds the VIP, but only receives its own service's connections. One VIP, protocol and port belongs to one service; a second claim on it is refused and reported.
- **Scheduler**: **weighted round robin** (`wrr`) by default: new connections go to backends in proportion to their weights - set per backend when it is assigned, `1` if not; `0` drains it. Maglev hashing (`mh`), least connections and the others are options per service.
- **Unhealthy backend**: weight 0 - no new connections, existing ones finish - not removed. A backend that detaches is removed, and its connections expire (`expire_nodest_conn`).
- **Failover without losing connections**: weighted round robin does not send a connection to the same backend on every director, so a connection survives a failover only if the new director already knows where it goes. **IPVS's connection synchronisation is on by default**: the director holding the VIPs runs the kernel's sync daemon as master, every member one as backup, sending connection entries by multicast (`224.0.0.81`, UDP `8848`) on the control VIP's interface, and the roles follow leadership. The sync ID is the cluster's - 1 to 255, from a hash of its ID, never 0, with which a backup would take in every cluster's connections. A synchronised entry needs no destination on the backup: direct routing and IP-in-IP send by the entry's own backend address, so the new leader carries on before it has programmed its services. The messages are not authenticated: the directors' network must be trusted. `sloppy_tcp` and `sloppy_sctp` let the new director carry on with what it was sent. Where multicast does not pass between directors, `doctor` says so, and connections in progress are lost on a failover - new ones are not; a service using Maglev hashing loses fewer of them without the sync.
- **What the directors share of weighted round robin**, so that the director taking over carries on where the last one left off:
  - **the weights** - and every backend's state, healthy or drained - are in Raft, so every director programs the same IPVS services with the same weights;
  - **where each connection goes** - IPVS's connection table, the part a failover must not lose - is copied to the standby directors as it changes, by the kernel's sync daemon;
  - **the turn** - which backend is next in the rotation - is not shared: it lives inside the kernel's scheduler, which no interface exposes, and it does not need to be: the new director starts a fresh rotation with the same weights, so new connections are spread in the same proportions, and connections already open follow the copied table.
- **Session persistence** (one client to one backend for a time) is a per-service option; its templates are part of the connection table, so they are copied too.
- **SCTP** is balanced per association, like a TCP connection. Multi-homing - one association over several addresses - is not carried: on theLB's side, the VIP is the association's only address.

### Forwarding methods

A service's forwarding method is a per-service setting:

| Method | How | When |
|---|---|---|
| **Direct routing** (first) | The director rewrites the destination MAC address. | Directors and backends on one L2 network: the fastest, and no encapsulation. |
| **IP-in-IP tunnelling** (`--method tunnel`, IPv4 in this build) | The director wraps the packet in an outer IP header to the backend's address; the backend unwraps it on theLB's own IPIP interface, `thelbt0`, which takes packets from any director, and finds the VIP on `thelb0`. | Backends in other networks, still with direct return - the director needs only a route to them, and a backend may join a control VIP it reaches through a router. The outer header costs 20 bytes, so the backend clamps the MSS its handshakes announce, in theLB's nftables table; reverse-path filtering is off on `thelbt0`, and loose rather than strict for all interfaces if it was strict. The backend's network must let its replies, from the VIP's address, through: a router that checks source addresses drops them. |

Neither changes the port, and neither lets the director see the replies: no L7, no TLS termination, and IPVS's connection state is kept from one direction only (its timeouts are tuned for that). The proxy, a later capability, is for what needs more.

### How a director takes a port

Nothing on the director listens on a VIP's port: no process opens a socket there. Once an IPVS service exists for a VIP, a protocol and a port, and the VIP is one of the director's addresses, the kernel's IPVS takes every packet for it as it arrives for local delivery, and forwards it; the director's own TCP, UDP and SCTP never see it.

A packet for a port of the VIP that has no IPVS service goes to the director's own stack, as a packet to any of its addresses does - refused, unless something on the director listens on that port **on every address**: an SSH server on `0.0.0.0:22` would answer on every VIP the director holds. `doctor` lists such listeners on directors; the fix is to bind them to the director's own addresses, or a firewall rule - the user's, not theLB's.

### Many interfaces, one daemon

One theLB serves VIPs on any of a director's interfaces at once - external VIPs in `10.10.1.0/24` on `eth0`, internal ones in `10.0.0.0/24` on `eth1`. Each VIP is added and announced on its pool's interface only; IPVS matches a packet by VIP, protocol and port, whichever interface it came in on; and a packet for a backend leaves through whichever interface reaches that backend directly. So a VIP on `eth0` may be served by backends on `eth1`'s network: they answer from the VIP through their own gateway - which is why an assignment checks [the return path](#offers-services-and-assignments).

### Port mapping on the backend

The director cannot change the port, so a backend whose application listens elsewhere - `VIP:443` served by an application on `8443` - maps it itself. When a backend's offer is assigned to a service whose port differs from the application's, its agent adds one rule to **theLB's own nftables table** (`inet thelb`): packets to `VIP:443` go to port `8443`. The kernel's connection tracking turns the replies back into `VIP:443`, so the client sees the service's port. The mapping is per VIP, protocol and port, so two VIPs can both offer 443 and lead to different applications. It is runtime state like the rest (see [Kernel state](#kernel-state-thelb-manages)); with matching ports there is no rule at all.

It works the same for TCP, UDP and SCTP. Kubernetes Services already map ports, with `targetPort`; this is for plain Linux backends.

**Built on nftables, not eBPF**: connection tracking turns the replies back for every connection, recomputes SCTP's CRC32c, translates the headers inside ICMP errors - path MTU discovery keeps working - and reassembles fragments, whose later parts carry no ports. A tc eBPF program would be faster per packet, but would have to do all of that itself, and share its hook with Cilium or Calico on RHEL 9's kernel, which has no multi-program tc attachment yet; the saving would be on the backend, beside the application's own cost - the director's path is IPVS either way. theLB writes the table over netlink, without the `nft` command, and replaces it whole, in one transaction, when a mapping changes: `nft list table inet thelb` shows it (`ip daddr 10.10.1.20 tcp dport 80 dnat ip to :8080`). The port a firewall must open on the backend is the application's: destination NAT, at priority `dstnat`, comes before the input filter.

## Kernel state theLB manages

Everything here is **runtime state**. theLB sets it at runtime - kernel parameters by writing `/proc/sys`, which is what the `sysctl` command does (the old `sysctl(2)` system call was removed from Linux in 5.5) - and never writes a configuration file. What is already right is left alone and never reverted; what theLB changes is recorded and restored when it stops cleanly ([Philosophy](philosophy.md#2-the-running-kernel-is-the-truth)).

### Director

| State | Value | Why |
|---|---|---|
| Kernel modules | `ip_vs`, and the schedulers in use (`ip_vs_wrr` by default) | IPVS itself. Loaded only if missing; needs `CAP_SYS_MODULE` only then. |
| `net.ipv4.vs.sloppy_tcp` | `1` | A director that takes over accepts flows already in progress. |
| `net.ipv4.vs.sloppy_sctp` | `1` | The same for SCTP associations - set only when there are SCTP services. |
| `net.ipv4.vs.expire_nodest_conn` | `1` | Connections to a removed backend end at once instead of hanging. |
| `net.ipv4.vs.expire_quiescent_template` | `1` | Persistence templates of a weight-0 backend expire, so its clients move on. |
| VIP addresses | on the chosen interface, `/32` or `/128` | The control VIP while this director leads; a service VIP while it holds it. |
| IPVS services | one per VIP, protocol and port | Only while this director holds the VIP. |
| IPVS connection synchronisation | the kernel's sync daemon: master where the VIPs are held, backup on the other directors | Connections in progress survive a failover (see [The data path](#the-data-path)). |
| Neighbour entries | each backend's address to its enrolled MAC address, permanent | Pins backends: nothing on the network can redirect a backend's traffic by answering ARP or NDP for its address (see [Backends](#backends-enrolment-trust-and-services)). |

The `net.ipv4.vs.*` parameters apply to IPv6 services too; IPVS keeps them all under `net.ipv4`. Direct routing needs no IP forwarding (`ip_forward`) on the director: IPVS transmits the packets itself.

### Backend (plain Linux)

| State | Value | Why |
|---|---|---|
| Dummy interface `thelb0` | created by theLB, up, no ARP | Holds the VIPs. theLB's own interface, so it never changes `lo`, and removing it removes everything theLB added. |
| VIP addresses on `thelb0` | `/32`, `/128` (IPv6 without duplicate address detection) | The kernel accepts the packets the director forwards. |
| `net.ipv4.conf.all.arp_ignore` | `1` | Answer ARP only for addresses on the interface the request came in on - never for the VIP on `thelb0`. |
| `net.ipv4.conf.all.arp_announce` | `2` | Never use the VIP as the source of the backend's own ARP requests. |
| nftables table `inet thelb` | one rule per mapped port | Only with [port mapping](#port-mapping-on-the-backend); never created otherwise. |
| Tunnel interface `thelbt0` | IPIP, from any sender (collect-metadata mode), up | Only while a tunnelled service is assigned to the backend; its own `rp_filter` 0, and `conf/all/rp_filter` 2 if it was 1 (recorded and restored). |
| MSS clamp | a rule in `inet thelb`, chain `mss`, at output: `ip saddr <vip> tcp flags syn tcp option maxseg size > N tcp option maxseg size set N` | For each tunnelled VIP: N is the interface's MTU less 60. |

**IPv6** has no `arp_ignore`. Linux answers neighbour solicitations only for addresses on the receiving interface, so a VIP on `thelb0` is not answered on `eth0`; phase 0 must prove this on every kernel theLB supports, before anything relies on it.

### Telling what theLB owns

theLB keeps nothing on disk, so after a restart it recognises its own state from the kernel:

- **Addresses** carry theLB's own address protocol value - 116, which `ip address` shows as `proto 0x74` - (`IFA_PROTO`, the "who added this address" field upstream Linux has since 6.1) where the kernel has it, and IPv4 addresses always a label (`eth0:thelb`). On a kernel without `IFA_PROTO` (RHEL 9's), an IPv6 address - which has no label - is theLB's when it is a configured VIP: a VIP in theLB's configuration is theLB's by definition, whoever added it.
- **IPVS services** are theLB's when they match a configured service; any other IPVS service is left alone, and `doctor` reports it.
- **`thelb0`, the `inet thelb` table and theLB's tunnel interfaces** are theLB's by name.
- **Kernel parameters** cannot be marked. After a crash theLB cannot know a parameter's value from before it started, so it treats the current value as the one to leave; this is logged.

## Startup, the watchdog loop, and stopping

```text
start ─▶ preflight ─▶ read the kernel ─▶ plan ─▶ apply ─▶ bootstrap or join ─▶ watchdog loop ─▶ stop
           │                               │
           └─ missing authority: exit 1    └─ `thelb doctor` prints this, and exits
```

1. **Preflight - full authority or nothing.** Effective capabilities (`CAP_NET_ADMIN`, `CAP_NET_RAW`; `CAP_SYS_MODULE` if a module is missing); `/proc/sys/net` writable; IPVS present (netlink family, schedulers); nftables if a port mapping is needed; IPv6 enabled if any VIP is IPv6; the interface exists; on a director serving Kubernetes, the API server reachable and every permission held (checked with `SelfSubjectAccessReview`). Anything missing: theLB changes nothing, exits `1`, and prints what is missing and how to grant it.
2. **Read the kernel**: every parameter, address, interface, table and IPVS entry it cares about.
3. **Plan**: the difference between what it needs and what is there.
4. **Apply**: kernel parameters and modules now; VIPs and IPVS services only when the cluster gives this node a VIP. Each change is logged with its old value.
5. **Bootstrap or join** - see [Bootstrapping and joining directors](#bootstrapping-and-joining-directors) and [Backends](#backends-enrolment-trust-and-services).
6. **Watchdog loop**: a periodic full comparison (every 10 s) - netlink notifications (addresses, links, neighbours), to react sooner, come later. Anything that drifted - an address removed, `ipvsadm -C`, a pinned neighbour entry changed, a parameter changed, a backend's table or interface deleted - is put back and logged (`WARN drift repaired: ...`). Between comparisons the leader applies only the difference between its new plan and the last one, and nothing while they are the same; a failed apply makes the next one a full comparison. Under systemd, theLB reports to the service manager's watchdog only while its loops go round - each loop says when it is due again, and one 30 s overdue stops the reports - so a hung process is restarted. **Built** (the agreed order's step 3): on the leader, IPVS services, destinations and weights, the service VIPs, the control VIP, pinned neighbours and the IPVS parameters; on a backend, `thelb0` and its VIPs, the nftables table (by its number of rules), `thelbt0` and the parameters.
7. **Stop** (`SIGTERM`, `SIGINT`, or `SIGHUP` - what a closed terminal sends; there is no configuration to reload): a leader hands leadership to another director first (so a planned restart is a short, clean failover); then theLB gives up its VIPs, removes its IPVS services, addresses, neighbour entries, table and interfaces, and restores the parameters it changed. After a crash nothing is cleaned up; the next start finds the state in the kernel and adopts or removes it.

## VIPs and announcing them

- **Interface**: for the control VIP, the director's interface with an address in its network; for a service VIP, its pool's interface (see [Address pools](#address-pools)).
- **Adding**: `/32` or `/128`, with theLB's address protocol and label. IPv6 VIPs skip duplicate address detection, so a takeover is immediate; the cluster, not DAD, prevents duplicates.
- **Announcing**, when a director takes a VIP: gratuitous ARP (request and reply forms) for IPv4, unsolicited Neighbor Advertisements with the override flag to all-nodes for IPv6 - three of each over two seconds, because switches and hosts miss single packets. A periodic re-announcement is an option, off by default.
- **Probing before taking a VIP that is new to the network** - the control VIP at bootstrap, a newly allocated service VIP: an ARP probe (RFC 5227, from `0.0.0.0`) for IPv4, a neighbour solicitation for IPv6. An answer means someone else has it, and theLB refuses. For the control VIP, theLB also connects to `VIP:port` and starts a join's TLS handshake: a cluster answers that, and nothing else does. A probe that cannot be made - the link is down, say - is not taken for silence: a bootstrap needs a whole window of probes that worked and found nothing.
- **Taking it**: the listener first - bound with `IP_FREEBIND` before the address exists - then the address, then the announcements, so no connection to the VIP is refused while it moves; **giving it up**: the address first, then the listener.
- **Conflict detection**: a director listens for ARP and NDP traffic claiming one of its VIPs from another MAC address. That means another system holds it - a second cluster, a forgotten static address - and is logged as an error and counted in the metrics. theLB does not fight over it; for the control VIP, see [the bootstrap rule](#restarts-and-the-bootstrap-rule).
- **Giving up**: the address is removed. Nothing is announced; the next director's announcement moves the clients.

## Bootstrapping and joining directors

Every director is started the same way - the control VIP, the control port and the two secret files, on the command line or in its systemd unit - with `--bootstrap` on the director that starts the cluster (and on others too, if wanted - see [the bootstrap rule](#restarts-and-the-bootstrap-rule)).

```text
# the first director: checks the VIP and port are free, creates the cluster and - this first time - its secrets
sudo thelb director --vip 10.0.0.10 --port 7443 --bootstrap \
  --secret-file /etc/thelb/director.secret --backend-secret-file /etc/thelb/backend.secret

# the user copies director.secret and backend.secret to every other director, and backend.secret to every
# backend; then every other director runs the same command (with or without --bootstrap)
sudo thelb director --vip 10.0.0.10 --port 7443 \
  --secret-file /etc/thelb/director.secret --backend-secret-file /etc/thelb/backend.secret
```

### The secret files

A director decides what to do from its secret file and from what answers on the control VIP:

| The secret file | `VIP:port` | The director |
|---|---|---|
| exists | a theLB cluster answers | **joins** it with the secret; if the cluster does not accept it, stops - see below |
| exists | nothing answers (with `--bootstrap`) | **creates the cluster** with this secret |
| exists | nothing answers (without `--bootstrap`) | waits, and retries, until a cluster answers |
| missing | nothing answers (with `--bootstrap`) | **creates the secrets**, writes them to the files, and creates the cluster |
| missing | anything else | stops: it has no secret, and says where to get one |
| either | something that is not theLB answers | refuses: the VIP or port is taken |

**Writing a secret file** is the one thing theLB writes to disk ([Philosophy](philosophy.md#1-one-binary-and-nothing-left-behind)), and it does so only then: the first time a cluster is bootstrapped, only to the files the user named, only when they do not exist, created readable by root only (mode `0600`), with an exclusive create, and never overwritten. The secrets go nowhere else - not to the log, not to standard output. A user who wants to create them beforehand can: `thelb generate secret` prints one.

**A secret the cluster does not accept** means the file is out of date - the cluster's secret was rotated, or recreated - or belongs to another cluster; from outside, the two look the same, and theLB says nothing more about the cluster:

```text
thelb: the cluster at 10.0.0.10:7443 did not accept the secret in /etc/thelb/director.secret: it is out of
date, or belongs to another cluster. On a director, run `sudo thelb admin secret show --role director`, put
what it prints in /etc/thelb/director.secret, and start this director again.
```

theLB reads the secret only from `--secret-file`; it never takes one on the command line, where other users could see it in the process list.

### Creating the cluster

A director with `--bootstrap`, when nothing answers:

1. Checks that the control VIP is free on the network (an ARP or NDP probe) and that nothing answers on `VIP:port`.
2. Creates the secrets if their files are missing, as above.
3. Creates the cluster: a cluster ID, a **certificate authority** (key and certificate) and its own certificate - all in memory.
4. Becomes a Raft cluster of one, and leader; takes the control VIP, announces it, and opens the control API on it, with mutual TLS once members have certificates.
5. Logs that the cluster exists - and nothing secret. **The CA's key is never printed or written**: under systemd, standard output goes to the journal, which is on disk.

### Joining it

Every other director, and a bootstrap director that finds a cluster already running:

1. Creates its key in memory and opens a TLS connection to `VIP:port`.
2. **Both sides prove they know the directors' secret**: each sends an HMAC, keyed with the secret, over the TLS connection's exported keying material. The proof is bound to that one connection, so a machine in the middle cannot relay it, and the secret itself never crosses the network. The joiner now knows it reached the real cluster, and the cluster knows the joiner holds the secret.
3. The joiner trusts the CA it received over that connection, and sends a certificate signing request with its name and addresses.
4. The leader checks that the connection comes from one of the addresses claimed, and that the name is not held by a live member. The request then waits, **pending**, until a director approves it - see [Approval](#approval).
5. Once approved, the leader signs its certificate and adds it to Raft as a non-voter. It receives the replicated state, **the CA included**, and is promoted to voter once it has caught up.

### Approval

**Every node that joins waits for a director's approval** - a director and a backend alike. A valid secret only lets a node ask: a secret that leaked lets someone knock, not come in. The one node that is never approved is the director that creates a cluster: it is the cluster.

- **Who approves**: any director that is itself an approved member - its certificate valid, not removed - with `sudo thelb admin approve <name>` (or `refuse <name>`) on that director. The local command asks its own director, which sends the approval to the leader under its own identity; the leader checks that the identity is a director's, and records who approved what, and when.
- **Several at once**: `sudo thelb admin approve --match '<regular expression>'` matches pending nodes' names in full (Go's RE2 syntax: `--match 'web-[0-9]+'`), shows the matching nodes as a table, and approves them after a `y` - or at once with `--yes`, for scripts. Without a terminal and without `--yes`, it changes nothing.
- **Backends cannot approve**: `thelb backend` has no approval command, and the leader refuses an approval that does not come from a director.
- **Where pending nodes appear**: in `thelb status` on any director - one `pending` table for both roles, with each node's name, role, address, MAC address, **node fingerprint** and how long it has waited (see [`thelb status`](#thelb-status)). `thelb status` on the pending node itself shows the same fingerprint, so the operator can compare the two before approving.
- **Approved once, rejoins by itself**: a node's keys live only in memory and change at every start, so approval is not tied to a key but to the **node** - its node fingerprint, which identifies the machine across restarts (see [The node fingerprint](#the-node-fingerprint)), with its addresses and MAC addresses. While the cluster remembers an approved node, that node restarting with the same fingerprint, the same address and MAC address on the wire, and the right secret rejoins without waiting. A changed fingerprint, address or MAC address waits for approval again; so does a node a director removed. Two live nodes with the same fingerprint - a cloned machine - are refused, and reported.
- **A pending request** lasts as long as the node keeps asking; a node that stops asking is dropped from the list after an hour. A refused node is told so, and stops.

### The node fingerprint

What a director approves, and recognises when an approved node comes back: a SHA-256 hash, shown as its first 16 hexadecimal digits, of

- **the machine's application-specific ID**: an HMAC-SHA256 keyed with `/etc/machine-id` over a fixed theLB string, as systemd recommends, so the machine ID itself never leaves the machine;
- **the SMBIOS system UUID** (`/sys/class/dmi/id/product_uuid`), where the machine has one;
- **the MAC address of the interface the node joins through**.

The hostname is shown beside it, as a label, not part of it.

Left out, for their problems: `hostid` (derived from an IP address when `/etc/hostid` is missing, and identical on clones); every MAC address (they change with every container veth, bridge, VPN and randomised Wi-Fi address); the root disk's UUID (absent in a container, changed by a reinstall, shared by every machine built from one cloud image); the hostname (neither stable nor unique).

**The fingerprint is reported by the node itself**: only its address and MAC address are checked on the wire. It recognises an approved machine; the secret and the first approval are what prove one.

**The secrets are strong**: 256 random bits, whether theLB or `generate secret` made them, and theLB refuses a shorter one - a weak secret could be guessed from a recorded proof. A secret file holds one line, `thelb1:` and the 32 bytes in base64url without padding; theLB reads it only if it is a regular file - not a symbolic link - that root owns and no one else can read.

**Why a secret, and not the CA certificate**: the CA certificate is public - the leader sends its certificate chain to anyone who connects - so having it would prove only that the joiner is talking to the real cluster, not that the cluster should trust the joiner. The secret works in both directions, and outlives any one CA: a cluster created again after a full stop has a new CA but the same secret, so no node needs anything new.

**Why the CA is replicated, and not re-created after a failover**: with the CA key on every director, a new leader carries on with the same CA - no director or backend has to be issued a new certificate at the moment a node has just failed. A director can already send all of the cluster's traffic anywhere, so a compromised director is a compromised cluster either way; the CA key on every director adds little to that.

### Restarts, and the bootstrap rule

Raft's state is in memory, so a restarted director remembers nothing of the cluster. The secret file is what lets it find its way back.

Every start is a new member identity, so the old one never votes again. **A planned stop takes it out of Raft**: the stopping director - after handing leadership over, if it led - asks the leader to remove its identity (API op `leave`), or removes it itself if it still leads, so the others keep their majority without it; two directors survive either one's planned stop. A crash leaves the identity in, counting towards the majority, until the director rejoins and the leader replaces it - or until the leader finds it **gone**: with three voters or more, a voter that has not checked in for `goneAfter` (10 s) and whose control port refuses connections, or whose address does not answer ARP or neighbour solicitation at all, is removed from Raft (not from the state: it stays approved, rejoins without approval, and shows `down`). The majority decides that, so it is safe; three directors then survive two crashes one after the other - the second leaves two voters, and the rule below. A voter that answers on the network while its port drops connections keeps its vote: it may be alive behind a partition.

**The VIPs are leased from the kernel** - the control VIP and every service VIP: the leader adds each with a lifetime (`IFA_CACHEINFO`, valid and preferred alike; IPv4 has had address lifetimes since Linux 3.9) and replaces it every third of the lifetime. A leader killed or hung stops renewing, and its own kernel removes the VIPs when the lease lapses - the state a `kill -9` used to leave answering. Until then that kernel goes on forwarding its services' traffic (IPVS is kernel state too), so the new leader waiting for the VIPs costs little: about two seconds after the lapse. The data plane renews the service VIPs from its one-second loop, every third of the lease, with one address dump to find any someone removed. The lease's length is in the replicated state (`thelb admin lease`, 6 s by default, 3 s to 1 h), so every director knows how long a stopped leader's VIP may still answer. **No director takes a control VIP another machine answers for**: a new leader - elected, or standing alone - takes it only once it is **no more**, by two looks a second apart, each finding no machine answering ARP or neighbour solicitation for it and nothing accepting a connection on its port. Each service VIP likewise: the leader's data plane asks the link about every VIP it is to take, all at once, at each step, and takes one once two looks a second or more apart found no machine answering for it. One still answering after the lease is held by hand, or by a theLB alive; it is left alone, and logged.

**Two stand alone**: the last of two, after the lease plus 2 s without a leader (8 s by default; at least `takeoverAfter`, 3 s; plus a 6 s stagger for the later member identity), finds the other gone, as above, its own gateway answering (or, with none on the interface, its link up), and then - last, so a director that decided first and claimed it is seen - the control VIP no more. A firewall that rejects the control port looks exactly like a theLB not running, so both may decide; the first claims the VIP the moment it decides, and the stagger covers the head start a leader that stepped down has over a follower in noticing there is no leader. Standing alone, a director ends its session and starts one of itself alone: the same member identity, key and certificate, and Raft seeded with a snapshot of the state it had - **its log's tail replayed into it**, entries past what it applied, which the leader may have confirmed before it went (as `RecoverCluster` does; the state records the index it last applied, so the replay starts exactly there) - a configuration of itself, and the term it had reached. Leading, it removes the other, as `admin remove` does: revoked, it waits for approval when it comes back. A director that answers on the network while its port drops connections is a partition, not a crash: neither stands alone, since the other may be leading, or about to. A director left behind - its own cluster leading on the control VIP while it has no leader - starts over, and is asked for approval.

**The leader watches the control VIP** every 5 s for another machine answering ARP or neighbour solicitation for it. A cut neither director can see - no gateway, the link up, the other silent - makes both stand alone, each on its side; when they see each other again, each finds the other answering for the VIP with its theLB alive, and the one whose member identity sorts later stands down: it starts over, waits for approval, and what changed on its side is lost. Any other holder - a machine that is no director, or one whose theLB is not running (which the lease makes rare) - is answered by announcing the VIP again, so the network's caches keep pointing at the leader.

The secret file is what lets a director find its way back:

| Situation | A director with `--bootstrap` | A director without it |
|---|---|---|
| The first start | Nothing answers: creates the secrets and the cluster | Joins it, with the copied secret |
| A restart while the cluster runs | Finds the cluster on the control VIP: **joins it** | Joins it |
| Every director stopped, then started | Nothing answers: creates a new cluster - a new CA, **the same secret** | Joins the new one, with no change to its files |
| Its secret file was lost, while the cluster runs | Stops: no secret, and a cluster answers - the user copies the file back | The same |
| Its secret file was lost, and every director stopped | Nothing answers: creates new secrets and a new cluster; every other node is then out of date until the user copies the new files | Waits for a cluster |

To keep one cluster per control VIP:

- **Only a director started with `--bootstrap` may create a cluster**, and only after probing the control VIP for longer than a failover takes and finding nothing on `VIP:port`. If a theLB cluster answers, it joins; if something else answers, it refuses.
- **A director without `--bootstrap` only ever joins**, retrying until a cluster answers.
- **Several directors may carry `--bootstrap`.** They share the secret, so after a full stop, if more than one creates a cluster at the same moment, the safety net below makes them one again - and the cluster no longer depends on one particular director coming back.
- **A cluster that sees its control VIP claimed by another cluster stands down** - the younger of the two, by creation time - releasing every VIP; its directors then join the elder cluster, with the same secret. A cluster whose secret is different cannot be joined, so its directors stop as out of date.
- **`--peers`**, optional: the other directors' addresses. A bootstrapping director asks each of them first whether it already belongs to a cluster.

### Changing a secret

`thelb admin secret show --role director|backend` prints the current secret, on a director only, for a node that is out of date. `thelb admin secret rotate --role ...` creates a new one (replicated, in memory) and prints it once; the cluster accepts the old and the new one until `thelb admin secret retire --role ...`, so the user can copy the new one into every node's file first. theLB does not rewrite the files: a node still holding the old secret after it is retired is out of date, and says so. A secret that leaked is rotated the same way, and retired at once.

## Backends: enrolment, trust and services

A backend is sent traffic only when the cluster knows who it is, where it is, that it may serve the service, and that it is still there.

### Enrolment

```text
sudo thelb backend --name web-1 --join 10.0.0.10:7443 --secret-file /etc/thelb/backend.secret
```

1. The backend creates its key in memory, opens a TLS connection to the control VIP, and proves - and checks - knowledge of the **backends' secret**, the same way a director does with its own. A backend secret can never join a node as a director.
2. It sends its name, a certificate signing request, its addresses with their MAC addresses, its kernel and its version.
3. The leader checks:
   - that the connection comes from one of the addresses claimed, inside the backend networks the directors allow;
   - that each claimed MAC address matches the leader's own neighbour table for that address - they share an L2 network, so it can tell;
   - that the name is not held by a live backend, and not one an operator removed.
4. The backend waits, **pending**, until a director approves it (`thelb admin approve web-1`) - see [Approval](#approval).
5. The leader signs a **certificate valid for 24 hours**, for `spiffe://dmarts.app/thelb/<cluster>/backend/web-1`, with the backend's addresses as IP names; records the backend; and pins each address to its MAC address on the director holding the VIPs.
6. From then on the backend uses that certificate for everything, sends a **heartbeat every 2 s** over the same connection (a lease of 10 s), and renews the certificate before it expires.

When the cluster is recreated - every director stopped - backends find the new cluster on the control VIP and enrol again with the same secret, by themselves.

### How a backend is trusted

| Question | Answered by |
|---|---|
| Who is it? | A certificate signed by the cluster's CA, naming the backend and its role |
| Is it where it says it is? | The connection comes from an address in its certificate, and that address's MAC address matches its enrolment; the MAC address is pinned on the director |
| May it serve this service? | Only if a director assigned its offer to that service - a backend never chooses one |
| Is it still there? | The heartbeat lease over its own connection, and the director's own health check |
| How did it get its certificate? | Proving the backends' secret over a connection bound to that proof; the backend networks the directors allow; a director's approval |
| How is it removed? | `thelb admin remove web-1`: its certificate's serial is denied until it would have expired, its name may not enrol again until an operator allows it, its connection is dropped, its assignments end. A backend that may have leaked the secret means rotating it |

### Offers, services and assignments

**A backend offers; a director decides.** A backend registers only what it serves - each application under a name of its own choosing, the protocol and port it listens on, and how to check it - when it starts, or later, once the application is up (decided by the user: a backend joins first, offering nothing if it likes, and registers its applications itself while it is a member):

```text
sudo thelb backend --name web-1 --join 10.0.0.10:7443 --secret-file /etc/thelb/backend.secret \
  --offer shop=tcp/8080,check=http:/healthz
sudo thelb offer add metrics tcp/9100          # on web-1, while it runs
sudo thelb offer remove metrics
```

It names no service and no VIP, so no backend needs to know a service's name, or which port its peers use: nginx may listen on 8080 on one node and on 9090 on another.

**The leader checks an offer before it records it**: the offer's own check - a TCP connect by default - from the leader, at the backend's address and port. An application that does not answer is refused, with the reason. A backend offers an `--offer` flag only once its own check of it passes - so it may start before its applications - and one refused again when its check passes again, or 10 s later. A recorded offer is then watched as any is: the backend's own check every 2 seconds, reported with each heartbeat, and - once assigned - the leader's. A failing check takes the offer out of its services' rotation (weight 0) without unassigning it; it comes back by itself.

**Offers outlive their nodes' restarts**: the cluster keeps them, and a backend that starts again takes up what the cluster holds for it. **And the directors' outage**: a backend keeps what it offers in memory - its flags, what it offered since, what it took up - and offers it all again to a cluster created again, once approved there, so a single director's restart costs the backends nothing; the directors' side - services and assignments - comes back from [`thelb generate commands`](#what-thelb-generate-prints). An offer is withdrawn only by `thelb offer remove` (or the backend's removal), never by a restart.

**Every offer appears in `thelb status`** on every director - `thelb status offers` - with its backend's own check and the service it is assigned to, or none: an offer without a service is waiting for one (see [`thelb status`](#thelb-status)).

**A director creates services, and assigns offers to them** - the service's name, chosen there; its VIP, the lowest free address of a pool or one it names; its protocol and port:

```text
sudo thelb admin service add nginx --pool external --proto tcp --port 80
sudo thelb admin assign nginx web-1:shop web-2:shop --weight web-1=3     # or web-1:tcp/8080, by port
```

`--match '<regular expression>'` assigns the waiting offers of every matching node, after a confirmation. A VIP carries any number of services, each on its own port.

**Before an assignment is committed**, the leader checks:

1. the offer's protocol is the service's;
2. **the forward path**: the director reaches the backend directly - its route to the backend's address goes through no gateway - and the backend's MAC address is the pinned one;
3. **the return path**: the backend is told the VIP and sends a probe with the VIP as its source - from a raw socket, so it needs no address for it - to a director's own address in the VIP's network. The director watches for it on its interfaces, ahead of its own IP stack (which drops a packet from one of its own addresses). The probe arriving shows that the backend's replies, sent from the VIP through its own gateway, get through. If it does not arrive, the assignment is refused, and `status` says why.

Then the backend adds the VIP to `thelb0`, maps the service's port to its own - 80 to 8080 on `web-1`, 80 to 9090 on `web-2` - checks its application, and reports ready; when both its own check and the director's pass, the director adds it to the IPVS service, with its weight. `thelb admin unassign nginx web-2` takes an offer back.

**The service's VIP comes up last** (decided by the user: the offer still active, then IPVS, then the VIP): the leader holds a service's VIP only while a backend serves one of the VIP's services, and adds it only after IPVS has that backend - so the first packet the VIP takes in has somewhere to go. When the last backend stops serving, the VIP goes first, then IPVS changes. A service without a backend serving has its address reserved in its pool, and nothing on the network.

**A backend never chooses a service**, so a compromised backend cannot claim one: it can offer, and a director decides.

**A Kubernetes Service is a service already**: its name, ports and protocols are its own, and its backends are its nodes - each node's host enrolled and approved as a theLB backend first. After that, its Services need no offers and no assignments: they are handled by themselves (see [Kubernetes](#kubernetes)).

## The cluster, and what Raft carries

- **[Raft](https://raft.github.io/)** ([hashicorp/raft](https://github.com/hashicorp/raft)) among the **directors only**. One director is a working cluster, without failover; **three are recommended**; five for more tolerance. `doctor` warns about two voters: losing either loses the majority, so two are less available than one.
- **Backends are not members**: they are clients of the leader, through the control VIP, however many there are.
- **Who holds the VIPs**: at first the leader holds the control VIP and every service VIP - one active director, the others standing by. Spreading service VIPs across directors (active-active, assigned by the leader) comes later.
- **Fail closed**: holding a VIP requires leadership with a valid lease from the majority. A director that loses it - a network partition, a hung leader - gives its VIPs up within the lease time, even if it seems healthy to itself. Every decision carries the leader's **term**: a director or backend ignores instructions from an older term, so a deposed leader cannot act after the fact.
- **Failover time**: Raft's election timeout (about a second by default, a few hundred milliseconds on a LAN) plus the announcement. Backends reconnect to the control VIP when it moves. To be measured, not promised.
- **Transport**: the control port on each director's own address, with mutual TLS 1.3. ALPN says what a connection is for: `thelb-raft/1` for Raft, `thelb-api/1` for requests between directors - check-ins, approvals, renewals - and `thelb-join/1`, on the control VIP only, for a node asking to join.
- **The library**: [hashicorp/raft](https://github.com/hashicorp/raft) v1.8.0, with its in-memory stores and its default timings - a heartbeat timeout of one second, a leader lease of half a second, pre-voting on, so a director coming back from a partition does not disrupt the cluster.
- **In memory only** - the log, the snapshots, the vote - so theLB writes nothing to disk for the cluster. Raft's safety needs a voter's term and vote to survive a restart, or a restarted voter could vote twice in one term, so **every process start is a new member identity** (the node's name and a random suffix): a restarted director joins as a new member, catches up, and is then promoted to voter, and the leader removes its old identity.
- **Check-ins**: every director that does not lead checks in with the leader every second - its member identity, how far it has applied the log, its version. They are not in Raft. A director that has not checked in for 3 seconds is shown down; a new member is promoted to voter at its first check-in that has applied its own admission. Every 10 seconds the leader removes member identities no approved node holds - a restarted director's old one - and drops pending nodes that stopped asking an hour ago.
- **A removed member** is not always told by Raft - the leader stops sending to it - but it sees its own record leave the replicated state, and stops.
- **Measured in the lab** (one virtual machine, three directors in network namespaces): a crashed leader's control VIP is on another director 1.4 to 2.9 seconds later, its gratuitous ARP already in the client's neighbour table; a leader cut off from the others gives the VIP up by itself.

### What Raft carries

Only the cluster's desired and decided state - small, and changed when someone or something changes it, not on every heartbeat:

| State | Changes when |
|---|---|
| The cluster: its ID, creation time, and **the CA's certificate and key** | bootstrap (and a CA rotation, if one is ever asked for) |
| Directors: name, member identity, addresses, certificate serial, voter or not | a director joins, is promoted, is removed |
| Rotated secrets, while old and new are both accepted (the original secrets come from the directors' files) | `admin secret rotate`, `admin secret retire` |
| Pending nodes: name, role, addresses, MAC addresses, node fingerprint, since when | a node asks to join; a director approves or refuses it |
| Approved nodes: name, role, node fingerprint, addresses, MAC addresses, who approved them and when - so a restart rejoins without approval | a director approves; a director removes |
| Backends: name, addresses, pinned MAC addresses, certificate serial, labels; removed names | enrolment, removal |
| Revoked certificate serials, until they would have expired | `admin remove` |
| Services: name, VIP, protocol, port, scheduler, persistence, forwarding method, where it came from (configuration, command line, Kubernetes) | the configuration is loaded, `admin service add`/`remove`, a Kubernetes Service changes |
| Pools: first address and prefix, count, interface, and every address allocated from them, with its service | configuration, `admin pool add`/`remove`, an allocation or a release |
| Offers: node, name, protocol, application port, check, since when | a backend offers - once the leader's check passes - or withdraws an offer; a backend removed |
| Assignments: offer, service, weight, the result of the path checks | `admin assign`/`unassign`, a backend removed |
| Backend health: healthy or not, per assignment | the state **changes**, debounced - not every check |
| VIP holders and their term | the leader changes; later, VIP assignments |
| Kubernetes allocations: Service to VIP(s) | a VIP is allocated or released |

**What it does not carry**: backends' heartbeats (they end at the leader), each health check's result, IPVS's connection state (the kernel's own sync daemon, when enabled, carries that), metrics, logs, and each node's own kernel state.

**On the wire**, directors exchange Raft's own messages: entries and heartbeats from the leader (a heartbeat every hundred milliseconds or so), votes in an election, snapshots for a member that is far behind - every new member identity starts with one, the CA in it - and the hand-off of leadership on a planned stop. The whole state is kilobytes to a few megabytes.

**Entries are versioned**, so directors of two versions can apply each other's entries during a rolling upgrade; an entry a director does not understand stops it, rather than being skipped.

**Every entry is authorised before it is written**: the leader checks the request against the requester's certificate - only a director creates pools and services, assigns offers and approves nodes; a backend only offers and reports - and records who asked. Applying an entry is deterministic, so every director arrives at the same state.

### When every director stops

The cluster's state is gone, by design; its secrets are not, because they are the user's files. The bootstrap director creates a new cluster, with a new CA, and every director and backend asks to join it again by itself - and, the cluster being new, **every one of them waits for approval again**; `thelb admin approve --match` approves many at once. Pools, services and assignments do not come back by themselves: theLB reads no configuration file - only its two secret files - so they are created again with `thelb admin`, by hand or from a script the user keeps (see [`generate commands`](#what-thelb-generate-prints)).

## Health checks

- **Two views, both required**: the backend's agent checks its application locally and reports it with its heartbeat; the director holding the VIP checks the backend from outside. A backend serves only when both say it can, and its heartbeat lease is current.
- **Kinds**: TCP connect; SCTP association (set up, then shut down cleanly); HTTP or HTTPS `GET` with an expected status; none (always healthy).
- **Defaults**: every 2 s, 1 s timeout, healthy after 2 successes, unhealthy after 3 failures.
- **The director's check goes to the backend's own address and the application's port** - never through the VIP, which on a director is local.
- **The directors share it**: one check per application - its address, port and kind - however many services it is assigned to, given by the leader to one director: itself or another whose check-in is current, chosen by rendezvous hashing of the application over the directors, so a director coming or going moves only the checks that were, or become, its own. The leader sends each director its share with the answer to its check-in; the director checks, from where the leader's view stands, and reports with its next one. The leader goes by a director's result once it has one and while it is fresh (3 s), and makes the check itself otherwise - before the first result, and when a director stops checking in - so a check is never left unmade.
- **Result**: an unhealthy backend gets weight 0; when every backend of a service is unhealthy, the service stays (connections are refused by the backends, not black-holed) and the state is reported. The other services on the same VIP are unaffected.
- IPv4 and IPv6 backends alike.

## Kubernetes

theLB is a provider for Services of type `LoadBalancer`, for **Kubernetes 1.35.0 and later**. It refuses to start against an older API server.

### How traffic reaches the pods

With direct return, a packet reaches a node with the VIP as its destination. **kube-proxy already handles that**: it programs every Service's load-balancer IP, so the packet goes on to a pod, and the reply leaves from the node, with the VIP as its source, straight to the client. theLB therefore:

- uses the **nodes as backends**, at the **Service's port** (not its node port): the packet keeps `VIP:port`, which kube-proxy matches, and the Service's `targetPort` already maps it to the pods' port;
- writes `status.loadBalancer.ingress` with **`ipMode: VIP`**: the load balancer passes packets through unchanged, so kube-proxy may also send in-cluster traffic for the VIP straight to the pods;
- needs nodes **not to answer ARP for the VIP**. kube-proxy in IPVS mode puts load-balancer IPs on its own interface, `kube-ipvs0`, on every node; theLB's backend on the node sets `arp_ignore` and `arp_announce` at runtime - what kube-proxy's `strictARP` would set - so kube-proxy's configuration needs no change.
- **patches nothing of kube-proxy's or Cilium's**: both read the Service's status, and theLB writes only that. Cilium's own LB-IPAM, L2 announcements and BGP take only Services without a class, or with Cilium's own, so `dmarts.app/thelb` keeps them away. To verify in kind (phase 3): that kube-proxy, in each of its modes, and Cilium's kube-proxy replacement program the load-balancer IP of a Service of another provider's class from its status.

### Which Services, and which backends

- Services of type `LoadBalancer` with **`spec.loadBalancerClass`** set to theLB's class, **`dmarts.app/thelb`**, so theLB never takes Services meant for another provider.
- **VIPs from theLB's own [address pools](#address-pools)**; dual-stack Services (`spec.ipFamilies`) get one VIP per family. A Service brings its own name, ports and protocols, so it needs no offers: its nodes are its backends.
- **Sharing a VIP**: Services with the same `dmarts.app/thelb-shared-vip` annotation share one VIP, as long as their ports do not overlap - each keeps its own backends.
- **A node is a backend only when its host runs theLB as a backend that a director approved** - matched to its Node by address - and has reported ready. A Kubernetes node goes through the same approval as any backend; once it is approved, its Services are handled by themselves - a VIP from their pool, the right nodes as backends - with no offers and no assignments.
- **`externalTrafficPolicy: Local`**: backends are the nodes with ready endpoints, checked at the Service's `healthCheckNodePort`, which kube-proxy answers per node; the client's address reaches the pod.
- **`externalTrafficPolicy: Cluster`**: every ready node is a backend, checked through kube-proxy's own health endpoint.
- **A director can be a Kubernetes node, but not one running kube-proxy** (or a replacement for it, such as Cilium's). The catch is in the data path: kube-proxy programs every Service's load-balancer IP on every node, ahead of IPVS. A packet for the VIP that reaches a director which is also a kube-proxy node is taken by kube-proxy's rules - in `PREROUTING` for its iptables and nftables modes, earlier still for eBPF - before IPVS, which sits at `LOCAL_IN`, ever sees it. That node then sends every connection to pods itself, the replies come back through it, direct return is lost, and theLB's IPVS service is never used; with `externalTrafficPolicy: Local` and no pod on that node, kube-proxy drops the packet instead. In IPVS mode, kube-proxy also manages the same kernel IPVS table and the same IPVS parameters as theLB, and the two would undo each other's changes. `ipMode` cannot help: it applies to every node at once, and the backends need `VIP`.
- So a director that is a Kubernetes node is a **dedicated** node: tainted so that no workload runs on it, and with kube-proxy kept off it (a node affinity on kube-proxy's DaemonSet) - its pods could not reach Services anyway. `doctor` refuses the director role on a node where kube-proxy, or its replacement, is running.

### Where theLB runs

**Always under systemd, never as a pod** - on a Kubernetes node as on any other machine. Decided, and confirmed (the user, 2026-11-09: "we will go with systemd for kubernetes, no daemonset").

**Why not a DaemonSet**, though it would bring a credential mounted and rotated by Kubernetes: watching Services and writing their status is **one writer's job**, and theLB has one - the leader, which already decides everything else, so it never contends with another writer for a Service's status. A DaemonSet would give write access to every Service's status to every node - as many watchers of the API as there are nodes, and any compromised node able to point any Service elsewhere - when the nodes need no API access at all. MetalLB needs a pod on every node because its nodes announce the addresses; theLB's directors announce them. Had the credential to stay inside the cluster, the way would have been a small controller Deployment, one writer still - not taken: nothing of theLB's runs in the cluster.

| Component | Runs as | Talks to the Kubernetes API |
|---|---|---|
| Directors | systemd services, on machines of their own or on dedicated nodes | with a **kubeconfig** for the `thelb-director` ServiceAccount - required, and checked at startup; only the leader watches Services and writes their status, the others hold the same credential to take over |
| Backends on Kubernetes nodes | systemd services on the nodes' hosts, enrolled and approved like any backend | not at all: the directors read the API and tell them what they need |

A director checks at startup that the API server answers and that it holds every permission it needs (`SelfSubjectAccessReview`, which every authenticated user may call); a missing one stops it with its name.

On a Kubernetes node, a backend adds nothing for a VIP - kube-proxy already accepts the VIP's packets; it sets `arp_ignore` and `arp_announce`, checks kube-proxy's mode, sends the return-path probe, and reports the node's health.

### The manifests

Nothing of theLB's runs in the cluster, so `thelb generate k8s` prints only what the directors need, in namespace **`thelb`**:

| Object | What for |
|---|---|
| `Namespace` `thelb` | Holds the directors' identity. |
| `ServiceAccount` `thelb-director` | The directors' identity. |
| `ClusterRole` + `ClusterRoleBinding` `thelb-director` | The permissions below. |
| `Secret` `thelb-director-token` (type `kubernetes.io/service-account-token`) | The directors' credential, and the kubeconfig built from it, printed for the user to install on the directors. |

**RBAC, the least the directors need:**

| API group | Resource | Verbs |
|---|---|---|
| core | `services` | `get`, `list`, `watch` |
| core | `services/status` | `update`, `patch` |
| `discovery.k8s.io` | `endpointslices` | `get`, `list`, `watch` |
| core | `nodes` | `get`, `list`, `watch` |
| `events.k8s.io` | `events` | `create`, `patch` |

## Address pools

**Pools hold the service VIPs**; the control VIP is the bootstrap's own, and in no pool. Any director registers a pool:

```text
sudo thelb admin pool add external --vip 10.10.1.20/24 --vippool-count 10
sudo thelb admin pool add internal --vip 10.0.0.50/24 --vippool-count 10 --default
```

- **CIDR, and a count**: the first address and its prefix - `10.10.1.20/24` - and how many addresses the pool keeps from there: `10.10.1.20` to `10.10.1.29`, inside `10.10.1.0/24`. A VIP has no port; services choose ports.
- **Its interface**, on each director: the one with an address in the pool's network, or the one `--interface` names. A director that has neither cannot hold that pool's VIPs, and `status` and `doctor` say so.
- **Checks when it is registered**: the range lies inside its network, without the network's or the broadcast address; it overlaps no other pool, the control VIP, or any director's own address; and a probe of the network (ARP or NDP) finds none of its addresses in use - an address in use is reported, and left out.
- **Checks before a VIP is plumbed**: it is probed again; if anything answers, the service takes the next free address instead, and the conflict is reported.
- **Many pools**, on different networks and interfaces, under one daemon - see [Many interfaces, one daemon](#many-interfaces-one-daemon).
- **Allocating**: a service takes the lowest free address of its pool, or the one it names; the allocation is in Raft.
- **Resizing**: a count may grow while the addresses it adds are free; it may not shrink below an address in use, and a pool with an address in use cannot be removed.
- **Kubernetes** draws from the same pools: `dmarts.app/thelb-pool` names one, or else a Service draws from its family's `--default` pool; `dmarts.app/thelb-ip` asks for a particular address (Kubernetes deprecated `spec.loadBalancerIP`); a dual-stack Service gets one address per family; the address goes back to its pool when the Service is deleted, or leaves theLB's class.
- **After a full stop**, pools and services are created again with `thelb admin`; a Kubernetes Service keeps its address in `status.loadBalancer.ingress` and asks for it back.

## Command line and configuration

Six commands, and `--version`. Everything else is a flag. **Every command runs only with effective user ID 0** - root - and refuses otherwise; only the help (`thelb`, `--help`) and `--version`, which read nothing, run for anyone.

| Command | Does |
|---|---|
| `thelb` | With no command: the help, and nothing else, whether or not theLB is running; exit status `0`. |
| `thelb director` | Runs this node as a director: `--vip` (an address, or a name) and `--port` for the control VIP, `--secret-file` for the directors' secret and `--backend-secret-file` for the backends' (written the first time a cluster is bootstrapped, if missing), `--bootstrap` where wanted, `--cluster` for the cluster's name at bootstrap (default `thelb`), `--name` for this node's (default: its hostname), `--interface` when the control VIP's interface is not the one in its network. |
| `thelb backend` | Runs this node as a backend: `--join <control VIP:port>`, `--secret-file` for the backends' secret, `--offer [<name>=]<protocol>/<port>[,check=...]` for each application it serves at start (none is fine), `--name`, `--interface`. |
| `thelb offer` | On a backend: `add <name> <protocol>/<port>[,check=...]` offers an application to the cluster - recorded once the leader's check passes - and `remove <name>` withdraws it. |
| `thelb status` | The cluster, in tables - see below: alone, a summary; `directors`, `backends` or `vips` for one part of it, `all` for everything, or a table's name (`cluster`, `nodes`, `certificates`, `pending`, `pools`, `offers`, `services`). `--json` for the same as JSON; `--watch` to repeat. |
| `thelb doctor` | Checks this node and says what it would change: kernel and its adaptations, modules, capabilities, kernel parameters (and whether the system's configuration would keep them after a reboot), interfaces, firewalld and SELinux, other software in the way (keepalived, another IPVS user, kube-proxy's mode and `strictARP`), the platform (clouds where VIPs cannot move), the cluster's voter count - and the plan: every change `director` or `backend` would make here. Changes nothing. `--json` too. |
| `thelb generate` | Prints file content and writes nothing: `secret`, `commands`, and - to come - `systemd`, `sysctl`, `modules`, `k8s` - see below. |
| `thelb admin` | The only way to change the cluster from outside, on a director: `approve <node>` or `approve --match <regex>`, `refuse <node>`, `remove <node>`, `pool add|remove`, `service add|remove`, `assign|unassign`, `secret show|rotate|retire`. A backend has no `admin`. |

### `thelb status`

One command answers "what is running where", one part at a time, so that no page is too full to read: **`thelb status`** alone is a summary - the cluster's row, and how many directors, backends, services and VIPs there are, how many are up, and how many nodes wait for approval; **`thelb status directors`** is the directors, those waiting for approval and their certificates; **`thelb status backends`** the backends, those waiting, their offers, and every backend serving a service, with its checks, weight and counters; **`thelb status vips`** the VIPs and who holds them, the services and the pools; **`thelb status all`** everything below, in one page. Together they show who leads, every node - with its node fingerprint - and when it last checked in, the cluster's certificates (the CA's, and each node's, with their fingerprints and expiry - never a key), the nodes waiting for approval, every pool, every offer - and which service it is assigned to, or that it waits for one - every VIP and who holds it, every service, and every backend serving it - as tables in the style of a SQL client's results, or as JSON.

```text
$ sudo thelb status all
 cluster | id       | created             | leader | term | leader since        | vip lease
---------+----------+---------------------+--------+------+---------------------+-----------
 prod    | 7f3c91a2 | 2026-11-09 10:02:11 | lb-1   |    4 | 2026-11-09 10:02:13 |        6s

 node  | role     | address   | state | version | node fingerprint | last check-in
-------+----------+-----------+-------+---------+------------------+---------------
 lb-1  | leader   | 10.0.0.11 | up    | v0.1.0  | 7c41e0a2f39b55d1 | -
 lb-2  | director | 10.0.0.12 | up    | v0.1.0  | 52e09a1bc3d84f70 | 0.3 s ago
 lb-3  | director | 10.0.0.13 | up    | v0.1.0  | a91f0c63d2e8b745 | 0.2 s ago
 web-1 | backend  | 10.0.0.21 | up    | v0.1.0  | 0be98d17c46a2f83 | 1.1 s ago
 web-2 | backend  | 10.0.0.22 | up    | v0.1.0  | 3d7e21f9a0c46b18 | 0.6 s ago
 db-1  | backend  | 10.0.0.31 | up    | v0.1.0  | c5a8e3071bd92f64 | 1.9 s ago

 certificate   | kind     | fingerprint      | not after
---------------+----------+------------------+---------------------
 theLB prod CA | ca       | b39b2bc27e695d13 | 2036-11-06 10:02:11
 lb-1          | director | 113b8cd5ce3cffd4 | 2026-11-10 10:02:11
 lb-2          | director | 6a04d1e95b3c27f8 | 2026-11-10 10:03:40

 pending | role     | address   | mac               | node fingerprint | waiting
---------+----------+-----------+-------------------+------------------+---------
 lb-4    | director | 10.0.0.14 | 52:54:00:3a:19:04 | 7c41e0a2f39b55d1 | 2 min
 web-3   | backend  | 10.0.0.23 | 52:54:00:3a:19:23 | 9f12c4e07ab35d61 | 15 s

 pool     | network      | addresses               | interface | used
----------+--------------+-------------------------+-----------+---------
 external | 10.10.1.0/24 | 10.10.1.20 - 10.10.1.29 | eth0      | 1 of 10
 internal | 10.0.0.0/24  | 10.0.0.50 - 10.0.0.59   | eth1      | 1 of 10

 node  | offer | address   | port     | check         | own check | service | since
-------+-------+-----------+----------+---------------+-----------+---------+-------
 web-1 | shop  | 10.0.0.21 | tcp/8080 | http /healthz | ok        | nginx   | 2 h
 web-2 | shop  | 10.0.0.22 | tcp/9090 | http /healthz | ok        | -       | 40 s
 db-1  | pg    | 10.0.0.31 | tcp/5432 | tcp           | ok        | db      | 2 h

 vip        | purpose | pool     | interface | held by | since
------------+---------+----------+-----------+---------+---------------------
 10.0.0.10  | control | -        | eth1      | lb-1    | 2026-11-09 10:02:13
 10.10.1.20 | service | external | eth0      | lb-1    | 2026-11-09 10:05:40
 10.0.0.50  | service | internal | eth1      | lb-1    | 2026-11-09 10:06:02

 service | vip        | port     | scheduler | method | backends up
---------+------------+----------+-----------+--------+-------------
 nginx   | 10.10.1.20 | tcp/80   | wrr       | direct | 1 of 1
 db      | 10.0.0.50  | tcp/5432 | wrr       | direct | 1 of 1

 service | vip            | backend | at             | checks  | weight | connections | packets | bytes  | last check-in
---------+----------------+---------+----------------+---------+--------+-------------+---------+--------+---------------
 nginx   | 10.10.1.20:80  | web-1   | 10.0.0.21:8080 | ok / ok |      3 |         412 |  918244 | 1.1 GB | 1.1 s ago
 db      | 10.0.0.50:5432 | db-1    | 10.0.0.31:5432 | ok / ok |      1 |          37 |   40116 | 9.8 MB | 1.9 s ago
```

`--json` prints the same, one key per table, each a list of rows; times are RFC 3339, and every "ago" is also a number of seconds:

```json
{
  "cluster": { "name": "prod", "id": "7f3c91a2", "created": "2026-11-09T10:02:11Z", "leader": "lb-1", "term": 4, "leader_since": "2026-11-09T10:02:13Z" },
  "nodes": [
    { "node": "lb-1", "role": "leader", "address": "10.0.0.11", "state": "up", "version": "v0.1.0", "node_fingerprint": "7c41e0a2f39b55d1", "last_check_in": null, "last_check_in_seconds": null },
    { "node": "web-1", "role": "backend", "address": "10.0.0.21", "state": "up", "version": "v0.1.0", "node_fingerprint": "0be98d17c46a2f83", "last_check_in": "2026-11-09T10:40:02Z", "last_check_in_seconds": 1.1 }
  ],
  "certificates": [ { "certificate": "theLB prod CA", "kind": "ca", "fingerprint": "b39b2bc27e695d13...", "not_after": "2036-11-06T10:02:11Z" } ],
  "pending": [ { "pending": "lb-4", "role": "director", "address": "10.0.0.14", "mac": "52:54:00:3a:19:04", "node_fingerprint": "7c41e0a2f39b55d1", "waiting_seconds": 120 } ],
  "pools": [ { "pool": "external", "network": "10.10.1.0/24", "first": "10.10.1.20", "count": 10, "interface": "eth0", "used": 1 } ],
  "offers": [ { "node": "web-2", "offer": "shop", "address": "10.0.0.22", "protocol": "tcp", "port": 9090, "check": "http /healthz", "own_check": "ok", "service": null, "since_seconds": 40 } ],
  "vips": [ { "vip": "10.10.1.20", "purpose": "service", "pool": "external", "interface": "eth0", "held_by": "lb-1", "since": "2026-11-09T10:05:40Z" } ],
  "services": [ { "service": "nginx", "vip": "10.10.1.20", "protocol": "tcp", "port": 80, "scheduler": "wrr", "method": "direct", "backends_up": 1, "backends": 1 } ],
  "backends": [ { "service": "nginx", "vip": "10.10.1.20", "port": 80, "backend": "web-1", "address": "10.0.0.21", "app_port": 8080, "own_check": "ok", "director_check": "ok", "weight": 3, "connections": 412, "packets": 918244, "bytes": 1181116006, "last_check_in_seconds": 1.1 } ]
}
```

**On a director**, `status` shows the whole cluster, as above - everything except secret material: no secret and no key, ever. **Only an approved director can tell which service runs where**: on which node's address and port, behind which VIP, and how much traffic it has been sent - `checks` is the backend's own check and the director's, `packets` and `bytes` what IPVS forwarded to it (direct return means the replies never pass the director, so there is no outbound count). A director that is not the leader asks the leader for what only the leader knows (backends' last check-ins), and says so if it cannot reach it. A secret is shown only by `thelb admin secret show`, and only on a director.

**On a backend**, `status` shows what that backend is, and nothing about the rest of the cluster: that it is a backend, the cluster's name, each of its offers - its name, port, check, its own check's state, whether the cluster holds it - with the name of the service a director assigned it to (`-` while it waits) - not the service's VIP - and its node fingerprint.

```text
$ sudo thelb status
 role    | cluster | state                   | offer | port     | check         | own check | offered | service | node fingerprint
---------+---------+-------------------------+-------+----------+---------------+-----------+---------+---------+------------------
 backend | prod    | a member of the cluster | shop  | tcp/8080 | http /healthz | ok        | yes     | nginx   | 0be98d17c46a2f83
```

`--json` gives the same: `{ "role": "backend", "cluster": "prod", "offers": [ { "offer": "shop", "port": "tcp/8080", "offered": "yes", "service": "nginx" } ], "node_fingerprint": "0be98d17c46a2f83" }`. While the backend waits for approval, it has no offers listed yet, and its fingerprint is the one a director's `pending` table shows for it. (The VIP is on the backend's `thelb0` all the same - `ip address` shows it; leaving it out of `status` keeps the backend's view to what the backend needs, it does not hide it.)

### The local socket

`thelb status`, `doctor` and `admin` do not read the running theLB's memory: they ask it, over a **Unix socket**, `/run/thelb/thelb.sock`, and it answers.

- **Only what each command shows crosses it.** The daemon answers requests - the tables `status` shows, the checks `doctor` makes, the changes `admin` asks for - and never exposes its internals: no keys, no secrets except where `admin secret show` exists to print one, no Raft log.
- **Root only.** The socket is `0600`, in a directory only root can enter, and the kernel tells the daemon who is calling (`SO_PEERCRED`): a request from anything but effective user ID 0 is refused, whatever the file's permissions.
- **One theLB per node.** A second `thelb director` or `thelb backend` on the same node finds the socket answering, and stops: theLB is already running on this node. A socket file left by a crash - nothing listening - is removed and replaced.
- **Nothing left behind**: `/run` is cleared at boot, and theLB removes the socket when it stops.

**Why not shared memory**: a shared memory segment is readable in full by whoever can map it, so it cannot answer one caller less than another, nor know who is reading; readers need locking against a writer mid-update; its layout ties the reader's version to the writer's; and a segment left by a crash stays in `/dev/shm` until reboot. A socket answers each caller with exactly what it may see, and its requests can be versioned like any API.

### No configuration file

theLB reads two files, and only two: the directors' secret file and the backends'. Everything else is on the command line - the flags of `thelb director` and `thelb backend`, which under systemd live in the unit the user installs - or in the cluster's state, changed with `thelb admin` and replicated by Raft. There is no `--config`.

What a configuration file would have brought back after a full stop - pools, services, assignments - is created again with `thelb admin`. The backend networks a director allows to enrol, which would have been configuration, become an `admin` setting in the cluster's state.

## What `thelb generate` prints

Nothing here is written by theLB. The user decides whether to install it, and where.

| Command | Prints |
|---|---|
| `generate secret` | A new random secret (256 bits), for a user who wants to create a role's secret file before the first bootstrap. |
| `generate systemd` | A unit file: `Type=notify`, `WatchdogSec=`, `Restart=always`, running as root with `CapabilityBoundingSet=` limited to the capabilities theLB needs, `NoNewPrivileges=yes` and the other hardening settings that do not stop it working - but not `ProtectKernelTunables=`, which would make `/proc/sys` read-only. |
| `generate sysctl` | A `sysctl.d` file with the parameters this node's roles need, for the user who wants them set from boot. |
| `generate modules` | A `modules-load.d` file with the modules this node needs. |
| `generate k8s` | The manifests in [Kubernetes](#the-manifests), as one YAML stream. |
| `generate commands` | On a director: the `thelb admin` commands that create the running pools, services and assignments again - each service on the VIP it has - with the nodes to approve as a comment, not a command. A script for the user to keep, and run after a full stop; theLB never reads it: it is not a configuration file. |

## Observability

- **Logs**: one line per event, saying what happened, what changed and from what - shint's grammar, with a timestamp a machine can sort - to standard error, which systemd's journal keeps. Never a key or a secret: a secret's type prints as `[redacted]` wherever it goes.

  ```text
  2026-11-08T18:58:18.437Z [director] OK leader node=d2 term=4 member=d2-36792f70
  2026-11-08T18:58:18.437Z [director] OK control VIP taken vip=198.18.0.10 port=7443 interface=eth0
  2026-11-08T18:58:22.816Z [director] OK director rejoined: approved before node=d3 approved_by=d1 member=d3-976b0f6f address=198.18.0.13
  2026-11-08T18:58:24.283Z [director] WARN join refused: the directors' secret was not proved from=198.18.0.14
  ```
- **Metrics**: Prometheus's text format, on the address `--metrics` names (`GET /metrics`, plain HTTP, written without a client library), from the node's own status: every director says how it stands - `thelb_leader`, `thelb_member`, `thelb_raft_term`, `thelb_nodes{role,state}`, `thelb_vip_lease_seconds`, `thelb_takeovers_total` - and the leader alone what the cluster serves - `thelb_vip_held`, `thelb_service_backends[_up]`, `thelb_backend_weight`, `thelb_backend_check_up{check=own|director}`, `thelb_backend_{connections,packets,bytes}_total`, `thelb_backend_connections_{active,inactive}`, and the traffic through it by service, `thelb_service_{connections,packets,bytes}_total` and `thelb_service_connections_active` - so nothing is counted twice; a backend says `thelb_member`, `thelb_offer_check_up`, `thelb_offer_assigned`. Not yet: enrolments, drift corrections and conflicts as counters.
- **Each connection, with `--debug`**: the leader reads IPVS's connection table (`/proc/net/ip_vs_conn`) every second and logs each new connection once - client, service and VIP, backend, protocol, state - at DEBUG, a level written only with `--debug`; nothing in the log without it.
- **`thelb status`** is the one view of the cluster - see [above](#thelb-status). It, `doctor` and `admin` talk to the running theLB over [the local socket](#the-local-socket).

## Security

- **Mutual TLS everywhere**: between directors, and between backends and the control VIP. Certificates come from the cluster's own CA, created at bootstrap and kept in memory on the directors.
- **Joining needs the role's secret**, proven over the TLS connection and never sent; directors and backends have separate secrets, both the user's files, rotatable - and every new director and backend also waits for an existing director's approval.
- **Backends are checked by address and MAC address**, pinned once enrolled, allowed only the services their certificate's identity is allowed, and cut off when their lease or certificate lapses.
- **Revocation**: removed members' certificate serials are denied until they would have expired; backend certificates last 24 hours.
- **Privileges**: theLB runs only as root (effective user ID 0), and every command checks it; under systemd its capability bounding set is limited to what it needs, so root does not mean every capability. theLB never runs in a container or a pod.
- **No secrets in logs, `status` or standard output**, except the output of `generate secret` and `admin secret show|rotate`, which exist to print one; keys never. Secrets are read only from files, never from the command line.
- **The secret files** are the one thing theLB writes, at the first bootstrap only, if missing, mode `0600`, never overwritten.
- **Listeners on every address**: a director's own service listening on `0.0.0.0` answers on every VIP the director holds; `doctor` lists them ([How a director takes a port](#how-a-director-takes-a-port)).
- **A security policy from the first release**: supported versions, private reporting, how dependencies' fixes are taken - as shint's `SECURITY.md` does - and `govulncheck` gating every release.

## Failure modes

| Failure | What theLB does |
|---|---|
| The leader dies | The others elect a leader within the election timeout; it takes the control VIP and the service VIPs, announces them, programs IPVS - with the same CA. Backends reconnect to the control VIP. Connections in progress carry on, through IPVS's connection synchronisation. |
| Network partition | The side with the majority keeps or takes the VIPs; the other side gives them up when its lease expires. With no majority anywhere, no one holds them. |
| Every director stops | The cluster's state is gone. The bootstrap director creates a new one, with a new CA, and every node joins it again by itself, with its secret. See [When every director stops](#when-every-director-stops). |
| The bootstrap director restarts while the cluster runs | It finds the cluster on the control VIP and joins it, with its secret. |
| The bootstrap director is gone for good | The cluster carries on; after a full stop, any other director with `--bootstrap` creates it again - with the same secret. |
| A node's secret is out of date | It stops, and says how to get the current one (`admin secret show` on a director). |
| A secret leaks | `admin secret rotate`, copy the new file to every node, `admin secret retire`; remove any member that should not be there. |
| Two clusters on one control VIP | The younger stands down. |
| A backend fails | Weight 0 after the failure threshold; back when healthy. |
| A backend's replies from the VIP cannot get through | The assignment's return-path check fails: the assignment is refused, and `status` says why. |
| Multicast does not pass between directors | Connection synchronisation cannot work; `doctor` says so. Connections in progress are lost on a failover; new ones are not. |
| A pool's address is taken on the network | It is left out when the pool is registered, or skipped for the next free one before it is plumbed; the conflict is reported. |
| A backend stops reporting | Weight 0 when its lease expires, whatever its port says. |
| Every backend of a service fails | The service stays, reported unhealthy; the VIP's other services are unaffected. |
| A machine pretends to be a backend | It has no certificate, or its address or MAC address does not match its enrolment: refused. |
| Something on the network answers ARP for a backend's address | The director's pinned neighbour entry ignores it, and the watchdog logs the attempt. |
| Someone flushes IPVS or removes a VIP | The watchdog loop puts it back within its interval, and logs it. |
| A backend stops cleanly | It tells the leader, which gives it weight 0 at once; it keeps its VIPs a moment longer, so connections already sent to it are not black-holed; then it removes `thelb0` and restores its parameters. |
| theLB crashes | The VIPs stay on the interface until it restarts - and the machine's kernel still answers ARP for them, so a host that asks again may be sent there until then (systemd's `Restart=` makes that short); the lease expiry makes another director take over meanwhile; on restart, theLB removes every VIP it left - recognised by its address protocol or label - before anything else, and, once it knows the cluster's services, the IPVS services and pinned backends it left, unless it leads and holds them again. |
| The process hangs | Under systemd, the watchdog restarts it; its lease expires either way. |
| Another system claims a VIP | Logged and counted as a conflict; theLB does not fight over it. |
| The Kubernetes API is unreachable | Current VIPs and backends keep working; no new allocations until it is back. |
| A kernel module is missing and cannot be loaded | Preflight fails; theLB does not start. |
| A switch ignores gratuitous ARP | The announcements repeat; `doctor` can test a link's behaviour. |

## Testing

- **Unit tests** for everything that is not the kernel: planning, the Raft state machine and its entries, authorisation, enrolment checks, VIP assignment, configuration, health-check logic.
- **A network lab in network namespaces**: a bridge, veth pairs, a client, three directors and several backends, IPv4 and IPv6 - a real L2 cluster on one machine, in CI too (GitHub's runners allow `sudo`).
- **Chaos tests in the lab**: kill the leader, partition the directors, restart the bootstrap director, stop every director, flap a backend, flush IPVS, remove a VIP, impersonate a backend, answer ARP for one; measure failover time and how many flows survive.
- **Kubernetes** in [kind](https://kind.sigs.k8s.io/), with kube-proxy in each of its modes, and theLB's backends under systemd inside kind's node containers, which run systemd.
- **Kernels**: the lab under QEMU on each supported kernel series - RHEL 9's 5.14 (from a Rocky or AlmaLinux 9 image), 6.1, and the newest long-term kernel - so both sides of every [kernel adaptation](#kernels) are tested.
- **`make check`** is the one command to run them all, as in shint.

## Project, tooling and releases

Like shint, simpler:

- **One repository**, `github.com/dmartsapp/thelb`, MIT. Go, one static binary (`CGO_ENABLED=0`), Linux only (`amd64`, `arm64`).
- **`Makefile` is the single entry point** for building and checking: `make check` (format, vet, tests, lint, `govulncheck`, docs), `make e2e` (the network lab, needs root).
- **Branches**: `main` is development. Each minor release gets a branch, `release/vX.Y`, cut when the release is ready; its releases are tagged there (`vX.Y.0`, then patches `vX.Y.1`...), and fixes are cherry-picked onto it from `main`. No fast-forwarding `main`.
- **Release workflows run only for a tag `vX.Y.Z` on `release/vX.Y`**: binaries, SHA-256 checksums, signed build attestations, a container image; `govulncheck` must pass.
- **Changelog** in `CHANGELOG.md`; **documentation** in Markdown under `docs/`, published as a site later.
- **Issues and milestones** on GitHub, one milestone per release.

## Phases

| Phase | Delivers |
|---|---|
| 0 - spike | On one machine, by hand-written code: an IPVS direct-routing service over netlink, a VIP added and announced, backends prepared, a port mapped in nftables - IPv4 and IPv6, on RHEL 9's kernel and a current one. Settles the kernel questions: IPv6 neighbour solicitations for a VIP on `thelb0`, probing for `IFA_PROTO`, weighted round robin with connection synchronisation on failover, the return-path probe, VIPs on two interfaces at once, pinned neighbour entries. |
| 1 - one director | **Begun in v0.1.0-dev:** the command line, logging, the secret files, the node fingerprint, the CA and the director's own certificate, the local socket and `status` with its views, and the control VIP - probed, held, announced and given up; **backends** enrolling with their secret and a director's approval, offering, heartbeating, holding their services' VIPs on `thelb0` with `arp_ignore` and `arp_announce`, draining when they stop; **pools, services and assignments** from `admin`, with the forward-path check; **IPVS** over netlink - direct routing, `wrr` by default, the leader holding the service VIPs, pinning backends' MAC addresses, setting `sloppy_tcp`, `expire_nodest_conn` and `expire_quiescent_template`; **health checks from both sides** (TCP, HTTP, none) with the heartbeat lease; what a crash left in the kernel removed at the next start; the network lab (`make lab` to try by hand, `make e2e`). **port mapping** on the backend, in theLB's own nftables table; IP-in-IP; offers by name, made while a backend runs; `generate commands`; drift repaired and the watchdog loop; **IPVS's connection synchronisation**, following leadership. **Built since:** preflight (capabilities, `/proc/sys/net`, IPVS, IPv6); `doctor`; `generate systemd|sysctl|modules`. **To come:** the return-path check; an SCTP check; `doctor`'s plan and its link tests; `generate k8s`; the lab in CI. |
| 2 - the cluster | **Begun in v0.1.0-dev, ahead of the rest of phase 1:** directors joining with their secret and waiting for approval (`admin approve`, `refuse`, `remove`); Raft with the replicated CA; the leader holding the control VIP; the bootstrap rule and restarts, approved directors rejoining by themselves; failover; fail closed; leadership hand-off on stop; the network lab (`make e2e`: a crash, a planned stop, a partition, a full stop). **To come:** a cluster that finds its control VIP claimed by another standing down; `--peers`; secret rotation; IPv6 in the lab on a kernel that has it; chaos tests under load. |
| 3 - Kubernetes | The provider; Kubernetes nodes as approved backends, their Services handled by themselves; shared VIPs; `generate k8s`; kind tests. |
| 4 - tunnelling | **Begun in v0.1.0-dev:** IP-in-IP as a per-service method (`--method tunnel`), IPv4: IPVS's tunnel forwarding, `thelbt0` on the backend, the MSS clamp, backends joining through a router. **To come:** IPv6 (ip6ip6), GUE as an option, the return-path check. |
| 5 - active-active | Service VIPs spread across directors; optional IPVS connection synchronisation. |
| 6 - proxy | A proxying mode, as a separate capability, for networks where direct return is not possible. |

## Open questions


1. **GUE**, beside IP-in-IP (built), as a per-service option for traffic across a multipath network or networks that drop IP protocol 4: 32 bytes a packet, a UDP port to open on the backend, the `fou` module.

### Decided

- **Names: the domain `dmarts.app`**, in DNS order and lowercase: the Service class `dmarts.app/thelb`, annotations under `dmarts.app/` (`dmarts.app/thelb-pool`, `dmarts.app/thelb-shared-vip`, ...), and custom resources, when there are any, in the API group `thelb.dmarts.app` (`ippools.thelb.dmarts.app`, ...). Lowercase because Kubernetes matches the class exactly, and namespace and CRD names must be lowercase anyway.
- **Raft's state is kept in memory only** - no data directory, no opt-in: every start is a new member identity ([The cluster](#the-cluster-and-what-raft-carries)).
- **A node can be both a director and a backend**, for small setups: two daemons, the backend started after the director with `thelb backend`, under a name of its own; one theLB per role per node ([Roles](#roles)).
- **The control VIP may be named**: `--vip thelb-vip`, or `--join thelb-vip:7443` on a backend, is resolved when theLB starts - from `/etc/hosts` or DNS - to the one address it has in a network of the node; a name with no such address, or more than one, is refused. A change to what the name resolves to takes effect at the next start.
- **`thelb generate commands`**, decided by the user: what a configuration file would have brought back after a full stop is a script of `admin` commands the user keeps.
- **No configuration file**: theLB reads its two secret files and nothing else; everything else is a flag, or the cluster's state, changed with `thelb admin` ([No configuration file](#no-configuration-file)).
- **Raft is hashicorp/raft**, theLB's first dependency, as the design always named it; its MPL-2.0 licence allows it in an MIT project's binary - its files are unchanged, and their source is public. theLB's own certificate authority stays its own: a few lines over Go's standard library, with the checks theLB needs (an issuer-chosen identity, SPIFFE IDs, a CA loaded from the replicated state).
- **One control port, three protocols**, told apart by TLS ALPN: Raft, the API between directors, and joins ([The cluster](#the-cluster-and-what-raft-carries)).
- **`thelb status` is a summary by default**, with a view for each part of the cluster - `directors`, `backends`, `vips` - and `all` for everything: one page with every table was too full to read ([`thelb status`](#thelb-status)).
- **A backend's heartbeat goes to the leader on the control VIP**, one connection, every 2 seconds, with its own checks of its offers and the VIPs it holds; the leader answers with what it is to serve, and the offers the cluster holds for it. Offers are made and withdrawn by requests of their own, each answered - recorded only after the leader's check. A backend that stops says so first, and keeps its VIPs until the leader has drained it.
- **The leader writes a backend's health to Raft only when it changes** - both checks passing, the heartbeat current, the VIP held - so a new leader programs the same weights at once, then confirms them with its own checks.
- **RHEL 9 is supported**, so the minimum kernel is 5.14; theLB probes the kernel's features rather than trusting its version, and adapts where a newer feature is missing ([Kernels](#kernels)).
- **Directors bootstrap from the command line**, all with the same command - the control VIP, its port and the secret files - and one or more also with `--bootstrap`. The first bootstrap creates the secrets and writes them to the named files, once, if missing; the user copies them to the other nodes; a node whose secret the cluster does not accept stops as out of date; the CA is created in memory by the bootstrapping director and replicated to every director, so a failover keeps it; after a full stop the cluster is created again and every node rejoins by itself ([Bootstrapping and joining](#bootstrapping-and-joining-directors)).
- **Every node that joins waits for a director's approval** - directors and backends alike; any approved director may approve; backends cannot; pending nodes appear in `thelb status` on any director. **An approved node rejoins after a restart without approval**, recognised by its node fingerprint, address and MAC address ([Approval](#approval)).
- **Root only**: every command runs with effective user ID 0, or not at all - except the help and `--version`, which any user may run.
- **The node fingerprint**: a hash of the machine's application-specific ID, its SMBIOS UUID where there is one, and the MAC address of the interface it joins through ([The node fingerprint](#the-node-fingerprint)).
- **After a full stop, every node is approved again**; `admin approve --match <regex>` approves many at once.
- **SCTP is in the first release**, alongside TCP and UDP.
- **A director may be a Kubernetes node only if it is dedicated, without kube-proxy** - kube-proxy would take the VIP's packets before IPVS ([Kubernetes](#which-services-and-which-backends)).
- **theLB always runs under systemd, never as a pod or a DaemonSet** - on Kubernetes nodes as on any machine; `generate k8s` prints only the directors' identity and permissions ([Where theLB runs](#where-thelb-runs)).
- **A Kubernetes node is a backend only once its host's theLB backend is approved**; its Services are then handled by themselves - no offers, no assignments ([Which Services, and which backends](#which-services-and-which-backends)).
- **What `status` shows**: on a director, everything but secret material, including which service runs on which node's address and port behind which VIP, and the traffic sent to it; on a backend, only its role, the cluster's name, its services by name and its node fingerprint.
- **The local socket**: `status`, `doctor` and `admin` ask the running theLB over a Unix socket that authorises each caller and exposes only what each command shows - not shared memory ([The local socket](#the-local-socket)).
- **The control VIP**, held by the leader, is where backends and the command line talk to the cluster; **backends are clients, not Raft members**.
- **Backends enrol and prove themselves** with the backends' secret, then certificates from the cluster's CA, their address and MAC address, and a heartbeat lease ([Backends](#backends-enrolment-trust-and-services)).
- **Pools** hold the service VIPs: registered on any director, as a first address in CIDR notation and `--vippool-count`, with overlap checks and network probes; the control VIP is the bootstrap's own ([Address pools](#address-pools)).
- **Backends offer, directors decide**: a backend registers its applications - each under a name it chooses, with a protocol and its application's port - at start or while it runs, checked by the leader first, and keeps them through the directors' outage; a director names services and assigns offers to them, after checking the forward and the return path; VIPs carry no port ([Offers, services and assignments](#offers-services-and-assignments)).
- **One daemon, every interface**: VIPs on any of a director's interfaces, served together ([Many interfaces, one daemon](#many-interfaces-one-daemon)).
- **Port mapping on the backend is nftables destination NAT**, in theLB's own table, written over netlink - not eBPF: connection tracking handles the replies, SCTP's checksum, ICMP errors and fragments ([Port mapping](#port-mapping-on-the-backend)).
- **Weighted round robin** is the default scheduler, with IPVS's connection synchronisation on by default so connections survive a failover.
- **The director never changes the port; a backend maps it**, in theLB's own nftables table, when its application's port differs ([Port mapping](#port-mapping-on-the-backend)).
- **One VIP, many services**, each with its own backends; in Kubernetes, Services share a VIP by annotation.
- **Direct routing first, IP-in-IP tunnelling second**, as a per-service forwarding method (`--method direct|tunnel`); IP-in-IP's encapsulation is IPVS's IPIP, for its simplicity - GUE later, as an option; proxying later.
