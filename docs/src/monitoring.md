---
title: Status, logs and metrics
lead: Three ways to see what theLB is doing - thelb status for now, the log for what happened, and Prometheus metrics for your dashboards.
description: Watch theLB - thelb status and its views, the log format, --debug connection logs, and the Prometheus metrics endpoint.
section: Guides
order: 8
nav: Status, logs, metrics
---

## `thelb status`

```bash
sudo thelb status                  # the summary
sudo thelb status directors        # directors, pending directors, certificates
sudo thelb status backends         # backends, offers, and who serves what
sudo thelb status offers           # every offer, and which are not assigned
sudo thelb status vips             # VIPs and who holds them, services, pools
sudo thelb status all              # everything
sudo thelb status --json           # the same, as JSON
sudo thelb status --watch          # again every 2 seconds
```

:::screen status
The summary.
:::

`status backends` shows each backend of each service: its checks, its weight in IPVS, and the traffic IPVS sent it:

:::screen status-backends
Every backend of every service, its checks (its own / the directors'), weight and traffic.
:::

[Reading thelb status](status.md) explains every column.

## Logs

theLB writes one line per event to standard error - so under systemd, to the journal (`journalctl -u thelb-director`).

```text
2026-11-09T01:28:39.596Z [director] OK service added service=web vip=10.0.0.20 proto=tcp port=80 scheduler=wrr method=direct pool=main by=lb-1
2026-11-09T01:28:55.655Z [director] WARN backend not serving: weight 0 service=web node=web-1 at=10.0.0.21:80 reason="the director's check fails"
```

- `OK` is something that went as it should; `WARN` is something to look at; `ERROR` is a failure.
- The word in brackets is the part of theLB that wrote it: `director`, `backend`, `network`, `dataplane`, `kernel`, `raft`, `socket` or `watchdog`.
- No secret or key is ever logged.

When a backend's application stops, the leader's log says so - and says so again when it is back:

:::screen app-down
`b2`'s web servers stopped and started again: weight 0, then serving again.
:::

When something changes theLB's setup behind its back, theLB puts it back within 10 seconds and logs it. Here someone cleared IPVS on the leader (`ipvsadm -C`):

:::screen drift
theLB finds its IPVS services gone, and adds them again.
:::

[Log messages](messages.md) lists the messages that need you to do something.

### Each connection, with `--debug`

Start a director with `--debug` to log every new connection. While it leads, it reads the kernel's connection table every second and logs each new connection once: the client's address, the service and VIP, and the backend it went to.

:::screen debug
Each new connection, once: client, service, VIP, backend, protocol, state.
:::

A busy service writes many lines, so turn `--debug` on to look, not for good. Without it there are no DEBUG lines at all.

## Metrics

`--metrics <address:port>` on a director or a backend serves Prometheus metrics at `http://<address:port>/metrics`.

```bash
sudo thelb director ... --metrics 10.0.0.11:9100
```

:::warning Choose the address carefully
The metrics are plain HTTP. Use an address that only your monitoring can reach - `127.0.0.1`, or the machine's management address. theLB refuses `0.0.0.0` and `::`, because they would include the VIPs.
:::

Every director reports its own state, and **only the leader** reports what the cluster serves. So scrape every director: nothing is counted twice.

:::screen metrics
Some of the leader's metrics: leadership, VIPs held, and the traffic through each service.
:::

| Metric | What it is |
|---|---|
| `thelb_build_info{role,version,commit}` | always 1; the build is in the labels |
| `thelb_member`, `thelb_leader` | 1 or 0: in a cluster; the leader |
| `thelb_raft_term`, `thelb_vip_lease_seconds` | the Raft term this director knows; the VIP lease |
| `thelb_nodes{role,state}` | the cluster's nodes, by role and state |
| `thelb_takeovers_total` | how many times this director stood alone, the other of two gone |
| `thelb_vip_held{vip,purpose,interface}` | leader: 1 for each VIP it holds |
| `thelb_service_backends{service,vip}`, `thelb_service_backends_up{service,vip}` | leader: backends assigned, and serving |
| `thelb_backend_weight{service,backend}` | leader: each backend's weight in IPVS (0 while it does not serve) |
| `thelb_backend_check_up{service,backend,check}` | leader: 1 while a check passes; `check` is `own` or `director` |
| `thelb_backend_connections_total`, `_packets_total`, `_bytes_total` | leader: what IPVS sent each backend |
| `thelb_backend_connections_active`, `_inactive` | leader: each backend's connections now |
| `thelb_service_connections_total`, `_packets_total`, `_bytes_total`, `thelb_service_connections_active` | leader: **the traffic through it**, by service and VIP |
| `thelb_offer_check_up{offer,port}`, `thelb_offer_assigned{offer,service}` | backend: its own checks, and its offers' services |

The traffic counts are what came **in**. Replies go from the backends straight to the clients, so they never pass a director, and there is no count of them.
