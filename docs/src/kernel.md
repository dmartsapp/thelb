---
title: What theLB changes
lead: Everything theLB changes on a machine is temporary. It is recorded, kept as it should be, and removed when theLB stops.
description: The kernel state theLB adds on directors and backends - VIPs, IPVS, neighbour entries, kernel parameters, interfaces, nftables - and how it repairs and cleans it.
section: Reference
order: 5
nav: What theLB changes
---

## The rule

theLB changes only the running system - never a configuration file. When it stops cleanly, it removes what it added and puts back the settings it changed. After a crash, the next start removes what the crash left. theLB marks what it owns: its addresses carry its own protocol number (`proto 0x74` in `ip address`), and its interfaces and nftables table have its own names.

## The full list

| Where | What | When |
|---|---|---|
| Director, the leader | the control VIP on its interface (`eth0:thelb`) and the service VIPs on their pools' interfaces - **leased**, renewed every third of the lease, and announced with gratuitous ARP or neighbour advertisements | while it leads |
| Director, the leader | IPVS services and their backends | while it leads, for each service |
| Director, the leader | permanent neighbour entries: each directly reachable backend's address, pinned to its MAC address | while it leads |
| Director, the leader | `net.ipv4.vs.sloppy_tcp`, `expire_nodest_conn` and `expire_quiescent_template` set to 1 | while it leads, with services |
| Director, the leader | IPVS's connection-sync daemon as master, on the control VIP's interface | while it leads |
| Director, every member | IPVS's connection-sync daemon as backup | while it is a member |
| Backend | `thelb0`, a dummy interface, with its services' VIPs | while it runs |
| Backend | `net.ipv4.conf.all.arp_ignore` 1 and `arp_announce` 2 | while it runs, unless already stricter |
| Backend | the nftables table `inet thelb`: port mappings and MSS clamps | while a mapping or a tunnelled service needs it |
| Backend | `thelbt0`, an IP-in-IP interface with the address `192.0.0.8`; `rp_filter` 0 on it, and `conf.all.rp_filter` 2 if it was 1 | while a tunnelled service is assigned |
| Every node | `/run/thelb/director/` or `/run/thelb/backend/`: a socket and a lock, root only | while it runs |
| A bootstrapping director | the two secret files, if missing | once |

IPVS services carry no mark. A director removes the ones the cluster has, and any on a pool's address, and leaves other programs' IPVS services alone.

:::screen ipvsadm
IPVS on the leader, as theLB programmed it.
:::

## theLB keeps it so

Every 10 seconds the leader compares the kernel with what it should be: IPVS services, backends and weights, the VIPs, the pinned neighbour entries and the IPVS settings. Every director checks its connection-sync daemons, and every backend checks its own setup: `thelb0` and its VIPs, the nftables table, `thelbt0`, the settings.

Anything removed or changed is put back, and logged as `WARN drift repaired: ...`. A backend someone added by hand to one of theLB's IPVS services is removed. Between checks, the leader changes only what its plan changed - and nothing at all while nothing changes.

A rule edited inside the nftables table is noticed only if the number of rules changed.

## See for yourself

On the leader:

```bash
ip address show                  # the VIPs, with valid_lft: their lease
ipvsadm -Ln                      # the IPVS services and backends
ipvsadm -L --daemon              # the connection-sync daemons
ip neighbour show nud permanent  # the pinned backends
```

On a backend:

```bash
ip address show thelb0
ip address show thelbt0          # with tunnelled services
nft list table inet thelb        # with port mapping or tunnelled services
```

`thelb doctor` shows all of this as a plan, before theLB runs.
