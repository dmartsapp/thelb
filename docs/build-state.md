# Build state

Where the code is, what is broken, and what comes next - kept up to date at every pause, so work can resume from here. Last updated: 2026-11-09, after offers by name were merged.

## Branches

| Branch | Holds | State |
|---|---|---|
| `main` | directors, Raft, the control VIP (by address or name), backends, a director's host as a backend, pools, services, IPVS direct routing and IP-in-IP tunnelling, port mapping on the backend, `status` views, `generate commands`, offers by name made while a backend runs, a service's VIP held once a backend serves it, the lab, the user manual | `make check` green here and on JH; `make e2e` green on JH (170 checks) |

`wip/offers` was merged into `main` by fast-forward on 2026-11-09, after `wip/gencmds`, `wip/ipip` and `wip/portmap`; nothing is in progress on another branch.

**The user's flow, as built (merged from `wip/offers`)** (2026-11-09: "backends should be able to register their services on their own after it has registered and is in active communication with the director... director tcp checks if service is actually running... director will unassign that offering from the service... director ensures the backend service offer is still active, then runs the ipvs then brings up the vip"; and "if director dies, all offers from backend are removed too... backends dont need to suffer for director's outage... this is for single node director"):
1. a backend joins - with or without `--offer` - and is approved;
2. it offers an application, named as it likes: `thelb offer add <name> <proto>/<port>[,check=...]`, or an `--offer [name=]...` flag - offered once its own check passes;
3. the leader runs the offer's check (TCP connect by default) at the backend's address before recording it; refused with the reason otherwise;
4. the backend checks it every 2 s and reports with each heartbeat - the watchdog; the leader checks assigned ones too. **Interpreted, and told the user**: a failing check takes the offer out of rotation (weight 0) without unassigning it; it comes back by itself. Literal unassigning is a small change if the user wants it;
5. a director creates the pool and the service - no VIP on the network yet;
6. `thelb admin assign <service> <node>:<name>` (or `<node>:<proto>/<port>`); `thelb status offers` lists the unassigned;
7. the leader programs IPVS, then brings the VIP up - only while a backend serves; the VIP goes first when none does.
The cluster keeps the offers through a backend's restart; the backend keeps them through the directors' outage and offers them again, by itself, to the cluster created again once approved.

**The user asked, about Kubernetes** (answered, not built; phase 3): which nodes get a `LoadBalancer` Service's traffic - the approved backends matched to Nodes by address, `Ready`, without `node.kubernetes.io/exclude-from-external-load-balancers`; `externalTrafficPolicy: Cluster` - all of them, checked at kube-proxy's `:10256/livez`; `Local` - those with ready endpoints in the Service's EndpointSlices, checked at `healthCheckNodePort`; at the Service's port (kube-proxy matches the VIP from `status.loadBalancer.ingress`). Proposed: weight = ready local endpoints, for `Local`. To verify in kind: Cilium's kube-proxy replacement health endpoint; tunnelled services into kube-proxy's rules.

## What came in last (from `wip/gencmds`)

- **`thelb generate commands`**, on a director: a script of the `thelb admin` commands that create the pools, services and assignments again - each service on the VIP it has, weights other than 1 kept - with the members' `approve --match` line as a comment, never a command. theLB prints it and never reads it. A backend refuses: it runs on a director. `localsock` op `generate`.
- **The lab's round trip** (end to end): every director stopped; d1 bootstraps the cluster again, empty; the directors and then the backends are approved with the script's own line (the backends enrol again by themselves when another cluster's CA answers); the script run; the client served 4/4/4 on the same VIP.

## Came in before (from `wip/portmap`)

- **Port mapping on the backend**: `internal/nft`, theLB's own nftables client over netlink - table `inet thelb`, chain `portmap` (nat, prerouting, priority dstnat), one rule per VIP, protocol and port (`ip daddr V tcp dport 80 dnat ip to :8080`), replaced whole in one transaction and removed when no mapping is left. The IPVS destination keeps the service's port; the director's check goes to the application's port.
- **A director's host as a backend**: two daemons - `thelb backend` started after the director, under its own `--name`; one socket and lock per role (`/run/thelb/director/`, `/run/thelb/backend/`); `status --role`; the clone check per role; the leader refuses a backend with a director's name, does not pin its own address, and assigns an offer from a director's address only on the service's port. IPVS lists the host as an ordinary direct-routing destination (`Route`), and its share arrives through the local stack. `test/lab.sh serve dN`.
- **The control VIP by name**: `--vip thelb-vip`, `--join thelb-vip:7443`, resolved at start to the one address it has in the node's networks.
- **Fixed**: the planner's pointers into a growing slice (two services: the first lost its destinations); an API stream's first deadline covering writes for its whole life (backend heartbeats and director check-ins cut 10 s in, `tls: bad record MAC`); a refused backend retrying in silence; the end-to-end cleanup waiting for ever on web servers it never stopped.
- **Decided**: nftables for port mapping, not eBPF; no configuration file; systemd for backends on Kubernetes nodes.

## Next - the agreed order

1. ~~Finish port mapping~~ - done, merged.
1b. ~~A director's host as a backend too~~ - done, merged: (the user: start the director first, register the backend later, keep systemd for backends): two daemons, `thelb backend` started after the director under its own `--name`; one socket and lock per role (`/run/thelb/director/`, `/run/thelb/backend/`); the clone check is per role; the leader refuses a backend with a director's name, does not pin its own address, and assigns an offer from a director's address only on the service's port. IPVS delivers the host's share locally while it leads. `test/lab.sh serve dN`, and an end-to-end scenario. **Also built: the control VIP by name** (`--vip thelb-vip`, `--join thelb-vip:7443`), resolved at start.
2. ~~IP-in-IP tunnelling~~ - done, merged: `--method tunnel`, IPv4; the director needs only a route to the backend; `thelbt0` (IPIP, collect-metadata mode, from any director) unwraps; `rp_filter` 0 on `thelbt0` and 2 for all if it was 1 (restored); an nftables MSS clamp (MTU - 60) for each tunnelled VIP; a backend may join a control VIP behind a router, and then serves tunnelled services only. **Left for later**: IPv6 (ip6ip6), GUE, the return-path check.
2b. **The user manual** - written: `docs/manual.md`, for operators; keep it in step with every change.
3. **Apply what changed, and repair drift**: the leader applies only the difference from what it last applied, and compares the whole kernel every 10 s - putting back what someone removed (the watchdog loop); systemd's watchdog is told only while that loop runs.
4. **IPVS connection synchronisation**: the sync daemon as master on the VIP holder, backup on the other directors, following leadership - so connections in progress survive a failover.
5. **A scale test in the lab**: a few thousand services; time an apply, count netlink calls, watch memory.
6. **Health checks spread across directors**, not all on the leader.

Then decisions for the user: **NAT** (who routes the backends' replies back: their network, theLB's policy routes, or full NAT); **BGP with ECMP**, beside or instead of ARP.

**Built after IP-in-IP: `thelb generate commands`** (the user: "proceed with the generate commands"): on a director, a script of the `admin` commands that create the pools, services and assignments again, each service on its VIP, with the members' `approve --match` as a comment; the lab's round trip - every director stopped, the cluster created again, the nodes approved with the script's own line, the script run - serves the same backends on the same VIP.

## Open with the user

- **Kubernetes nodes as backends: decided - systemd, no DaemonSet** (confirmed 2026-11-09), and written into the design's [Kubernetes](design.md#where-thelb-runs) section with its reasoning: the leader is the one writer of Services' status; nodes need no API access; nothing of kube-proxy's or Cilium's is patched.
- **NAT** and **BGP**: see above.

## Still to build (from the design)

Preflight (full authority or refuse to start, loading modules); kernel feature probing, tested on RHEL 9's 5.14; VIP conflict detection, and a cluster standing down when another claims its control VIP; `--peers`; secret rotation (`admin secret rotate|retire`); the backend networks a director allows to enrol, as an `admin` setting; a removed node kept out until allowed; term fencing on backends; an SCTP health check; Prometheus metrics; IPv6 tested end to end (JH boots with IPv6 disabled); `doctor`; `generate`; then the Kubernetes provider (phase 3), tunnelling's GUE, active-active VIPs (phase 5), the proxy (phase 6); CI, releases, the docs site.

## Decided on 2026-11-08

- Port mapping on the backend is nftables destination NAT, over netlink - not eBPF.
- **No configuration file**: theLB reads the two secret files and nothing else.
- `thelb status` is a summary by default, with views: `directors`, `backends`, `vips`, `all`.
- theLB's own CA stays its own; `simplepki` would need fixes first (a non-RSA CSR panics the signer; CSR signatures are not checked; the requester chooses its identity).
- The order above: IP-in-IP before NAT; NAT waits for connection synchronisation.

## How to resume

- The code: this repository, `github.com/dmartsapp/thelb`. Development and tests run on JH (`ssh JH`, Debian 13, kernel 6.12): the tree is copied to `~/thelb` by `make remote-sync REMOTE=JH`, which every `remote-*` target runs first.
- JH has Go 1.27.3 in `~/.local/go`, golangci-lint 2.15.0 and govulncheck in `~/go/bin`, and `nft`, `ipvsadm`, `curl` and `python3`; it boots with `ipv6.disable=1`.
- `make check` here and `make remote-check REMOTE=JH` there: format, vet, race tests, lint, govulncheck.
- `make remote-e2e REMOTE=JH`: the root kernel tests and the lab end to end - longer than 10 minutes now, so run it in the background.
- `make lab` on JH, then `sudo test/lab.sh ...` (see the README): the lab by hand; `make lab-down` takes it away.
- No AI attribution in commits.
