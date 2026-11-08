# Build state

Where the code is, what is broken, and what comes next - kept up to date at every pause, so work can resume from here. Last updated: 2026-11-08.

## Branches

| Branch | Holds | State |
|---|---|---|
| `main` | up to `54c83a3`: directors, Raft, the control VIP, backends, pools, services, IPVS direct routing, `status` views, the lab | `make check` green; `make e2e` green (110 checks) |
| `wip/portmap` | port mapping on the backend (nftables), a planner fix, the no-configuration-file decision, this file | **one test fails** - see below; merge into `main` once it and `make e2e` pass |

## What `wip/portmap` adds

- `internal/nft`: theLB's own nftables client over netlink - table `inet thelb`, chain `portmap` (nat, prerouting, priority dstnat), one rule per VIP, protocol and port (`ip daddr V tcp dport 80 dnat ip to :8080`). The table is replaced whole in one transaction, and removed when no mapping is left. Its root test passes on JH (`TestRootTheTableIsReplacedWhole`).
- Backends apply the mappings their orders need (`backend.Kernel.Map`); `backnet` removes a table a crash left; `api.Assignment` carries `AppPort`.
- Directors accept an offer whose port differs from the service's; the IPVS destination keeps the service's port (direct routing never changes it); the director's health check goes to the application's port.
- **Fixed**: the leader's planner kept pointers into a slice it was still growing, so with two or more services the first by name lost its destinations in IPVS (found by the new lab scenario). It keeps indices now; `TestABackendServesAService` adds a second service to catch it.
- The lab (`test/lab.sh`) has a second service, `mapped`, on `198.18.0.21:80`, from 8080 on b1, 9090 on b2 and 7070 on b3. The end-to-end test has the same scenario, and its `within` waits by a deadline, not a count of tries.
- Docs: port mapping is nftables, not eBPF, and why; **no configuration file** (decided); `generate commands` proposed; the changelog, README and help updated.

## Broken - fix first

**`TestABackendServesAService`** (`internal/director/backend_linux_test.go`) fails on JH:

1. Just after the test's second service (`api`) is removed, the backend's heartbeat connection fails with `local error: tls: bad record MAC` - a TLS record that did not authenticate - and the backend reconnects.
2. Later the leader removes the backend (`OK node removed node=web-1` in its log), but the backend never stops: it never gets the `Removed` order.

What to look at:

- **Why the record is corrupt.** Something must write to one end of that TLS connection from two places at once, or reuse a buffer - the netlink reader had the same kind of bug. Run the test with `-race` first. Suspects: the leader's `serveBackend` and anything else holding the same `*tls.Conn` or `wire.Conn`; the backend's `session`.
- **Why the removal never arrives.** After the reconnect, either the new connection is not served, or the backend retries silently: once its certificate is revoked the handshake is refused, `serve` logs only the first loss (`lost` stays set), and `anotherCluster` sees the same CA, so it loops for ever. Whatever the cause, a backend whose handshakes keep being refused should ask - through the join protocol, with its secret - whether it is still a member, and stop if it was removed; and every distinct error should be logged, at most once a minute.

To run it alone, on JH: `cd ~/thelb && go test -count=1 -race -run TestABackendServesAService -v ./internal/director/`.

Then: `make remote-check REMOTE=JH`, and `make remote-e2e REMOTE=JH` - about 12 minutes, so in the background - then merge `wip/portmap` into `main` and push.

## Next - the agreed order

1. **Finish port mapping**: the two bugs above, the lab, the merge.
2. **IP-in-IP tunnelling**, a per-service method (`--method tunnel`); direct routing stays the default for backends on the director's network. Director: IPVS's tunnel forwarding (tunnel type IPIP). Backend: an `ipip` tunnel interface holding the VIP, `rp_filter` relaxed on it, MSS clamping or MTU guidance. The return-path check matters more here: networks drop replies from the VIP's address outside its subnet. GUE later, as an option.
3. **Apply what changed, and repair drift**: the leader applies only the difference from what it last applied, and compares the whole kernel every 10 s - putting back what someone removed (the watchdog loop); systemd's watchdog is told only while that loop runs.
4. **IPVS connection synchronisation**: the sync daemon as master on the VIP holder, backup on the other directors, following leadership - so connections in progress survive a failover.
5. **A scale test in the lab**: a few thousand services; time an apply, count netlink calls, watch memory.
6. **Health checks spread across directors**, not all on the leader.

Then decisions for the user: **NAT** (who routes the backends' replies back: their network, theLB's policy routes, or full NAT); **BGP with ECMP**, beside or instead of ARP; **`generate commands`**.

## Still to build (from the design)

Preflight (full authority or refuse to start, loading modules); kernel feature probing, tested on RHEL 9's 5.14; VIP conflict detection, and a cluster standing down when another claims its control VIP; `--peers`; secret rotation (`admin secret rotate|retire`); the backend networks a director allows to enrol, as an `admin` setting; a removed node kept out until allowed; term fencing on backends; an SCTP health check; Prometheus metrics; IPv6 tested end to end (JH boots with IPv6 disabled); a director and a backend on one node (open question 0); `doctor`; `generate`; then the Kubernetes provider (phase 3), tunnelling's GUE, active-active VIPs (phase 5), the proxy (phase 6); CI, releases, the docs site.

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
