---
title: How it works
lead: The pieces inside theLB, and how a packet, a change and a failover move through them.
description: theLB's architecture - directors, Raft, the control VIP and its lease, IPVS, the backend agent, health checks, the control protocol and the source layout.
section: Technical
order: 2
nav: How it works
---

## The big picture

theLB has a **control plane** and a **data plane**:

- The **data plane** is the Linux kernel: IPVS forwards packets, and ARP or neighbour discovery tell the network where each VIP is. theLB never touches a packet itself.
- The **control plane** is theLB: the directors agree on what the kernel should do, the leader does it, and every node checks that it stays done.

:::html
<div class="diagram">
<svg viewBox="0 0 660 300" role="img" aria-label="Three directors in a Raft cluster; the leader holds the VIPs and programs IPVS; backends check in with the leader over mutual TLS">
  <defs><marker id="arr" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse"><path d="M0 0L10 5L0 10z"/></marker></defs>
  <rect class="box accent" x="30" y="30" width="170" height="78" rx="10"/>
  <text class="h" x="115" y="56" text-anchor="middle">Director 1 - leader</text>
  <text class="sub" x="115" y="75" text-anchor="middle">VIPs (leased) · IPVS</text>
  <text class="sub" x="115" y="92" text-anchor="middle">checks · decisions</text>
  <rect class="box" x="245" y="30" width="170" height="78" rx="10"/>
  <text class="h" x="330" y="56" text-anchor="middle">Director 2</text>
  <text class="sub" x="330" y="75" text-anchor="middle">follows · shares checks</text>
  <text class="sub" x="330" y="92" text-anchor="middle">sync backup</text>
  <rect class="box" x="460" y="30" width="170" height="78" rx="10"/>
  <text class="h" x="545" y="56" text-anchor="middle">Director 3</text>
  <text class="sub" x="545" y="75" text-anchor="middle">follows · shares checks</text>
  <text class="sub" x="545" y="92" text-anchor="middle">sync backup</text>
  <path class="arrow" d="M200 69H243"/>
  <path class="arrow" d="M200 60C260 8 460 8 500 28"/>
  <text class="sub" x="330" y="135" text-anchor="middle">Raft: every change is agreed by a majority, then applied</text>
  <rect class="box" x="60" y="200" width="150" height="70" rx="10"/>
  <text class="h" x="135" y="226" text-anchor="middle">Backend</text>
  <text class="sub" x="135" y="245" text-anchor="middle">thelb0 · own checks</text>
  <text class="sub" x="135" y="260" text-anchor="middle">heartbeat every 2 s</text>
  <rect class="box" x="255" y="200" width="150" height="70" rx="10"/>
  <text class="h" x="330" y="226" text-anchor="middle">Backend</text>
  <text class="sub" x="330" y="245" text-anchor="middle">port mapping (nftables)</text>
  <text class="sub" x="330" y="260" text-anchor="middle">heartbeat every 2 s</text>
  <rect class="box" x="450" y="200" width="150" height="70" rx="10"/>
  <text class="h" x="525" y="226" text-anchor="middle">Backend</text>
  <text class="sub" x="525" y="245" text-anchor="middle">thelbt0 (IP-in-IP)</text>
  <text class="sub" x="525" y="260" text-anchor="middle">heartbeat every 2 s</text>
  <path class="arrow" d="M135 198L115 110"/>
  <path class="arrow" d="M330 198L140 110"/>
  <path class="arrow" d="M525 198L170 110"/>
  <text class="sub" x="330" y="168" text-anchor="middle">backends talk to the control VIP: mutual TLS 1.3</text>
</svg>
</div>
:::

## A packet

1. A client sends a packet to a service VIP. The leader holds the VIP on its interface and has announced it, so the network delivers the packet to the leader.
2. IPVS on the leader matches the VIP, protocol and port to a service, and picks a backend with the service's scheduler (weighted round robin by default). An existing connection keeps its backend.
3. **Direct routing**: IPVS sends the packet, unchanged, to the backend's MAC address. **IP-in-IP**: it wraps the packet in a new IPv4 header addressed to the backend.
4. The backend has the VIP on `thelb0`, a dummy interface that never answers ARP, so it accepts the packet as its own. A tunnelled packet is unwrapped on `thelbt0` first. If the application listens on another port, nftables on the backend changes the port.
5. The application answers. The reply goes from the VIP's address straight to the client - through the backend's own router, never back through the director.

## A change

1. `sudo thelb admin service add ...` talks to the local director over a Unix socket (`/run/thelb/director/`), which only root can use.
2. A director that is not the leader passes the change to the leader, over mutual TLS.
3. The leader checks the change (is the VIP free on the network? does the backend offer that port? can the director reach the backend's network?), then puts it in the Raft log.
4. Once a majority of directors have the entry, every director applies it to its copy of the cluster's state.
5. The leader turns the new state into a **plan** - the VIPs, IPVS services and backends, weights, neighbour entries - and changes only what the plan changed: IPVS first, then the VIP.

## The VIPs and the lease

The leader adds every VIP with a kernel address lifetime - the **lease**, 6 seconds by default - and renews it every third of that, many VIPs per netlink message. If theLB is killed or hangs, the kernel removes the VIPs by itself when the lease runs out. The lease is part of the Raft state, so every director knows how long an old leader's VIP can still answer.

A new leader takes a VIP only when two looks, one second apart, find nothing answering for it: no ARP or neighbour reply, and for the control VIP, nothing accepting a TCP connection. With two directors, the survivor waits for the lease plus 2 seconds before it even looks; the director whose identity sorts later waits 6 seconds more.

While it leads, the leader watches the control VIP every 5 seconds. If another machine answers for it, the leader announces itself again - or, if the other is a second leader whose identity sorts first, it stands down.

## Health checks

- Each **backend** checks its own applications every 2 seconds and reports in its heartbeat.
- The **directors** check each application from the outside. The leader shares the checks among the directors with rendezvous hashing, so a director coming or going moves only its own share. Each director reports its results with its check-in, every second. The leader runs a check itself before the first result arrives, and whenever a result is more than 3 seconds old.
- One check of an application (address, port and kind) serves every service it is assigned to.
- The leader gives a backend weight 0 when its heartbeat is late, when either check fails, or when it does not yet hold the VIP.

## Joining and trust

- A new node proves it knows its role's secret, and sends its **node fingerprint**. A director approves it by hand. The node then gets a certificate from the cluster's certificate authority, which lives only in the directors' memory.
- All node-to-node traffic is TLS 1.3 with certificates on both sides. `admin remove` revokes a certificate.
- An approved node rejoins after a restart without approval, as long as its fingerprint, address and MAC address are the same.

## The source

theLB is written in Go. Its only direct dependencies are HashiCorp's Raft and its logger. It talks to the kernel itself, over netlink - no `ip`, `ipvsadm` or `nft` commands.

| Package | What it does |
|---|---|
| `cmd/thelb` | the program's entry point |
| `internal/cli` | the command line, help and `generate` |
| `internal/director`, `internal/backend` | the two roles |
| `internal/cluster`, `internal/raftlog` | what Raft carries, and Raft's log lines |
| `internal/dplane`, `internal/ctlnet` | the leader's data plane, and a director's network (the control VIP, its lease) |
| `internal/backnet`, `internal/nft` | a backend's kernel state, and its nftables table |
| `internal/ipvs`, `internal/netlink`, `internal/announce` | IPVS, rtnetlink and ARP / neighbour discovery, by hand |
| `internal/wire`, `internal/join`, `internal/api`, `internal/ca` | the protocol between nodes, joining, the messages, and the certificate authority |
| `internal/check`, `internal/preflight`, `internal/doctor` | health checks, preflight, and `doctor` |
| `internal/status`, `internal/table`, `internal/localsock` | `thelb status`, its tables, and the local socket |
| `internal/metrics`, `internal/logx`, `internal/alive`, `internal/sdnotify` | metrics, logging, the watchdog and systemd notifications |
| `internal/secret`, `internal/fingerprint`, `internal/sysctl`, `internal/hostnet` | secrets, fingerprints, kernel settings, this machine's interfaces |

## The full design

This page is the short version. The full design - every decision and its reasons, Kubernetes, and the questions still open - is in the repository:

- [docs/design.md](https://github.com/dmartsapp/thelb/blob/main/docs/design.md) - the design;
- [docs/diagrams.md](https://github.com/dmartsapp/thelb/blob/main/docs/diagrams.md) - the design in pictures;
- [docs/build-state.md](https://github.com/dmartsapp/thelb/blob/main/docs/build-state.md) - where the code is, and what comes next.
