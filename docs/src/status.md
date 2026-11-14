---
title: Reading thelb status
lead: What each table and column of thelb status means.
description: The tables and columns of thelb status - nodes, backends, services, offers, VIPs and the cluster - and what each state means.
section: Reference
order: 3
nav: Reading thelb status
---

## The views

| Command | Shows |
|---|---|
| `thelb status` | the summary: the cluster, and how many of each thing are up |
| `thelb status directors` | the cluster, directors, pending directors, certificates |
| `thelb status backends` | backends, pending backends, offers, and each service's backends |
| `thelb status offers` | every backend's offers |
| `thelb status vips` | VIPs and who holds them, services, pools |
| `thelb status all` | everything |

`thelb status`, `status directors` and `status vips` are quick at any size. The views with the backends table also read IPVS's traffic counters on the leader, service by service: about a second for a thousand services.

## The summary

:::screen status
The summary: each kind of thing, how many there are, and how many are up, down or waiting.
:::

## Nodes

:::screen status-directors
The cluster table, then the nodes and their certificates.
:::

**state** is one of:

| State | Means |
|---|---|
| `up` | working |
| `down` | a director that has not checked in for 3 seconds, or a backend with no heartbeat for 10; also a director the leader found gone and took out of the vote |
| `catching up` | a new director, copying the cluster's state; it cannot vote yet |
| `stopped` | a director stopped on purpose; out of the vote until it starts again |
| `unknown` | this director cannot reach the leader, so it cannot tell |

The **node fingerprint** identifies the machine. Compare it before you approve a node.

## The cluster

- **leader**, **term**, **leader since**: who leads, the Raft term, and since when.
- **vip lease**: how long the kernel keeps a VIP after the leader's last renewal - `thelb admin lease`.

## Backends

:::screen status-backends
Each backend of each service.
:::

| Column | Means |
|---|---|
| **checks** | the backend's own check, then the directors' check: `ok`, `fail`, or `-` (no result yet) |
| **weight** | what IPVS has now: the assigned weight, or 0 while the backend does not serve |
| **connections**, **packets**, **bytes** | what IPVS sent the backend. There is no count of replies: they never pass the director |
| **last check-in** | how long ago the backend's last heartbeat arrived |

## Services

- **backends up**: how many of the assigned backends have a weight above 0.
- **method**: `direct` or `tunnel`.
- **scheduler**: how connections are spread - `wrr` by default.

## Offers

:::screen status-offers
Every offer.
:::

- **offer**: the name the backend gave it.
- **own check**: the backend's own check, as its last heartbeat said.
- **service**: what it is assigned to; `-` while not assigned.

## VIPs

:::screen status-vips
The VIPs: the control VIP and each service's, with their holder.
:::

- **purpose**: `control` (the cluster's own address) or `service`.
- **held by**: the leader, while a backend serves one of the VIP's services; `-` otherwise.
- **since**: when the holder took it.

## JSON

`thelb status --json` gives everything as JSON, for scripts. It has a few fields the tables do not show - for example, who makes each backend's directors' check (`checked_by`), and each backend's active and inactive connections.
