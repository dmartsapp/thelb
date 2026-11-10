# theLB user manual

How to install theLB, build a cluster, serve services through it, and keep it running. This manual describes **this build** (v0.1.0-dev); what the design plans but this build does not have yet is listed at the end. For how theLB works inside, see the [design](design.md).

**Contents**

1. [What theLB is](#1-what-thelb-is)
2. [Requirements](#2-requirements)
3. [Installing](#3-installing)
4. [Quick start: one director, two backends](#4-quick-start-one-director-two-backends)
5. [More directors, for failover](#5-more-directors-for-failover)
6. [Running under systemd](#6-running-under-systemd)
7. [The secret files](#7-the-secret-files)
8. [Approving nodes](#8-approving-nodes)
9. [Pools, services and assignments](#9-pools-services-and-assignments)
10. [Backends](#10-backends)
11. [Forwarding: direct routing or IP-in-IP](#11-forwarding-direct-routing-or-ip-in-ip)
12. [A director's host as a backend too](#12-a-directors-host-as-a-backend-too)
13. [Reading `thelb status`](#13-reading-thelb-status)
14. [What theLB changes on a machine](#14-what-thelb-changes-on-a-machine)
15. [Logs](#15-logs)
16. [When things fail](#16-when-things-fail)
17. [Troubleshooting](#17-troubleshooting)
18. [Command reference](#18-command-reference)
19. [Trying it on one machine](#19-trying-it-on-one-machine)
20. [Not in this build yet](#20-not-in-this-build-yet)

## 1. What theLB is

theLB balances TCP, UDP and SCTP traffic sent to a virtual IP address - a **VIP** - across a set of servers, using the Linux kernel's own load balancer, IPVS. The servers answer the clients directly: replies never pass back through the load balancer.

Every machine runs the same binary, `thelb`, in one of two roles:

- **Directors** receive the clients' traffic and forward it. They form a cluster with Raft; one of them, the **leader**, holds the VIPs, programs IPVS and makes every decision. One director works; **three are recommended**, so that one can fail.
- **Backends** run your applications. Each one **offers** its applications to the cluster - each under a name the backend gives it, at a protocol and a port: `shop` at `tcp/8080` - when it starts, or later, once the application is up. The leader checks that an application answers before it records the offer, and the backend keeps checking its own applications.

On a director you create **pools** of VIP addresses, **services** (a VIP, a protocol and a port, `198.18.0.20 tcp/80`), and **assign** backends' offers to them. A backend never chooses a service: a director decides.

The cluster has its own address too, the **control VIP**, held by the leader: backends, and directors joining the cluster, talk to it on the **control port**, with mutual TLS.

## 2. Requirements

- **Linux**, 64-bit (amd64 or arm64). theLB is tested on Debian 13 (kernel 6.12); it targets kernels from 5.14 (RHEL 9) on.
- **Root** on every node: every command but `--help` and `--version` runs only as root.
- **Kernel modules**: `ip_vs` and its schedulers (`ip_vs_wrr` for the default) on directors; `dummy` and `nf_tables` with its NAT support on backends; `ipip` on backends of tunnelled services. The kernel loads them on demand where they are installed.
- **The network**:
  - Directors share an L2 network with the control VIP - the VIP must be an unused address in a network the directors have an interface in.
  - Backends of **directly routed** services share an L2 network with the directors. Backends of **tunnelled** services only need to be routed to.
  - Open, in firewalls: the control port (TCP) to the control VIP and between directors; the services' ports to the VIPs; the applications' ports from directors to backends (health checks); IP protocol 4 from directors to backends of tunnelled services; **multicast to `224.0.0.81`, UDP port `8848`, between directors** on the control VIP's interface - IPVS's connection synchronisation, which needs an IPv4 address on that interface.
  - **The directors' network must be trusted**: the kernel's connection-synchronisation messages are not authenticated, so a machine on that network can add entries to the directors' connection tables. Keep the directors on a network of their own, or one only trusted machines share.
- **IPv4 and IPv6**: control VIPs and directly routed services may be either. Tunnelled services are IPv4 in this build. IPv6 is not yet tested end to end.

## 3. Installing

There are no packages yet. Build the binaries with Go 1.27.3 or later:

```text
make build        # bin/thelb.linux.amd64 and bin/thelb.linux.arm64
```

Copy the binary for your machines to each of them, as `/usr/local/bin/thelb`, and check it:

```text
thelb --version   # thelb 0.1.0-dev (commit ..., go1.27.3 linux/amd64)
```

theLB writes nothing to disk except the two secret files at the first bootstrap, and reads no configuration file: everything is a command-line flag, or the cluster's state, changed with `thelb admin`.

## 4. Quick start: one director, two backends

The example network: directors and backends on `10.0.0.0/24`, the control VIP `10.0.0.10`, port `7443`, service VIPs from `10.0.0.20`.

**1. Start the first director**, with `--bootstrap`, on `lb-1` (`10.0.0.11`):

```text
sudo thelb director --vip 10.0.0.10 --port 7443 --bootstrap \
  --secret-file /etc/thelb/director.secret --backend-secret-file /etc/thelb/backend.secret
```

It looks at the control VIP for 5 seconds, finds nothing, and creates the cluster - and, because the files do not exist yet, both secret files, readable by root only. It then holds the control VIP. Leave it running (section 6 runs it under systemd instead).

**2. Copy the backends' secret** to each backend, readable by root only:

```text
sudo install -o root -m 0600 backend.secret /etc/thelb/backend.secret   # on web-1 and web-2
```

**3. Start each backend**, here with nginx on port 80:

```text
sudo thelb backend --join 10.0.0.10:7443 --secret-file /etc/thelb/backend.secret \
  --offer web=tcp/80,check=http:/
```

It joins and waits for approval: its log says `waiting for approval`, with its node fingerprint. (Without `--offer`, it joins offering nothing; offer an application later with `thelb offer add` - section 10.)

**4. Approve them**, on the director - after checking that the fingerprints are the ones the backends show:

```text
sudo thelb status backends                        # the pending table, with fingerprints
sudo thelb admin approve --match 'web-[0-9]+'     # shows the matching nodes, asks y/N
```

**5. Create a pool, a service, and assign the offers**:

```text
sudo thelb admin pool add main --vip 10.0.0.20/24 --vippool-count 10
sudo thelb admin service add web --pool main --proto tcp --port 80
sudo thelb status offers        # each backend's offer "web", unassigned
sudo thelb admin assign web web-1:web web-2:web
```

The service takes the pool's lowest free address, `10.0.0.20`. Within a few seconds both backends pass their checks and get traffic, and the leader brings the VIP up - once IPVS has a backend for it:

```text
sudo thelb status vips          # the VIPs, the service: "2 of 2" backends up
curl http://10.0.0.20/          # from a client
```

## 5. More directors, for failover

Copy **both** secret files to each new director, readable by root only, and start it with the same command - with or without `--bootstrap`:

```text
sudo thelb director --vip 10.0.0.10 --port 7443 \
  --secret-file /etc/thelb/director.secret --backend-secret-file /etc/thelb/backend.secret
```

It finds the cluster on the control VIP and waits for approval; approve it on a director (`sudo thelb admin approve lb-2`). It then follows the leader, is promoted to a voting member once it has caught up, and takes over if the leader fails.

- **A planned stop takes a director out of the vote.** Stopped with `SIGTERM`, `Ctrl+C` or `SIGHUP`, a director - after handing leadership over, if it led - has the leader take its member identity out of Raft, so the directors left keep their majority without it. It stays approved, and rejoins without approval when it starts again. `thelb status directors` shows it as `stopped`, not `down`.
- **Two directors survive either one's planned stop, and either one's crash.** A crashed director still counts towards the majority, so the one left cannot win an election. After 3 seconds without a leader (6 for the director whose member identity sorts later), it looks: if the other is **gone** - its control port refuses connections (its theLB is not running), or its machine does not answer at all - and nothing but the crashed director's leftover holds the control VIP, and its own gateway still answers, it **stands alone**: it goes on as the cluster's only voter, with every pool, service and assignment, takes the control VIP, and removes the other. The crashed director, started again, **waits for approval** (`thelb admin approve <name>`). If the other answers on the network but its control port does not - a firewall, a partition - neither stands alone, and the cluster has no leader until the two reach each other again: guessing there could give two leaders.
- **Under systemd with two directors, use `RestartSec=10`**: a crashed director restarted at once may create a cluster of its own before the other has stood alone (section 6).
- **Use three directors** (or five) to survive a crash: three keep their majority when any one fails.
- **`--bootstrap`** on a director lets it create the cluster when it finds none - after looking for 5 seconds. Put it on the first director; you may put it on others too, so that the cluster does not depend on one machine coming back after every director has stopped.
- **A director that restarts** while the cluster runs rejoins by itself, without approval: the cluster remembers it, by its node fingerprint, address and MAC address.
- **Connections in progress survive a failover.** The leader runs IPVS's connection-synchronisation daemon as master on the control VIP's interface, sending its connection entries by multicast; every director runs one as backup, taking them in. The director that takes over already knows which backend each connection goes to. The sync ID comes from the cluster's ID, so two clusters on one network do not mix. The roles follow leadership, with no setting to make.

## 6. Running under systemd

theLB tells systemd when it has started (`Type=notify`) and that it is alive (`WatchdogSec=`) - only while the loops that keep it working go round: on a director, its check-ins with the leader and, while it leads, the data plane; on a backend, its heartbeat and the repair of its kernel state (section 14). A loop that has not gone round for 30 seconds stops the telling, and systemd restarts theLB - with `Restart=always`, a hung theLB comes back by itself. It installs no unit files itself (a `thelb generate systemd` that prints them is planned); write them yourself. A director:

```ini
# /etc/systemd/system/thelb-director.service
[Unit]
Description=theLB director
After=network-online.target
Wants=network-online.target

[Service]
Type=notify
NotifyAccess=main
ExecStart=/usr/local/bin/thelb director --vip 10.0.0.10 --port 7443 --bootstrap \
  --secret-file /etc/thelb/director.secret --backend-secret-file /etc/thelb/backend.secret
Restart=always
RestartSec=10
WatchdogSec=30

[Install]
WantedBy=multi-user.target
```

A backend:

```ini
# /etc/systemd/system/thelb-backend.service
[Unit]
Description=theLB backend
After=network-online.target
Wants=network-online.target

[Service]
Type=notify
NotifyAccess=main
ExecStart=/usr/local/bin/thelb backend --join 10.0.0.10:7443 \
  --secret-file /etc/thelb/backend.secret --offer web=tcp/80,check=http:/
Restart=always
RestartSec=1
WatchdogSec=30

[Install]
WantedBy=multi-user.target
```

```text
sudo systemctl daemon-reload
sudo systemctl enable --now thelb-director     # or thelb-backend
journalctl -u thelb-director -f                # its log
```

The flags in a unit are the node's whole setup: there is no configuration file. A backend offers its `--offer` flags at every start; what it offers while it runs, with `thelb offer add`, the cluster keeps - a backend that starts again takes it up again. To withdraw an offer, use `thelb offer remove <name>`: taking a flag out of the unit does not withdraw what the cluster holds.

## 7. The secret files

The cluster has two secrets, one for directors and one for backends, each in a file of its own.

- **Created once**: the first `--bootstrap`, if a file is missing, writes a new secret to it - mode `0600`, owned by root - and never overwrites one. You may create them beforehand instead: `sudo thelb generate secret | sudo tee /etc/thelb/director.secret >/dev/null`, then `chmod 0600`.
- **Where they go**: `director.secret` to every director; `backend.secret` to every director and every backend.
- **Read only from files**, never from the command line; a file that anyone but root can read, or that is a symbolic link, is refused.
- **Never logged or printed**, except by `generate secret` and by `sudo thelb admin secret show --role director|backend` on a director - the way to repair a node whose file is out of date.
- **Not proof enough**: a node with the right secret may ask to join, and then waits for a director's approval.

After every director has stopped, the cluster is created again - with a new certificate authority, and **the same secrets**. Secret rotation is planned, not built.

## 8. Approving nodes

Every new director and backend waits for approval, by any director:

```text
sudo thelb status directors        # pending directors, with their fingerprints
sudo thelb status backends         # pending backends
sudo thelb admin approve web-1 web-2
sudo thelb admin approve --match 'web-[0-9]+'          # whole names, RE2 syntax; asks y/N
sudo thelb admin approve --match 'web-[0-9]+' --yes    # for scripts
sudo thelb admin refuse web-9      # it is told so, and stops
sudo thelb admin remove web-2      # its certificate is revoked; a backend stops, a director leaves
```

- **Compare the fingerprint** a pending node shows in its own log or `thelb status` with the one the director lists: it identifies the machine.
- **Approved once, a node rejoins by itself** after a restart, while the cluster runs - unless its fingerprint, address or MAC address changed.
- **After every director has stopped**, the cluster is new: every node waits for approval again (`approve --match` approves many at once).
- A pending request that stops asking is dropped after an hour.

## 9. Pools, services and assignments

### Pools

A pool is a range of addresses for services' VIPs:

```text
sudo thelb admin pool add main --vip 10.0.0.20/24 --vippool-count 10   # 10.0.0.20 - 10.0.0.29
sudo thelb admin pool add dmz --vip 192.168.50.10/24 --vippool-count 5 --interface eth1
sudo thelb admin pool remove dmz       # only when no service uses it
```

The first address and its network's prefix, and how many addresses. The range must lie inside the network, without its network or broadcast address, and must overlap no other pool, the control VIP or any node's address. The leader holds the pool's VIPs on its interface in that network - or the one `--interface` names. `--default` makes a pool the one used when a service names none.

### Services

```text
sudo thelb admin service add web --pool main --proto tcp --port 80
sudo thelb admin service add dns --pool main --vip 10.0.0.20 --proto udp --port 53    # the same VIP, another port
sudo thelb admin service add far --pool main --proto tcp --port 80 --method tunnel   # section 11
sudo thelb admin service remove dns
```

- **The VIP**: the pool's lowest free address, checked on the network (ARP) first - or the one `--vip` names, so that one VIP carries several services, each on its own port.
- **`--scheduler`**: `wrr` (weighted round robin, the default), `rr`, `wlc`, `lc`, `sh` or `mh`.
- **`--method`**: `direct` (the default) or `tunnel` - see section 11.

### Assignments

```text
sudo thelb status offers                                  # every backend's offers; "-" under service: unassigned
sudo thelb admin assign web web-1:shop web-2:shop --weight web-1=3
sudo thelb admin unassign web web-2
```

An offer is `node:name`, by the name its backend gave it - or `node:protocol/port`, as `web-1:tcp/8080`. The leader checks that the backend offers it, that its protocol is the service's, and - for a directly routed service - that the director has an interface in the backend's network. Weights are 0 to 65535, 1 by default; 0 sends no new connections. A backend withdrawing an offer ends its assignments; one that moves an offer to another port takes its assignments along.

### Port mapping

A backend's application may listen on another port than its service's: assign `web-1:tcp/8080` to a service on port 80, and the backend maps port 80 to 8080 itself, in its own nftables table - the director never changes the port. Each backend may use a port of its own: `web-1:tcp/8080 web-2:tcp/9090`.

### Keeping them: `thelb generate commands`

theLB keeps the cluster's state in memory only, and reads no configuration file, so after **every** director has stopped, the pools, services and assignments are gone. Keep them as a script, printed on any director:

```text
sudo thelb generate commands > /root/thelb-restore.sh     # again after every change you want kept
```

It holds the `thelb admin` commands that create them again - each service on the VIP it has now - and, as a comment, the `approve` command for the nodes that were members. After a full stop, once the cluster is created again:

1. approve the directors and the backends as they ask - the script's comment has the command; compare fingerprints first;
2. wait until `sudo thelb status offers` lists the backends' offers - each backend offers again, by itself, what it offered before;
3. run the script on a director: `sudo sh /root/thelb-restore.sh`.

theLB prints the script and never reads it. Keep it with your other backups; it holds no secret.

### Health

A backend gets traffic - its weight - only while all of these hold, and weight 0 otherwise (no new connections; open ones finish):

- its **heartbeat** reached the leader in the last 10 seconds (it sends one every 2 seconds);
- its **own check** of the application passes;
- it **holds the service's VIP**;
- the **leader's check** of the application, at the backend's address and the application's port, passes.

Checks run every 2 seconds, with a 1-second timeout: healthy after 2 successes in a row, unhealthy after 3 failures. Kinds: `check=tcp` (connect; TCP's default), `check=http:/path` (a GET, any 2xx or 3xx), `check=none` (UDP's and SCTP's default).

A backend whose check fails stays assigned: it gets no traffic until its checks pass again, and then gets it back by itself.

**The service's VIP** is held by the leader only while a backend serves one of the VIP's services: IPVS is programmed first, then the VIP comes up. A service with no backend serving has no VIP on the network - its address stays reserved in the pool - and when its last backend stops serving, the VIP goes first, then IPVS changes.

## 10. Backends

```text
sudo thelb backend --join <control VIP:port> --secret-file /etc/thelb/backend.secret \
  [--offer shop=tcp/8080,check=http:/healthz --offer dns=udp/53] [--name web-1] [--interface eth0]
```

- **`--join`** is the control VIP - an address or a name - and the control port. **`--name`** defaults to the hostname. **`--offer`** may be given any number of times, or not at all.

### Offers

An offer is one of the backend's applications: a **name** the backend chooses (letters, digits, `.`, `-`, `_`, up to 63), a protocol, a port, and a check. Each name, and each protocol and port, once per backend. A backend offers in two ways:

```text
--offer shop=tcp/8080,check=http:/healthz        # at start: named tcp-8080 without "shop="
sudo thelb offer add shop tcp/8080,check=http:/healthz     # while it runs, once the application is up
sudo thelb offer remove shop                               # withdrawn: its assignments end
```

- **The leader checks first**: before it records an offer, it runs the offer's check against the application, at the backend's address. `thelb offer add` prints the result, or why the leader refused - `the leader's check of shop (http /healthz) at 10.0.0.21:8080 fails: ...`. An `--offer` is offered once the backend's own check of it passes - so a backend may start before its applications - and, if the leader refuses it, again when its check passes again, or 10 seconds later.
- **Recorded, it is unassigned** until a director assigns it: `sudo thelb status offers` on a director lists every offer, its backend's own check, and its service - `-` while unassigned.
- **The backend checks it** every 2 seconds and reports with every heartbeat: assigned, it gets traffic only while its checks pass.
- **The cluster keeps the offers**: a backend that restarts takes up what the cluster holds for it, flags or not.
- **The backend keeps them too**: when every director has stopped and the cluster is created again, the backend enrols again by itself and - once approved - offers everything it offered before; nothing has to be done on the backend. The services and assignments are the directors' to create again (section 9, `thelb generate commands`).
- `thelb offer add` with the name of an offer already made, and another port or check, replaces it: its assignments follow it to the new port.
- **Your application** must listen on all addresses (`0.0.0.0`, `::`), or on the VIP: the VIP is added to the backend's own interface `thelb0`, not to `eth0`.
- **What it changes**: it creates `thelb0` (no ARP) and puts the VIPs of its services on it; it sets `net.ipv4.conf.all.arp_ignore=1` and `arp_announce=2` (unless already stricter), so it never answers ARP for a VIP; it maps ports, and for tunnelled services creates `thelbt0` and clamps TCP's MSS (section 11). All of it is removed, and the parameters restored, when it stops.
- **Stopping it** (`SIGTERM`, `systemctl stop`, `Ctrl+C`, or `SIGHUP` - a closed terminal or SSH session): it asks the leader to drain it, keeps its VIPs a moment longer so connections already sent to it are not lost, then removes everything.
- **Removed by a director**, it stops; if it starts again, it waits for approval.
- **`thelb status` on a backend** shows only that backend: its role, the cluster's name, its state, each offer - its name, port, check, its own check's state, whether the cluster holds it (`offered`), the service it is assigned to - never a VIP - and its node fingerprint.

## 11. Forwarding: direct routing or IP-in-IP

A service's `--method` decides how the director hands its packets to the backends. Either way the backend answers the client directly, from the VIP, and the client's address reaches the application.

| | `direct` (default) | `tunnel` |
|---|---|---|
| The director | rewrites the destination MAC address | wraps the packet in an outer IPv4 header to the backend (IP-in-IP) |
| Backends | on the director's L2 network | anywhere routed |
| Per packet | nothing added | 20 bytes |
| On the backend | the VIP on `thelb0` | also `thelbt0`, which unwraps the packets, and an MSS clamp |

**A tunnelled service's backends**:

- may be in other networks; a backend whose control VIP is behind a router joins through it - its log says it can serve only tunnelled services - and a director refuses to assign it to a directly routed service.
- unwrap the packets on `thelbt0`, theLB's own IPIP interface, which accepts them from any director. Reverse-path filtering is turned off on `thelbt0` - the unwrapped packets come from clients reached through another interface - and, if it was strict (`1`) for all interfaces, made loose (`2`); both are put back when the backend stops.
- announce a smaller TCP MSS in their handshakes - the interface's MTU less 60, 1440 on a 1500-byte network - so that the client's packets still fit once wrapped.
- **send their replies from the VIP's address through their own router**. A router or firewall that drops packets whose source is outside the network they come from (source-address validation, uRPF, cloud networks' source checks) drops them, and the service does not work: allow the VIPs as sources on the backends' networks.
- need IP protocol 4 let through from the directors, and the `ipip` module.

Tunnelled services are IPv4 in this build.

## 12. A director's host as a backend too

For small setups, a director's machine can also run your application. Start the director first; whenever you are ready, start a backend there too - the same command as any backend's, as a second systemd unit (`After=thelb-director.service`), with **a name of its own**:

```text
sudo thelb backend --join 10.0.0.10:7443 --secret-file /etc/thelb/backend.secret \
  --name lb-1-web --offer web=tcp/80,check=http:/
sudo thelb admin approve lb-1-web
sudo thelb admin assign web lb-1-web:web
```

- The two are separate daemons, with separate sockets: `thelb status` on that machine asks the director (and lists the machine twice: as a director and as a backend); `thelb status --role backend` asks the backend.
- A backend with its director's name is refused, and told to choose another.
- **Its offers must use the service's port**: no port mapping for a backend on a director's machine.
- While that director leads, IPVS hands the machine's own share to its own stack; while another leads, it sends it, like any backend.

## 13. Reading `thelb status`

```text
sudo thelb status                  # the summary: the cluster, and how many of each are up
sudo thelb status directors        # directors, pending directors, their certificates
sudo thelb status backends         # backends, pending backends, offers, who serves what
sudo thelb status offers           # every backend's offers, and which are unassigned
sudo thelb status vips             # VIPs and their holders, services, pools
sudo thelb status all              # everything
sudo thelb status nodes pending    # tables by name: cluster, nodes, certificates, pending, pools, offers, services
sudo thelb status --json [names]   # the same as JSON
sudo thelb status --watch          # again every 2 seconds
```

What the columns say:

- **nodes - state**: `up`; `down` (no check-in for 3 seconds - a director - or no heartbeat for 10 - a backend); `catching up` (a new director, not yet voting); `stopped` (a director stopped by plan, out of the vote until it starts again); `unknown` (this director cannot reach the leader).
- **backends - checks**: the backend's own check and the leader's: `ok`, `fail`, or `-` (no result yet). **weight**: what IPVS has now - the assigned weight, or 0. **connections, packets, bytes**: what IPVS sent the backend; there is no outbound count, because the replies never pass the director.
- **services - backends up**: how many of the assigned backends have a weight above 0. **method**: `direct` or `tunnel`.
- **offers - offer**: the name its backend gave it; **own check**: the backend's own check, as its last heartbeat said; **service**: what it is assigned to, `-` while unassigned.
- **vips - held by**: the leader, while a backend serves one of the VIP's services - `-` otherwise; **since**: when it took it.

## 14. What theLB changes on a machine

Everything is runtime state, recorded and removed - parameters restored - when theLB stops cleanly. After a crash, the next start removes what the crash left (marked by theLB's address protocol, `proto 0x74` in `ip address`, or by its interfaces' and table's names). IPVS services carry no mark: a director removes those the cluster has, and any on a pool's address - one removed while it was down among them - and leaves other programs' alone.

| Where | What | When |
|---|---|---|
| Director, the leader | the control VIP on its interface (`eth0:thelb`), and the service VIPs on their pools' interfaces, announced by gratuitous ARP or neighbour advertisement | while it leads |
| Director, the leader | IPVS services and destinations | while it leads, for each service |
| Director, the leader | permanent neighbour entries: each directly reachable backend's address to its MAC address | while it leads |
| Director, the leader | `net.ipv4.vs.sloppy_tcp`, `expire_nodest_conn`, `expire_quiescent_template` set to 1 | while it leads, with services |
| Director, the leader | IPVS's connection-synchronisation daemon as master, on the control VIP's interface (`ipvsadm -L --daemon`) | while it leads |
| Director, every member | IPVS's connection-synchronisation daemon as backup, on the control VIP's interface | while it is a member |
| Backend | `thelb0`, a dummy interface, with its services' VIPs | while it runs |
| Backend | `net.ipv4.conf.all.arp_ignore` 1, `arp_announce` 2 | while it runs, unless already right |
| Backend | the nftables table `inet thelb`: port mappings, MSS clamps | while a mapping or a tunnelled service needs it |
| Backend | `thelbt0`, an IPIP interface; `rp_filter` 0 on it, and `conf.all.rp_filter` 2 if it was 1 | while a tunnelled service is assigned |
| Every node | `/run/thelb/director/` or `/run/thelb/backend/`: a socket and a lock, root only | while it runs |
| A bootstrapping director | the two secret files, if missing | once |

**theLB keeps it so.** Every 10 seconds the leader compares the kernel with what it should hold - IPVS services, destinations and weights, the service VIPs and the control VIP on their interfaces, the pinned neighbour entries, the IPVS parameters - and every director its connection-synchronisation daemons; a backend its own: `thelb0` and its VIPs, the nftables table, `thelbt0`, the parameters. What someone removed or changed is put back, and logged as `WARN drift repaired: ...`; a destination someone added to one of theLB's IPVS services is removed. In between, the leader changes only what its plan changed - nothing at all while nothing changes. A rule edited inside the nftables table is found only if the number of rules changed.

## 15. Logs

One line per event, on standard error - in the journal, under systemd:

```text
2026-11-09T01:28:39.596Z [director] OK service added service=web vip=10.0.0.20 proto=tcp port=80 scheduler=wrr method=direct pool=main by=lb-1
2026-11-09T01:28:43.601Z [director] OK backend serving service=web node=web-1 at=10.0.0.21:80 weight=3
2026-11-09T01:28:55.655Z [director] WARN backend not serving: weight 0 service=web node=web-1 at=10.0.0.21:80 reason="the director's check fails"
```

`OK` is an event that went as it should, `WARN` one to look at, `ERROR` a failure. Modules: `director`, `backend`, `network`, `dataplane`, `kernel`, `raft`, `socket`, `watchdog`. No secret or key is ever logged.

```text
2026-11-09T05:02:10.004Z [director] WARN drift repaired: an IPVS service was removed; added again service=tcp/10.0.0.20:80 scheduler=wrr
2026-11-09T05:02:10.005Z [kernel] WARN drift repaired: the VIP 10.0.0.20 was removed from thelb0: added again
2026-11-09T05:02:12.310Z [dataplane] OK IPVS connection sync started role=master interface=eth0 sync_id=147
2026-11-09T05:03:40.120Z [watchdog] ERROR a loop has stopped: systemd's watchdog is told no more, and restarts theLB loop="data plane" late=31s
```

## 16. When things fail

| What | What happens |
|---|---|
| The leader dies | The other directors elect a leader - within 1.5 to 3 seconds in tests - which takes the control VIP and the service VIPs, announces them and programs IPVS the same way. New connections go on, and so do connections in progress: IPVS's connection synchronisation has told the new leader where each one goes. Where multicast does not pass between the directors, connections in progress are lost; new ones are not. |
| The leader is stopped (`SIGTERM`, `Ctrl+C`, or `SIGHUP` when its terminal or SSH session closes) | It hands leadership to another director first: a short, clean failover. Then the new leader takes it out of the vote, so the others keep their majority without it - two directors survive it. |
| Another director is stopped by plan | The leader takes it out of the vote: it leads on, even when it is the only director left. |
| One of two directors crashes - its theLB killed, or its machine down | After 3 to 6 seconds without a leader the other finds it gone and stands alone: it leads with the cluster's state, takes the control VIP, and removes the crashed one, which waits for approval when it comes back. A crashed theLB whose machine stays up leaves the control VIP on its interface, still answering ARP, until it starts again and removes it. |
| Two directors cannot reach each other, though both run | Neither stands alone: each finds the other answering on the network, so the cluster has no leader - no VIP held - until they reach each other again. Look for `not standing alone reason=...` in their logs. |
| The leader is cut off from the others | It gives every VIP up by itself, within about a second; the others elect a leader. |
| A director restarts | It removes what it left in the kernel, rejoins without approval, and follows. |
| Every director stops | The cluster's state is gone: on restart, a director with `--bootstrap` creates a new cluster (new CA, same secrets); **every node waits for approval again, and pools, services and assignments must be created again** - from the script `thelb generate commands` printed (section 9). The backends lose nothing: each enrols again by itself and, once approved, offers again what it offered. |
| A backend restarts | It rejoins without approval, and takes up the offers the cluster holds for it; its assignments stay. |
| A backend's application fails | Weight 0 after 3 failed checks (about 6 seconds); back after 2 good ones. It stays assigned. When no backend of a VIP's services serves, the leader gives the VIP up until one does. |
| A backend dies | Weight 0 when its 10-second heartbeat lease lapses, or sooner by the leader's check. |
| Someone removes or changes what theLB set up - `ipvsadm -C`, `ip address del` of a VIP, `nft delete table inet thelb`, a kernel parameter | Put back within 10 seconds, and logged: `WARN drift repaired: ...`. |
| theLB hangs | Under systemd, the watchdog hears no more within 30 seconds of the loop stopping, and restarts it; a leader's lease lapses and the others elect a leader. |
| theLB crashes on a machine that stays up | Its kernel state stays - a leader's VIPs, IPVS rules and sync master keep answering - until theLB starts again (`Restart=always`) and removes it. |

## 17. Troubleshooting

| The message | What it means, and what to do |
|---|---|
| `the cluster at ... did not accept the secret in ...: it is out of date, or belongs to another cluster` | The node's secret file is not the cluster's. On a director: `sudo thelb admin secret show --role director` (or `backend`), put what it prints in the file, start the node again. |
| `waiting for approval` (and nothing more) | Approve the node on a director: `sudo thelb admin approve <name>`. |
| `something that is not theLB answers on ...: the control VIP or port is taken` | Another machine or program has the control VIP or port: choose another, or remove it there. |
| `<MAC> holds <VIP>, but nothing accepts a connection on port <port>` | The VIP answers on the network, but its control port does not: most often a firewall on the leader (`ufw`, `firewalld`, nftables) dropping it - open the control port to the directors there (`sudo ufw allow from 10.0.0.0/24 to any port 7443 proto tcp`). Otherwise another machine has the address: choose another VIP. |
| `the control VIP ... is already on eth0, and theLB did not add it` | The address is configured on this machine by someone else: remove it, or choose another `--vip`. |
| `no interface has an address in the network of ...: name one with --interface` | The VIP is in no network of this node: give a VIP in the node's network, or `--interface`. |
| `--vip ... resolves to ..., and none of them is in a network of this node` / `... more than one address` | A named VIP must resolve to exactly one address in the node's networks: fix `/etc/hosts` or DNS, or give the address. |
| `the name ... is a director's: give this backend a name of its own, with --name` | On a director's machine, name the backend differently. |
| `the forward path to ...: this director has no interface in ...'s network - direct routing needs one` | That backend is in another network: use a `--method tunnel` service for it. |
| `... runs on a director, so its offer must be on the service's port` | A backend on a director's machine cannot map ports: offer the service's port. |
| `the leader's check of <offer> (...) at <address:port> fails: ...` | The leader cannot reach the application at the backend's address and port: start it, make it listen on that address, or open the firewall to the directors. An `--offer` is offered again once its check passes. |
| `<node> offers tcp/8080 already, as <name>` | One offer per protocol and port on a backend: withdraw the other first (`thelb offer remove <name>`). |
| `this backend is not a member of a cluster yet` | `thelb offer` works once the backend is approved. |
| `<node> does not offer <name>: it offers ...` | Assign one of the offers listed, by its name: `sudo thelb status offers`. |
| `backend not serving: weight 0 ... reason="..."` | The reason says which condition failed: `no heartbeat`, `its own check fails`, `it does not hold the VIP yet`, `the director's check fails`, `it is stopping`. |
| `IPVS is not available (is the ip_vs module loaded?)` | Load `ip_vs` (`sudo modprobe ip_vs`), or install the kernel's modules. |
| `IPVS connection synchronisation failed: connections in progress would not survive a failover` | The kernel refused the sync daemon: the error says why - often no IPv4 address on the control VIP's interface. theLB tries again every 10 seconds; services work meanwhile, but a failover loses connections in progress. |
| `standing alone: the other director is gone; ... node=<name> reason=...` | The other of two directors crashed: this one leads alone and removes it. Start the crashed one again, and approve it (`thelb admin approve <name>`). |
| `no leader, and the other director may be alive: not standing alone reason=...` | Two directors without a leader, and this one cannot tell that the other is gone. `a partition, not a crash`: it answers on the network but its control port does not - open the control port between them (a firewall), or reconnect them. `does not reach its network`: this director's own link or gateway is down. |
| `removed an IPVS connection sync daemon an earlier run left role=master` | A director that crashed while it led left its sync master running: removed at the next start, as it no longer leads. |
| `creating thelbt0: ... (is the ipip module available?)` | The backend of a tunnelled service needs `ipip`: `sudo modprobe ipip`. |
| `the cluster no longer accepts this backend's certificate` | The backend was removed, or the cluster was created again: it asks to join again, and waits for approval. |
| `WARN drift repaired: ...` | Something else changed theLB's kernel state - a script, a person, another network tool - and theLB put it back. Find what did it: the two will undo each other every 10 seconds. |
| `ERROR a loop has stopped ...` | A loop of theLB's stopped going round; under systemd theLB is restarted. Report it with the log around it. |
| `theLB is already running on this node` | One director and one backend may run on a machine, each once. |
| `thelb ... runs only as root` | Use `sudo`. |

`thelb status` on every node, and the logs, are the first places to look.

## 18. Command reference

Every command but `--help` and `--version` runs only as root. Exit status: `0` done, `1` failed, `2` used wrongly. `thelb <command> --help` shows a command's flags.

```text
thelb director --vip <address|name> --port <port> --secret-file <file> --backend-secret-file <file>
               [--bootstrap] [--cluster <name>] [--name <name>] [--interface <name>]
thelb backend  --join <address|name>:<port> --secret-file <file> [--offer [<name>=]<proto>/<port>[,check=tcp|none|http:<path>] ...]
               [--name <name>] [--interface <name>]
thelb offer add <name> <proto>/<port>[,check=tcp|none|http:<path>]      (on a backend)
thelb offer remove <name>                                               (on a backend)
thelb status   [--json] [--watch] [--role director|backend] [directors|backends|vips|all|<table> ...]
thelb generate secret
thelb generate commands
thelb admin approve <name> ... | --match '<regex>' [--yes]
thelb admin refuse <name>
thelb admin remove <name>
thelb admin pool add <name> --vip <first/prefix> --vippool-count <n> [--interface <name>] [--default]
thelb admin pool remove <name>
thelb admin service add <name> --proto tcp|udp|sctp --port <port> [--pool <name>] [--vip <address>]
               [--scheduler wrr|rr|wlc|lc|sh|mh] [--method direct|tunnel]
thelb admin service remove <name>
thelb admin assign <service> <node>:<offer> ... [--weight <node>=<n>,...]     (<offer>: its name, or <proto>/<port>)
thelb admin unassign <service> <node>
thelb admin secret show --role director|backend
thelb --version
```

- `--cluster` names the cluster when this director creates it (default `thelb`). `--name` defaults to the hostname. `--interface` is needed only when the control VIP's interface is not the one in its network.
- `thelb admin` runs on a director; on a director that is not the leader, the change goes to the leader, recorded under that director's name.

## 19. Trying it on one machine

`make lab`, on a Linux machine with root, builds a whole setup in network namespaces on `198.18.0.0/24` - nothing reaches the machine's own network: three directors, three backends, a client, and behind a router a second network with a fourth backend; and three services - `web` on `198.18.0.20:80` (direct routing), `mapped` on `.21:80` (each backend on a port of its own) and `far` on `.22:80` (IP-in-IP, to the backend behind the router). Then:

```text
sudo test/lab.sh curl 12                     # the client fetches web 12 times: who answered
sudo test/lab.sh curl 8 198.18.0.22          # far, by IP-in-IP
sudo test/lab.sh run d1 thelb status         # any command, on any node: d1-d3, b1-b4, c (the client)
sudo test/lab.sh run d1 ipvsadm -Ln          # what theLB programmed, on the leader
sudo test/lab.sh app b2 down                 # b2's application fails; app b2 up brings it back
sudo test/lab.sh kill d1                     # the leader crashes; start d1 brings it back
sudo test/lab.sh serve d2                    # a backend beside d2's director: d2-web
sudo test/lab.sh logs d1                     # follow a node's log
make lab-down                                # take it all away
```

## 20. Not in this build yet

`thelb doctor`; `thelb generate` for systemd units, sysctl and module files, and Kubernetes; the Kubernetes provider; secret rotation (`admin secret rotate|retire`); the return-path check before an assignment; tunnelled IPv6 services, and GUE; SCTP health checks; Prometheus metrics; detecting a VIP claimed by another machine; preflight checks of capabilities and modules before starting; limiting which networks backends may enrol from. See the [build state](build-state.md) for what comes next.
