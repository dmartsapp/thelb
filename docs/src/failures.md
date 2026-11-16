---
title: When things fail
lead: What theLB does when a director, a backend, an application or the network fails - and what you need to do, if anything.
description: theLB's behaviour in every failure - leader crash, planned stop, partitions, backend and application failures, drift, hangs - and how long each takes.
section: Reference
order: 4
nav: When things fail
---

## Directors

| What happens | What theLB does |
|---|---|
| **The leader crashes** (three or more directors) | The others elect a new leader within a few seconds. It takes each VIP once its lease runs out on the crashed machine (6 seconds by default) and nothing answers for it. New connections go on; open connections go on too, because connection sync told the new leader where each one goes. After 10 seconds, the crashed director is taken out of the vote. |
| **The leader is stopped** (`SIGTERM`, `Ctrl+C`, closing its terminal) | It hands leadership to another director first - a short, clean failover - and leaves the vote. |
| **Another director is stopped** | It leaves the vote. The leader carries on, even when it is the only director left. |
| **One of two directors crashes** | The other waits out the lease (8 to 14 seconds without a leader, by default), checks that the crashed one is gone and the VIP is free, then stands alone with the cluster's full state. The crashed one must be approved again. The VIP is never on both machines. |
| **Two directors cannot reach each other** | Depends on what is between them - see [Two directors](directors.md#two-directors). |
| **The leader is cut off from the others** | It gives every VIP up by itself, within about a second. The others elect a leader. |
| **A director restarts** | It removes what it left in the kernel, rejoins without approval, and follows. |
| **Every director stops** | The cluster's state is gone. The next director with `--bootstrap` creates a new cluster (a new certificate authority, the same secrets). Every node must be approved again, and pools, services and assignments must be created again - from your [saved commands](services.md#keeping-them-thelb-generate-commands). The backends lose nothing: each one joins again by itself and offers what it offered before. Kubernetes Services keep their VIPs: once the pools are back, each takes again the address its status names. |
| **theLB crashes, but the machine stays up** | Its VIPs keep answering, and its kernel keeps forwarding, until the lease runs out (6 seconds). The new leader takes each VIP only then. The old IPVS rules stay until theLB starts again and removes them - harmless, because no VIP brings traffic to them. |
| **theLB hangs** | Under systemd, the watchdog restarts it within 30 seconds. A hung leader stops renewing its lease, so the VIPs move. |

:::screen failover
The new leader's log after the old leader was killed in the lab.
:::

## Backends and applications

| What happens | What theLB does |
|---|---|
| **An application fails** | Weight 0 after 3 failed checks (about 6 seconds); back after 2 good ones. It stays assigned. If no backend of a VIP serves, the leader gives the VIP up until one does. |
| **A backend dies** | Weight 0 when its 10-second heartbeat lease runs out, or sooner if the directors' check fails first. |
| **A backend restarts** | It rejoins without approval and takes back its offers. Its assignments stay. |
| **A backend is stopped** | It asks to be drained, keeps its VIPs a moment so that connections on their way are not lost, then removes everything. |

:::screen app-down
An application stops and starts again: weight 0, then serving again.
:::

## Someone changes theLB's setup

| What happens | What theLB does |
|---|---|
| Someone runs `ipvsadm -C`, deletes a VIP with `ip address del`, deletes the nftables table, or changes a kernel setting theLB set | Puts it back within 10 seconds, and logs `WARN drift repaired: ...`. |
| Someone adds a backend to one of theLB's IPVS services by hand | Removes it. |

:::screen drift
IPVS cleared by hand on the leader, and put back.
:::
