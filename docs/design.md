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
| **Agent** | theLB running on a backend. On a Kubernetes node it is a pod of the agent DaemonSet. |
| **Cluster secrets** | Two random secrets - one for directors, one for backends - created by the director that first bootstraps the cluster, in the files the user names, and copied by the user to every node of that role. A node joins by proving it knows its role's secret. |
| **Attachment** | A backend serving a service: which backend, which service, the application's port, and how to check it. |

## Where it works

Direct return has hard requirements, and theLB checks them rather than hoping:

- **Directors and backends share an L2 network** (one broadcast domain, per address family), for direct routing: the director forwards a packet by sending it to the backend's MAC address. Tunnelling, the second forwarding method, lifts this - see [The data path](#the-data-path).
- **The director does not change the port.** A backend receives the packet as the client sent it, with the service's port. A backend whose application uses another port has theLB map it locally - see [Port mapping on the backend](#port-mapping-on-the-backend).
- **Backends must accept traffic to the VIP and must not announce it** - see [Kernel state](#kernel-state-thelb-manages).
- **The network must let a VIP move between machines.** On-premises switches and most hypervisors do (some need settings such as accepting forged transmits); public clouds generally do not. `thelb doctor` detects the major clouds and says so.
- **Linux only**, with IPVS in the kernel: **kernel 5.14 or later**, so that RHEL 9 and its rebuilds (Rocky, AlmaLinux), still in extended support, are covered - see [Kernels](#kernels).

### Kernels

theLB supports every kernel from 5.14, the kernel of RHEL 9. It does not decide what a kernel can do from its version number - distribution kernels, RHEL's above all, carry features backported from much newer kernels - but **probes each feature it needs** at startup and adapts:

| Feature | When the kernel has it | When it does not |
|---|---|---|
| Address protocol (`IFA_PROTO`, upstream since 6.1) | theLB's addresses carry theLB's own protocol value | IPv4 addresses are recognised by their label alone, IPv6 addresses by matching a configured VIP (see [Telling what theLB owns](#telling-what-thelb-owns)) |
| The Maglev scheduler (`ip_vs_mh`, upstream since 4.18) | the default scheduler | source hashing (`sh`), with backends always added in the same order so every director hashes alike, and a warning |
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
| `backend` | **Not** a Raft member: a client of the cluster, through the control VIP. It enrols, attaches to the services it serves, holds their VIPs on its own dummy interface without announcing them, maps ports where asked, checks its applications locally, and keeps reporting. |

A node can run both (small setups); a director that is also a backend forwards to itself through the local stack. On a Kubernetes node, the backend role mostly checks rather than configures: kube-proxy already accepts traffic for a Service's load-balancer IP (see [Kubernetes](#kubernetes)).

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
- **Scheduler**: Maglev hashing (`mh`) by default. It maps a flow to the same backend on every director with the same backend set, so a failover keeps most flows on the backend they were using. Round robin, weighted round robin and least connections are options.
- **Unhealthy backend**: weight 0 - no new connections, existing ones finish - not removed. A backend that detaches is removed, and its connections expire (`expire_nodest_conn`).
- **Failover without losing connections**: `sloppy_tcp` lets the new director accept a flow it never saw start; Maglev sends it to the same backend. IPVS's own connection synchronisation (the kernel's sync daemon, master to backups) is an option on top, off by default.
- **Session persistence** (one client to one backend for a time) is a per-service option.
- **SCTP** is balanced per association, like a TCP connection. Multi-homing - one association over several addresses - is not carried: on theLB's side, the VIP is the association's only address.

### Forwarding methods

A service's forwarding method is a per-service setting:

| Method | How | When |
|---|---|---|
| **Direct routing** (first) | The director rewrites the destination MAC address. | Directors and backends on one L2 network: the fastest, and no encapsulation. |
| **IP-in-IP tunnelling** (next) | The director wraps the packet in an outer IP header to the backend's address; the backend's agent creates the tunnel interface and unwraps it. | Backends in other subnets, still with direct return. The outer header costs bytes, so `doctor` checks the path's MTU. |

Neither changes the port, and neither lets the director see the replies: no L7, no TLS termination, and IPVS's connection state is kept from one direction only (its timeouts are tuned for that). The proxy, a later capability, is for what needs more.

### Port mapping on the backend

The director cannot change the port, so a backend whose application listens elsewhere - `VIP:443` served by an application on `8443` - maps it itself. When a backend attaches with an application port different from the service's port, its agent adds one rule to **theLB's own nftables table** (`inet thelb`): packets to `VIP:443` go to port `8443`. The kernel's connection tracking turns the replies back into `VIP:443`, so the client sees the service's port. The mapping is per VIP, protocol and port, so two VIPs can both offer 443 and lead to different applications. It is runtime state like the rest (see [Kernel state](#kernel-state-thelb-manages)); with matching ports there is no rule at all.

It works the same for TCP, UDP and SCTP. Kubernetes Services already map ports, with `targetPort`; this is for plain Linux backends.

## Kernel state theLB manages

Everything here is **runtime state**. theLB sets it at runtime - kernel parameters by writing `/proc/sys`, which is what the `sysctl` command does (the old `sysctl(2)` system call was removed from Linux in 5.5) - and never writes a configuration file. What is already right is left alone and never reverted; what theLB changes is recorded and restored when it stops cleanly ([Philosophy](philosophy.md#2-the-running-kernel-is-the-truth)).

### Director

| State | Value | Why |
|---|---|---|
| Kernel modules | `ip_vs`, and the schedulers in use (`ip_vs_mh` by default) | IPVS itself. Loaded only if missing; needs `CAP_SYS_MODULE` only then. |
| `net.ipv4.vs.sloppy_tcp` | `1` | A director that takes over accepts flows already in progress. |
| `net.ipv4.vs.sloppy_sctp` | `1` | The same for SCTP associations - set only when there are SCTP services. |
| `net.ipv4.vs.expire_nodest_conn` | `1` | Connections to a removed backend end at once instead of hanging. |
| `net.ipv4.vs.expire_quiescent_template` | `1` | Persistence templates of a weight-0 backend expire, so its clients move on. |
| VIP addresses | on the chosen interface, `/32` or `/128` | The control VIP while this director leads; a service VIP while it holds it. |
| IPVS services | one per VIP, protocol and port | Only while this director holds the VIP. |
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
| Tunnel interface | created by theLB | Only for services that use tunnelling. |

**IPv6** has no `arp_ignore`. Linux answers neighbour solicitations only for addresses on the receiving interface, so a VIP on `thelb0` is not answered on `eth0`; phase 0 must prove this on every kernel theLB supports, before anything relies on it.

### Telling what theLB owns

theLB keeps nothing on disk, so after a restart it recognises its own state from the kernel:

- **Addresses** carry theLB's own address protocol value (`IFA_PROTO`, the "who added this address" field upstream Linux has since 6.1) where the kernel has it, and IPv4 addresses always a label (`eth0:thelb`). On a kernel without `IFA_PROTO` (RHEL 9's), an IPv6 address - which has no label - is theLB's when it is a configured VIP: a VIP in theLB's configuration is theLB's by definition, whoever added it.
- **IPVS services** are theLB's when they match a configured service; any other IPVS service is left alone, and `doctor` reports it.
- **`thelb0`, the `inet thelb` table and theLB's tunnel interfaces** are theLB's by name.
- **Kernel parameters** cannot be marked. After a crash theLB cannot know a parameter's value from before it started, so it treats the current value as the one to leave; this is logged.

## Startup, the watchdog loop, and stopping

```text
start ─▶ preflight ─▶ read the kernel ─▶ plan ─▶ apply ─▶ bootstrap or join ─▶ watchdog loop ─▶ stop
           │                               │
           └─ missing authority: exit 1    └─ `thelb doctor` prints this, and exits
```

1. **Preflight - full authority or nothing.** Effective capabilities (`CAP_NET_ADMIN`, `CAP_NET_RAW`; `CAP_SYS_MODULE` if a module is missing); `/proc/sys/net` writable; IPVS present (netlink family, schedulers); nftables if a port mapping is needed; IPv6 enabled if any VIP is IPv6; the interface exists; in Kubernetes, the API server reachable and every permission held (checked with `SelfSubjectAccessReview`). Anything missing: theLB changes nothing, exits `1`, and prints what is missing and how to grant it.
2. **Read the kernel**: every parameter, address, interface, table and IPVS entry it cares about.
3. **Plan**: the difference between what it needs and what is there.
4. **Apply**: kernel parameters and modules now; VIPs and IPVS services only when the cluster gives this node a VIP. Each change is logged with its old value.
5. **Bootstrap or join** - see [Bootstrapping and joining directors](#bootstrapping-and-joining-directors) and [Backends](#backends-enrolment-trust-and-services).
6. **Watchdog loop**: netlink notifications (addresses, links, neighbours) and a periodic full comparison (every 10 s by default). Anything that drifted - an address removed, `ipvsadm -C`, a pinned neighbour entry changed, a parameter changed - is put back and logged. Under systemd, theLB reports to the service manager's watchdog only while this loop is healthy, so a hung process is restarted.
7. **Stop** (`SIGTERM`): a leader hands leadership to another director first (so a planned restart is a short, clean failover); then theLB gives up its VIPs, removes its IPVS services, addresses, neighbour entries, table and interfaces, and restores the parameters it changed. After a crash nothing is cleaned up; the next start finds the state in the kernel and adopts or removes it.

## VIPs and announcing them

- **Interface**: the one given on the command line or in the configuration, or else the interface the route to the VIP's network leaves through. A VIP that no interface's network contains is an error.
- **Adding**: `/32` or `/128`, with theLB's address protocol and label. IPv6 VIPs skip duplicate address detection, so a takeover is immediate; the cluster, not DAD, prevents duplicates.
- **Announcing**, when a director takes a VIP: gratuitous ARP (request and reply forms) for IPv4, unsolicited Neighbor Advertisements with the override flag to all-nodes for IPv6 - three of each over two seconds, because switches and hosts miss single packets. A periodic re-announcement is an option, off by default.
- **Probing before taking a VIP that is new to the network** - the control VIP at bootstrap, a newly allocated service VIP: an ARP probe (RFC 5227) for IPv4, a neighbour solicitation for IPv6. An answer means someone else has it, and theLB refuses.
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

The hostname is shown beside it, as a label, not part of it. In Kubernetes, the agent reads the node's machine ID and SMBIOS UUID through read-only host mounts.

Left out, for their problems: `hostid` (derived from an IP address when `/etc/hostid` is missing, and identical on clones); every MAC address (they change with every container veth, bridge, VPN and randomised Wi-Fi address); the root disk's UUID (absent in a container, changed by a reinstall, shared by every machine built from one cloud image); the hostname (neither stable nor unique).

**The fingerprint is reported by the node itself**: only its address and MAC address are checked on the wire. It recognises an approved machine; the secret and the first approval are what prove one.

**The secrets are strong**: 256 random bits, whether theLB or `generate secret` made them, and theLB refuses a shorter one - a weak secret could be guessed from a recorded proof.

**Why a secret, and not the CA certificate**: the CA certificate is public - the leader sends its certificate chain to anyone who connects - so having it would prove only that the joiner is talking to the real cluster, not that the cluster should trust the joiner. The secret works in both directions, and outlives any one CA: a cluster created again after a full stop has a new CA but the same secret, so no node needs anything new.

**Why the CA is replicated, and not re-created after a failover**: with the CA key on every director, a new leader carries on with the same CA - no director or backend has to be issued a new certificate at the moment a node has just failed. A director can already send all of the cluster's traffic anywhere, so a compromised director is a compromised cluster either way; the CA key on every director adds little to that.

### Restarts, and the bootstrap rule

Raft's state is in memory, so a restarted director remembers nothing of the cluster. The secret file is what lets it find its way back:

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
| May it serve this service? | Its role, and the service's allow-list (backend names, patterns or labels) |
| Is it still there? | The heartbeat lease over its own connection, and the director's own health check |
| How did it get its certificate? | Proving the backends' secret over a connection bound to that proof; the backend networks the directors allow; a director's approval |
| How is it removed? | `thelb admin remove web-1`: its certificate's serial is denied until it would have expired, its name may not enrol again until an operator allows it, its connection is dropped, its attachments end. A backend that may have leaked the secret means rotating it |

### Services and attachments

**Services are defined by the operator**: in the directors' configuration file, with `thelb admin service add`, or by Kubernetes. A service is a VIP, a protocol, a port, a scheduler, a forwarding method and an allow-list:

```text
sudo thelb admin service add web --vip 192.0.2.10 --proto tcp --port 443 --scheduler mh --allow 'backend:web-*'
```

**Backends attach to services**, in their own configuration or with `--serve` when they start:

```text
sudo thelb backend --name web-1 --join 10.0.0.10:7443 --secret-file /etc/thelb/backend.secret \
  --serve web=8443,check=http:/healthz
```

1. The request travels over the backend's own connection, so the leader knows who is asking; `web-1` matches `backend:web-*`.
2. The leader records the attachment. The backend adds the VIP to `thelb0`, sets the ARP parameters, adds the port mapping (443 to 8443), checks its application locally, and reports ready.
3. The director holding the VIP checks the backend from outside (`10.0.0.21:8443/healthz`); when **both** the backend's own report and the director's check pass, it adds the backend to the IPVS service.

**A backend may create a service** only from a pool the operator delegated to it: `--serve api=8080,pool=apps`, with the VIP allocated from `apps`. Without a delegation, a backend can only attach to services the operator defined - so a compromised backend cannot claim an address.

## The cluster, and what Raft carries

- **[Raft](https://raft.github.io/)** ([hashicorp/raft](https://github.com/hashicorp/raft)) among the **directors only**. One director is a working cluster, without failover; **three are recommended**; five for more tolerance. `doctor` warns about two voters: losing either loses the majority, so two are less available than one.
- **Backends are not members**: they are clients of the leader, through the control VIP, however many there are.
- **Who holds the VIPs**: at first the leader holds the control VIP and every service VIP - one active director, the others standing by. Spreading service VIPs across directors (active-active, assigned by the leader) comes later.
- **Fail closed**: holding a VIP requires leadership with a valid lease from the majority. A director that loses it - a network partition, a hung leader - gives its VIPs up within the lease time, even if it seems healthy to itself. Every decision carries the leader's **term**: a director or backend ignores instructions from an older term, so a deposed leader cannot act after the fact.
- **Failover time**: Raft's election timeout (about a second by default, a few hundred milliseconds on a LAN) plus the announcement. Backends reconnect to the control VIP when it moves. To be measured, not promised.
- **Transport**: TCP with mutual TLS between directors.
- **In memory only** - the log, the snapshots, the vote - so theLB writes nothing to disk for the cluster. Raft's safety needs a voter's term and vote to survive a restart, or a restarted voter could vote twice in one term, so **every process start is a new member identity** (the node's name and a random suffix): a restarted director joins as a new member, catches up, and is then promoted to voter, and the leader removes its old identity.

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
| Services: VIP, protocol, port, scheduler, persistence, forwarding method, allow-list, where it came from (configuration, command line, Kubernetes) | the configuration is loaded, `admin service add`/`remove`, a Kubernetes Service changes |
| Pools: ranges, who may draw from them, and every address allocated from them, with its owner | configuration, `admin pool`, an allocation or a release |
| Attachments: backend, service, application port, check | a backend's `--serve`, a backend detaching or being removed |
| Backend health: healthy or not, per attachment | the state **changes**, debounced - not every check |
| VIP holders and their term | the leader changes; later, VIP assignments |
| Kubernetes allocations: Service to VIP(s) | a VIP is allocated or released |

**What it does not carry**: backends' heartbeats (they end at the leader), each health check's result, IPVS's connection state (the kernel's own sync daemon, when enabled, carries that), metrics, logs, and each node's own kernel state.

**On the wire**, directors exchange Raft's own messages: entries and heartbeats from the leader (a heartbeat every hundred milliseconds or so), votes in an election, snapshots for a member that is far behind - every new member identity starts with one, the CA in it - and the hand-off of leadership on a planned stop. The whole state is kilobytes to a few megabytes.

**Entries are versioned**, so directors of two versions can apply each other's entries during a rolling upgrade; an entry a director does not understand stops it, rather than being skipped.

**Every entry is authorised before it is written**: the leader checks the request against the requester's certificate (role, allow-lists, pools), and records who asked. Applying an entry is deterministic, so every director arrives at the same state.

### When every director stops

The cluster's state is gone, by design; its secrets are not, because they are the user's files. The bootstrap director creates a new cluster, with a new CA, and every director and backend asks to join it again by itself - and, the cluster being new, **every one of them waits for approval again**; `thelb admin approve --match` approves many at once. Services in the directors' configuration file come back with it; services added from the command line do not, unless they were saved with `thelb generate config`.

## Health checks

- **Two views, both required**: the backend's agent checks its application locally and reports it with its heartbeat; the director holding the VIP checks the backend from outside. A backend serves only when both say it can, and its heartbeat lease is current.
- **Kinds**: TCP connect; SCTP association (set up, then shut down cleanly); HTTP or HTTPS `GET` with an expected status; none (always healthy).
- **Defaults**: every 2 s, 1 s timeout, healthy after 2 successes, unhealthy after 3 failures.
- **The director's check goes to the backend's own address and the application's port** - never through the VIP, which on a director is local.
- **Result**: an unhealthy backend gets weight 0; when every backend of a service is unhealthy, the service stays (connections are refused by the backends, not black-holed) and the state is reported. The other services on the same VIP are unaffected.
- IPv4 and IPv6 backends alike.

## Kubernetes

theLB is a provider for Services of type `LoadBalancer`, for **Kubernetes 1.35.0 and later**. It refuses to start against an older API server.

### How traffic reaches the pods

With direct return, a packet reaches a node with the VIP as its destination. **kube-proxy already handles that**: it programs every Service's load-balancer IP, so the packet goes on to a pod, and the reply leaves from the node, with the VIP as its source, straight to the client. theLB therefore:

- uses the **nodes as backends**, at the **Service's port** (not its node port): the packet keeps `VIP:port`, which kube-proxy matches, and the Service's `targetPort` already maps it to the pods' port;
- writes `status.loadBalancer.ingress` with **`ipMode: VIP`**: the load balancer passes packets through unchanged, so kube-proxy may also send in-cluster traffic for the VIP straight to the pods;
- needs nodes **not to answer ARP for the VIP**. kube-proxy in IPVS mode puts load-balancer IPs on its own interface, so in that mode `strictARP` must be on; the agent checks it on every node.

### Which Services, and which backends

- Services of type `LoadBalancer` with **`spec.loadBalancerClass`** set to theLB's class, **`dmarts.app/thelb`**, so theLB never takes Services meant for another provider.
- **VIPs from theLB's own [address pools](#address-pools)**; dual-stack Services (`spec.ipFamilies`) get one VIP per family.
- **Sharing a VIP**: Services with the same `dmarts.app/thelb-shared-vip` annotation share one VIP, as long as their ports do not overlap - each keeps its own backends.
- **A node is a backend only when it appears in the API and its agent has enrolled and reported ready.**
- **`externalTrafficPolicy: Local`**: backends are the nodes with ready endpoints, checked at the Service's `healthCheckNodePort`, which kube-proxy answers per node; the client's address reaches the pod.
- **`externalTrafficPolicy: Cluster`**: every ready node is a backend, checked through kube-proxy's own health endpoint.
- **A director can be a Kubernetes node, but not one running kube-proxy** (or a replacement for it, such as Cilium's). The catch is in the data path: kube-proxy programs every Service's load-balancer IP on every node, ahead of IPVS. A packet for the VIP that reaches a director which is also a kube-proxy node is taken by kube-proxy's rules - in `PREROUTING` for its iptables and nftables modes, earlier still for eBPF - before IPVS, which sits at `LOCAL_IN`, ever sees it. That node then sends every connection to pods itself, the replies come back through it, direct return is lost, and theLB's IPVS service is never used; with `externalTrafficPolicy: Local` and no pod on that node, kube-proxy drops the packet instead. In IPVS mode, kube-proxy also manages the same kernel IPVS table and the same IPVS parameters as theLB, and the two would undo each other's changes. `ipMode` cannot help: it applies to every node at once, and the backends need `VIP`.
- So a director that is a Kubernetes node is a **dedicated** node: tainted so that no workload runs on it, and with kube-proxy kept off it (a node affinity on kube-proxy's DaemonSet) - its pods could not reach Services anyway. `doctor` refuses the director role on a node where kube-proxy, or its replacement, is running.

### Where theLB runs

| Component | Runs as | Talks to the API with |
|---|---|---|
| Directors | systemd services on their own machines, outside the cluster | a **kubeconfig** for the `thelb-director` ServiceAccount - required, and checked at startup |
| Agents | the `thelb-agent` **DaemonSet**, one pod per node, host network; they enrol with the backends' secret, from the `thelb-backend-secret` Secret | the pod's **ServiceAccount token**, which Kubernetes mounts into the pod; no kubeconfig file is needed inside the cluster. `--kubeconfig` and `$KUBECONFIG` are used only outside it |

Both check at startup that the API server answers and that they hold every permission they need (`SelfSubjectAccessReview`, which every authenticated user may call); a missing one stops theLB with its name.

### The manifests

`thelb generate k8s` prints everything, for namespace **`thelb`** (Kubernetes namespace names are lowercase):

| Object | What for |
|---|---|
| `Namespace` `thelb` | Everything theLB runs, labelled `pod-security.kubernetes.io/enforce: privileged`: the agent needs host networking and writable kernel parameters, which no other Pod Security level allows. |
| `ServiceAccount` `thelb-agent`, `thelb-director` | One identity per component. |
| `ClusterRole` + `ClusterRoleBinding` per component | The cluster-wide permissions below. |
| `Role` + `RoleBinding` for the agent, in `thelb` | Reading its secret, and nothing else in the namespace. |
| `Secret` `thelb-backend-secret` | The backends' secret, for the agents. `generate k8s --backend-secret-file` fills it in; without it, the Secret is left for the user to fill. |
| `PriorityClass` `thelb-node-critical` | The agent is not evicted before the workloads it carries traffic for. |
| `DaemonSet` `thelb-agent` | One agent per node: host network, privileged, `/lib/modules` and `/etc/machine-id` read-only, tolerates every taint. |
| `Secret` `thelb-director-token` (type `kubernetes.io/service-account-token`) | The directors' credential, and the kubeconfig built from it, printed for the user to install on the directors. |

**RBAC, the least each component needs:**

| Component | API group | Resource | Verbs |
|---|---|---|---|
| director | core | `services` | `get`, `list`, `watch` |
| director | core | `services/status` | `update`, `patch` |
| director | `discovery.k8s.io` | `endpointslices` | `get`, `list`, `watch` |
| director | core | `nodes` | `get`, `list`, `watch` |
| director | `events.k8s.io` | `events` | `create`, `patch` |
| agent | core | `nodes` | `get`, `list`, `watch` |
| agent | core | `services` | `get`, `list`, `watch` |
| agent | `events.k8s.io` | `events` | `create`, `patch` |
| agent, in `thelb` | core | `secrets`, `thelb-backend-secret` only | `get` |

## Address pools

**A proposal, still to be decided** (see [Open questions](#open-questions)). Pools are theLB's own: defined in the directors' configuration or with `thelb admin pool add|remove`, replicated through Raft, and the same for plain Linux and for Kubernetes. Kubernetes holds no pool definition - no ConfigMap, no custom resource.

- **A pool** has a name, address ranges per family (subnets, or first and last addresses), addresses to leave out, and who may draw from it: backends, by name pattern (a delegated pool); Kubernetes, by namespace pattern; or only the operator.
- **Allocating**: the leader takes the lowest free address of the right family, skipping a subnet's network and broadcast addresses and the left-out ones; probes it on the network (ARP or NDP) before handing it out, and skips it if anything answers; and records the allocation, with its owner, in Raft.
- **Kubernetes**: a Service names its pool with the `dmarts.app/thelb-pool` annotation, or draws from its family's default pool; asks for a particular address with `dmarts.app/thelb-ip` (Kubernetes deprecated `spec.loadBalancerIP`); a dual-stack Service gets one address per family. The address goes back to the pool when the Service is deleted, or no longer has theLB's class.
- **No overlaps**: pools may not overlap each other or the control VIP; a service VIP the operator defined inside a pool is reserved in it.
- **Removing a pool** that has addresses allocated is refused until they are released.
- **After a full stop**, allocations are rebuilt from where they are still known: a Kubernetes Service keeps its address in `status.loadBalancer.ingress`, and a backend that created a service from a delegated pool still holds its VIP on `thelb0` - each asks for its address back, and gets it unless someone else already has it.

## Command line and configuration

Six commands, and `--version`. Everything else is a flag. **Every command runs only with effective user ID 0** - root - and refuses otherwise; only the help (`thelb`, `--help`) and `--version`, which read nothing, run for anyone.

| Command | Does |
|---|---|
| `thelb` | With no command: the help, and nothing else, whether or not theLB is running; exit status `0`. |
| `thelb director` | Runs this node as a director: `--vip` and `--port` for the control VIP, `--secret-file` for the directors' secret and `--backend-secret-file` for the backends' (written the first time a cluster is bootstrapped, if missing), `--bootstrap` where wanted, `--config` for the rest. |
| `thelb backend` | Runs this node as a backend: `--join <control VIP:port>`, `--secret-file` for the backends' secret, `--serve <service>=<port>[,check=...][,pool=...]` for each service it serves, `--config` for the rest. |
| `thelb status` | The whole cluster, in tables - see below. `--json` for the same as one JSON document; a table name (`nodes`, `vips`, `services`, `backends`) for one table; `--watch` to repeat. |
| `thelb doctor` | Checks this node and says what it would change: kernel and its adaptations, modules, capabilities, kernel parameters (and whether the system's configuration would keep them after a reboot), interfaces, firewalld and SELinux, other software in the way (keepalived, another IPVS user, kube-proxy's mode and `strictARP`), the platform (clouds where VIPs cannot move), the cluster's voter count - and the plan: every change `director` or `backend` would make here. Changes nothing. `--json` too. |
| `thelb generate` | Prints file content and writes nothing: `secret`, `systemd`, `sysctl`, `modules`, `k8s`, `config` - see below. |
| `thelb admin` | The only way to change the cluster from outside, on a director: `approve <node>` or `approve --match <regex>`, `refuse <node>`, `remove <node>`, `service add|remove`, `pool add|remove`, `secret show|rotate|retire`. A backend has no `admin`. |

### `thelb status`

One command answers "what is running where": who leads, every node and when it last checked in, every VIP and who holds it, every service, and every backend serving it - as tables in the style of a SQL client's results, or as JSON.

```text
$ sudo thelb status
 cluster | id       | created             | leader | term | leader since
---------+----------+---------------------+--------+------+---------------------
 prod    | 7f3c91a2 | 2026-11-09 10:02:11 | lb-1   |    4 | 2026-11-09 10:02:13
(1 row)

 node  | role     | address   | state | version | last check-in
-------+----------+-----------+-------+---------+---------------
 lb-1  | leader   | 10.0.0.11 | up    | v0.1.0  | -
 lb-2  | director | 10.0.0.12 | up    | v0.1.0  | 0.3 s ago
 lb-3  | director | 10.0.0.13 | up    | v0.1.0  | 0.2 s ago
 web-1 | backend  | 10.0.0.21 | up    | v0.1.0  | 1.1 s ago
 db-1  | backend  | 10.0.0.31 | up    | v0.1.0  | 1.9 s ago
(5 rows)

 pending | role     | address   | mac               | node fingerprint | waiting
---------+----------+-----------+-------------------+------------------+---------
 lb-4    | director | 10.0.0.14 | 52:54:00:3a:19:04 | 7c41e0a2f39b55d1 | 2 min
 web-2   | backend  | 10.0.0.22 | 52:54:00:3a:19:22 | 0be98d17c46a2f83 | 15 s
(2 rows)

 vip        | purpose | held by | since               | services
------------+---------+---------+---------------------+----------
 10.0.0.10  | control | lb-1    | 2026-11-09 10:02:13 |        -
 192.0.2.10 | service | lb-1    | 2026-11-09 10:02:14 |        2
(2 rows)

 service | vip        | port     | scheduler | method | backends up
---------+------------+----------+-----------+--------+-------------
 web     | 192.0.2.10 | tcp/443  | mh        | direct | 1 of 1
 db      | 192.0.2.10 | tcp/5432 | mh        | direct | 1 of 1
(2 rows)

 service | vip             | backend | at             | checks  | weight | connections | packets | bytes  | last check-in
---------+-----------------+---------+----------------+---------+--------+-------------+---------+--------+---------------
 web     | 192.0.2.10:443  | web-1   | 10.0.0.21:8443 | ok / ok |      1 |         412 |  918244 | 1.1 GB | 1.1 s ago
 db      | 192.0.2.10:5432 | db-1    | 10.0.0.31:5432 | ok / ok |      1 |          37 |   40116 | 9.8 MB | 1.9 s ago
(2 rows)
```

`--json` prints the same, one key per table, each a list of rows; times are RFC 3339, and every "ago" is also a number of seconds:

```json
{
  "cluster": { "name": "prod", "id": "7f3c91a2", "created": "2026-11-09T10:02:11Z", "leader": "lb-1", "term": 4, "leader_since": "2026-11-09T10:02:13Z" },
  "nodes": [
    { "node": "lb-1", "role": "leader", "address": "10.0.0.11", "state": "up", "version": "v0.1.0", "last_check_in": null, "last_check_in_seconds": null },
    { "node": "web-1", "role": "backend", "address": "10.0.0.21", "state": "up", "version": "v0.1.0", "last_check_in": "2026-11-09T10:40:02Z", "last_check_in_seconds": 1.1 }
  ],
  "pending": [ { "pending": "lb-4", "role": "director", "address": "10.0.0.14", "mac": "52:54:00:3a:19:04", "node_fingerprint": "7c41e0a2f39b55d1", "waiting_seconds": 120 } ],
  "vips": [ { "vip": "192.0.2.10", "purpose": "service", "held_by": "lb-1", "since": "2026-11-09T10:02:14Z", "services": 2 } ],
  "services": [ { "service": "web", "vip": "192.0.2.10", "protocol": "tcp", "port": 443, "scheduler": "mh", "method": "direct", "backends_up": 1, "backends": 1 } ],
  "backends": [ { "service": "web", "vip": "192.0.2.10", "port": 443, "backend": "web-1", "address": "10.0.0.21", "app_port": 8443, "own_check": "ok", "director_check": "ok", "weight": 1, "connections": 412, "packets": 918244, "bytes": 1181116006, "last_check_in_seconds": 1.1 } ]
}
```

**On a director**, `status` shows the whole cluster, as above - everything except secret material: no secret and no key, ever. **Only an approved director can tell which service runs where**: on which node's address and port, behind which VIP, and how much traffic it has been sent - `checks` is the backend's own check and the director's, `packets` and `bytes` what IPVS forwarded to it (direct return means the replies never pass the director, so there is no outbound count). A director that is not the leader asks the leader for what only the leader knows (backends' last check-ins), and says so if it cannot reach it. A secret is shown only by `thelb admin secret show`, and only on a director.

**On a backend**, `status` shows what that backend is, and nothing about the rest of the cluster: that it is a backend, the cluster's name, the services it has registered with the directors - by their names only, not their VIPs - and its node fingerprint.

```text
$ sudo thelb status
 role    | cluster | services | node fingerprint
---------+---------+----------+------------------
 backend | prod    | web, db  | 0be98d17c46a2f83
(1 row)
```

`--json` gives the same row: `{ "role": "backend", "cluster": "prod", "services": ["web", "db"], "node_fingerprint": "0be98d17c46a2f83" }`. While the backend waits for approval, its services are empty, and its fingerprint is the one a director's `pending` table shows for it.

### The local socket

`thelb status`, `doctor` and `admin` do not read the running theLB's memory: they ask it, over a **Unix socket**, `/run/thelb/thelb.sock`, and it answers.

- **Only what each command shows crosses it.** The daemon answers requests - the tables `status` shows, the checks `doctor` makes, the changes `admin` asks for - and never exposes its internals: no keys, no secrets except where `admin secret show` exists to print one, no Raft log.
- **Root only.** The socket is `0600`, in a directory only root can enter, and the kernel tells the daemon who is calling (`SO_PEERCRED`): a request from anything but effective user ID 0 is refused, whatever the file's permissions.
- **One theLB per node.** A second `thelb director` or `thelb backend` on the same node finds the socket answering, and stops: theLB is already running on this node. A socket file left by a crash - nothing listening - is removed and replaced.
- **Nothing left behind**: `/run` is cleared at boot, and theLB removes the socket when it stops.
- **In Kubernetes**, `kubectl exec` into an agent's pod and `thelb status` there reaches that agent the same way.

**Why not shared memory**: a shared memory segment is readable in full by whoever can map it, so it cannot answer one caller less than another, nor know who is reading; readers need locking against a writer mid-update; its layout ties the reader's version to the writer's; and a segment left by a crash stays in `/dev/shm` until reboot. A socket answers each caller with exactly what it may see, and its requests can be versioned like any API.

### Configuration

Optional: everything above works from the command line. A YAML file that the user writes (theLB never writes it) holds what should come back after a restart:

```yaml
# a director
director:
  control: { vip: 10.0.0.10, port: 7443 }
  bootstrap: true                                  # where wanted
  secret_file: /etc/thelb/director.secret          # written at the first bootstrap if missing
  backend_secret_file: /etc/thelb/backend.secret   # the same; the leader checks backends with it
  enrollment: { backend_networks: [10.0.0.0/24, fd00::/64] }
services:
  - { name: web, vip: 192.0.2.10, protocol: tcp, port: 443, scheduler: mh, allow: ["backend:web-*"] }
  - { name: db, vip: 192.0.2.10, protocol: tcp, port: 5432, allow: ["backend:db-*"] }
pools:
  - { name: apps, addresses: [192.0.2.64/27], delegate_to: ["backend:app-*"] }
```

```yaml
# a backend
backend:
  name: web-1
  join: 10.0.0.10:7443
  secret_file: /etc/thelb/backend.secret
serve:
  - { service: web, port: 8443, check: { http: /healthz } }
```

## What `thelb generate` prints

Nothing here is written by theLB. The user decides whether to install it, and where.

| Command | Prints |
|---|---|
| `generate secret` | A new random secret (256 bits), for a user who wants to create a role's secret file before the first bootstrap. |
| `generate systemd` | A unit file: `Type=notify`, `WatchdogSec=`, `Restart=always`, running as root with `CapabilityBoundingSet=` limited to the capabilities theLB needs, `NoNewPrivileges=yes` and the other hardening settings that do not stop it working - but not `ProtectKernelTunables=`, which would make `/proc/sys` read-only. |
| `generate sysctl` | A `sysctl.d` file with the parameters this node's roles need, for the user who wants them set from boot. |
| `generate modules` | A `modules-load.d` file with the modules this node needs. |
| `generate k8s` | The manifests in [Kubernetes](#the-manifests), as one YAML stream. |
| `generate config` | The running services, pools and attachments as a configuration file - so what was set up from the command line can be kept, if the user chooses. |

## Observability

- **Logs**: one line per event, saying what happened, what changed and from what - in the style of shint's log lines. Never a key or a secret.
- **Metrics**: Prometheus, on a configurable address: VIP ownership, backend health, IPVS counters per service and backend (connections, packets, bytes), cluster state, failovers, enrolments, drift corrections, conflicts.
- **`thelb status`** is the one view of the cluster - see [above](#thelb-status). It, `doctor` and `admin` talk to the running theLB over [the local socket](#the-local-socket).

## Security

- **Mutual TLS everywhere**: between directors, and between backends and the control VIP. Certificates come from the cluster's own CA, created at bootstrap and kept in memory on the directors.
- **Joining needs the role's secret**, proven over the TLS connection and never sent; directors and backends have separate secrets, both the user's files, rotatable - and every new director and backend also waits for an existing director's approval.
- **Backends are checked by address and MAC address**, pinned once enrolled, allowed only the services their certificate's identity is allowed, and cut off when their lease or certificate lapses.
- **Revocation**: removed members' certificate serials are denied until they would have expired; backend certificates last 24 hours.
- **Privileges**: theLB runs only as root (effective user ID 0), and every command checks it; under systemd its capability bounding set is limited to what it needs, so root does not mean every capability. In Kubernetes the agent must be privileged (kernel parameters are writable only so); the `thelb` namespace isolates it.
- **No secrets in logs, `status` or standard output**, except the output of `generate secret` and `admin secret show|rotate`, which exist to print one; keys never. Secrets are read only from files, never from the command line.
- **The secret files** are the one thing theLB writes, at the first bootstrap only, if missing, mode `0600`, never overwritten.
- **A security policy from the first release**: supported versions, private reporting, how dependencies' fixes are taken - as shint's `SECURITY.md` does - and `govulncheck` gating every release.

## Failure modes

| Failure | What theLB does |
|---|---|
| The leader dies | The others elect a leader within the election timeout; it takes the control VIP and the service VIPs, announces them, programs IPVS - with the same CA. Backends reconnect to the control VIP. Flows continue where `sloppy_tcp` and Maglev allow. |
| Network partition | The side with the majority keeps or takes the VIPs; the other side gives them up when its lease expires. With no majority anywhere, no one holds them. |
| Every director stops | The cluster's state is gone. The bootstrap director creates a new one, with a new CA, and every node joins it again by itself, with its secret. See [When every director stops](#when-every-director-stops). |
| The bootstrap director restarts while the cluster runs | It finds the cluster on the control VIP and joins it, with its secret. |
| The bootstrap director is gone for good | The cluster carries on; after a full stop, any other director with `--bootstrap` creates it again - with the same secret. |
| A node's secret is out of date | It stops, and says how to get the current one (`admin secret show` on a director). |
| A secret leaks | `admin secret rotate`, copy the new file to every node, `admin secret retire`; remove any member that should not be there. |
| Two clusters on one control VIP | The younger stands down. |
| A backend fails | Weight 0 after the failure threshold; back when healthy. |
| A backend stops reporting | Weight 0 when its lease expires, whatever its port says. |
| Every backend of a service fails | The service stays, reported unhealthy; the VIP's other services are unaffected. |
| A machine pretends to be a backend | It has no certificate, or its address or MAC address does not match its enrolment: refused. |
| Something on the network answers ARP for a backend's address | The director's pinned neighbour entry ignores it, and the watchdog logs the attempt. |
| Someone flushes IPVS or removes a VIP | The watchdog loop puts it back within its interval, and logs it. |
| theLB crashes | The VIPs stay on the interface until it restarts; the lease expiry makes another director take over meanwhile; on restart, theLB adopts or removes what it finds. |
| The process hangs | Under systemd, the watchdog restarts it; its lease expires either way. |
| Another system claims a VIP | Logged and counted as a conflict; theLB does not fight over it. |
| The Kubernetes API is unreachable | Current VIPs and backends keep working; no new allocations until it is back. |
| A kernel module is missing and cannot be loaded | Preflight fails; theLB does not start. |
| A switch ignores gratuitous ARP | The announcements repeat; `doctor` can test a link's behaviour. |

## Testing

- **Unit tests** for everything that is not the kernel: planning, the Raft state machine and its entries, authorisation, enrolment checks, VIP assignment, configuration, health-check logic.
- **A network lab in network namespaces**: a bridge, veth pairs, a client, three directors and several backends, IPv4 and IPv6 - a real L2 cluster on one machine, in CI too (GitHub's runners allow `sudo`).
- **Chaos tests in the lab**: kill the leader, partition the directors, restart the bootstrap director, stop every director, flap a backend, flush IPVS, remove a VIP, impersonate a backend, answer ARP for one; measure failover time and how many flows survive.
- **Kubernetes** in [kind](https://kind.sigs.k8s.io/), with kube-proxy in each of its modes.
- **Kernels**: the lab under QEMU on each supported kernel series - RHEL 9's 5.14 (from a Rocky or AlmaLinux 9 image), 6.1, and the newest long-term kernel - so both sides of every [kernel adaptation](#kernels) are tested.
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
| 0 - spike | On one machine, by hand-written code: an IPVS direct-routing service over netlink, a VIP added and announced, backends prepared, a port mapped in nftables - IPv4 and IPv6, on RHEL 9's kernel and a current one. Settles the kernel questions: IPv6 neighbour solicitations for a VIP on `thelb0`, probing for `IFA_PROTO` and `ip_vs_mh`, Maglev and source hashing on failover, pinned neighbour entries. |
| 1 - one director | `thelb director --bootstrap` with the control VIP, the secret files and its CA; backends enrolling with their secret, attaching and mapping ports; services from the configuration and `admin`; health checks from both sides; `status`, `doctor`, `generate`; the watchdog loop; IPv4 and IPv6; the network lab in CI. |
| 2 - the cluster | Directors joining with their secret; Raft with the replicated CA; the bootstrap rule and restarts; failover; fail closed; leadership hand-off on stop; chaos tests. |
| 3 - Kubernetes | The provider; the agent DaemonSet; shared VIPs; `generate k8s`; kind tests. |
| 4 - tunnelling | IP-in-IP forwarding, for backends in other subnets. |
| 5 - active-active | Service VIPs spread across directors; optional IPVS connection synchronisation. |
| 6 - proxy | A proxying mode, as a separate capability, for networks where direct return is not possible. |

## Open questions

1. **How theLB runs on a dedicated Kubernetes director node**: as a second DaemonSet, `thelb-director`, with a node selector and the in-cluster credentials, or under systemd as on any machine. A later option would let directors be ordinary nodes: theLB as the service proxy for its own Services (`service.kubernetes.io/service-proxy-name`), programming both ends itself instead of kube-proxy - a large piece of work, not for the first releases.
2. **Address pools**: the [proposal](#address-pools) - who may draw from a pool, ranges and left-out addresses, default pools, the order addresses are handed out in (lowest free first, or spread out, for IPv6 above all), and the annotations' names.
3. **The tunnel's encapsulation**: plain IP-in-IP (and its IPv6 equivalent), or GUE, which network equipment can balance by its UDP ports.

### Decided

- **Names: the domain `dmarts.app`**, in DNS order and lowercase: the Service class `dmarts.app/thelb`, annotations under `dmarts.app/` (`dmarts.app/thelb-pool`, `dmarts.app/thelb-shared-vip`, ...), and custom resources, when there are any, in the API group `thelb.dmarts.app` (`ippools.thelb.dmarts.app`, ...). Lowercase because Kubernetes matches the class exactly, and namespace and CRD names must be lowercase anyway.
- **Raft's state is kept in memory only** - no data directory, no opt-in: every start is a new member identity ([The cluster](#the-cluster-and-what-raft-carries)).
- **RHEL 9 is supported**, so the minimum kernel is 5.14; theLB probes the kernel's features rather than trusting its version, and adapts where a newer feature is missing ([Kernels](#kernels)).
- **Directors bootstrap from the command line**, all with the same command - the control VIP, its port and the secret files - and one or more also with `--bootstrap`. The first bootstrap creates the secrets and writes them to the named files, once, if missing; the user copies them to the other nodes; a node whose secret the cluster does not accept stops as out of date; the CA is created in memory by the bootstrapping director and replicated to every director, so a failover keeps it; after a full stop the cluster is created again and every node rejoins by itself ([Bootstrapping and joining](#bootstrapping-and-joining-directors)).
- **Every node that joins waits for a director's approval** - directors and backends alike; any approved director may approve; backends cannot; pending nodes appear in `thelb status` on any director. **An approved node rejoins after a restart without approval**, recognised by its node fingerprint, address and MAC address ([Approval](#approval)).
- **Root only**: every command runs with effective user ID 0, or not at all - except the help and `--version`, which any user may run.
- **The node fingerprint**: a hash of the machine's application-specific ID, its SMBIOS UUID where there is one, and the MAC address of the interface it joins through ([The node fingerprint](#the-node-fingerprint)).
- **After a full stop, every node is approved again**; `admin approve --match <regex>` approves many at once.
- **SCTP is in the first release**, alongside TCP and UDP.
- **Address pools are theLB's own**, not Kubernetes objects; how they work is still open ([Address pools](#address-pools)).
- **A director may be a Kubernetes node only if it is dedicated, without kube-proxy** - kube-proxy would take the VIP's packets before IPVS ([Kubernetes](#which-services-and-which-backends)).
- **What `status` shows**: on a director, everything but secret material, including which service runs on which node's address and port behind which VIP, and the traffic sent to it; on a backend, only its role, the cluster's name, its services by name and its node fingerprint.
- **The local socket**: `status`, `doctor` and `admin` ask the running theLB over a Unix socket that authorises each caller and exposes only what each command shows - not shared memory ([The local socket](#the-local-socket)).
- **The control VIP**, held by the leader, is where backends and the command line talk to the cluster; **backends are clients, not Raft members**.
- **Backends enrol and prove themselves** with the backends' secret, then certificates from the cluster's CA, their address and MAC address, allow-lists and a heartbeat lease ([Backends](#backends-enrolment-trust-and-services)).
- **The director never changes the port; a backend maps it**, in theLB's own nftables table, when its application's port differs ([Port mapping](#port-mapping-on-the-backend)).
- **One VIP, many services**, each with its own backends; in Kubernetes, Services share a VIP by annotation.
- **Direct routing first, IP-in-IP tunnelling second**, as a per-service forwarding method; proxying later.
