---
title: Directors and failover
lead: Add directors so that the VIPs survive a failed machine. Three is the right number for most setups.
description: How theLB directors form a cluster, how the leader holds the VIPs with a kernel lease, and what happens when a director stops, crashes or is cut off.
section: Guides
order: 1
nav: Directors and failover
---

## Add a director

Copy **both** secret files - `director.secret` and `backend.secret` - to the new machine, readable by root only. Then start it with the same command as the first one (with or without `--bootstrap`):

```bash
sudo thelb director --vip 10.0.0.10 --port 7443 \
  --secret-file /etc/thelb/director.secret --backend-secret-file /etc/thelb/backend.secret
```

It finds the cluster on the control VIP and waits for approval. Approve it on any director:

```bash
sudo thelb admin approve lb-2
```

It then copies the cluster's state from the leader. Once it has caught up, it becomes a voting member, and can take over if the leader fails.

:::screen director-join
A second director's log: it finds the cluster, waits, is approved, catches up and is ready.
:::

`thelb status directors` shows every director, its state, and the cluster's leader:

:::screen status-directors
The cluster, its directors, and their certificates.
:::

## How many directors?

| Directors | What it survives |
|---|---|
| 1 | nothing: if it stops, the VIPs stop. Fine for trying theLB. |
| 2 | one planned stop, or one crash - with a few rules (below) |
| **3** | **any one failure, with no guessing. Recommended.** |
| 5 | any two failures |

The cluster uses **Raft**: a change counts once a majority of the voting directors have it. Three directors keep their majority when any one fails.

## The leader and the VIP lease

One director, the **leader**, holds the control VIP and every service VIP, and programs the kernel's load balancer (IPVS). The others follow, and are ready to take over.

The leader adds each VIP with a **lifetime** - a lease. By default the lease is 6 seconds, and the leader renews it every 2 seconds. You can see it with `ip address`: `valid_lft 6sec`.

:::screen lease
The leader's `eth0`: its own address, the control VIP (`eth0:thelb`) and the service VIPs, each with `valid_lft` - its lease.
:::

Why a lease? If theLB is killed, or its machine hangs, it stops renewing. Its own kernel then removes the VIPs when the lease runs out. So a dead theLB never leaves a VIP answering on the network.

**No director ever takes a VIP that another machine still answers for.** Before a new leader takes a VIP, it looks twice, one second apart. It takes the VIP only when nothing answers for it (no ARP or neighbour reply, and for the control VIP, nothing accepting connections on it).

You can change the lease:

```bash
sudo thelb admin lease 10     # seconds, from 3 to 3600
```

The lease is stored in the cluster's state, so every director knows how long a stopped leader's VIP can still answer. A longer lease means a slower takeover after a crash. A shorter one means more renewals.

## When a director stops or fails

### A planned stop

Stop a director with `SIGTERM`, `Ctrl+C` or `systemctl stop`. If it was leading, it first hands leadership to another director - a short, clean failover. Then it leaves the vote, so the others keep their majority without it.

It stays approved: when it starts again, it rejoins without approval. `thelb status directors` shows it as `stopped`.

### A crash, with three or more directors

The others still have a majority. They elect a new leader within a few seconds. The new leader takes each VIP once its lease has run out on the dead machine.

After 10 seconds without hearing from the dead director, the leader checks it. If it is really gone - its control port refuses connections, or its machine does not answer at all - the leader takes it **out of the vote**. That way, a second crash still leaves a working majority. `thelb status` shows it as `down`:

:::screen failover-status
After the leader was killed: the new leader, and the old one `down`, out of the vote.
:::

When the crashed director starts again, it cleans up what it left in its kernel and rejoins - without approval:

:::screen rejoin
The crashed director, started again: it removes what the crash left, rejoins and follows.
:::

### Connections in progress

Connections keep going through a failover. The leader sends its connection table to the other directors all the time (IPVS *connection sync*, by multicast). So the new leader already knows which backend each open connection belongs to. This needs multicast between the directors - see the firewall table in [Install](install.md#open-these-in-your-firewalls).

## Two directors

Two directors work, but a crash is harder to judge: the one left alone has no majority, so it cannot simply win an election. theLB handles it like this:

1. The survivor waits until the lease has surely run out - the lease plus 2 seconds (8 seconds by default). The director whose name sorts later waits 6 seconds more, so the two never act at the same moment.
2. Then it checks that **the other is gone** (its control port refuses connections, or its machine is silent), that **its own gateway answers** (so it is not the one cut off), and that **the control VIP is free**.
3. If all three hold, it **stands alone**: it takes the VIP and goes on as the only voter, with every pool, service and assignment. The crashed director must be **approved again** when it comes back - it might have crashed for a reason you want to look at first.

If the other's machine still holds the VIP after the lease, the survivor does not take it: something is still alive there.

:::warning Give two directors a gateway
The gateway check is what tells a director that it is the one cut off. Make sure the control VIP's network has a gateway that answers.
:::

When both directors are alive but cannot talk to each other:

| What is between them | What happens |
|---|---|
| A firewall that **drops** the control port | Neither stands alone: each can see the other on the network. No leader until they can talk again. |
| A firewall that **rejects** the control port | One stands alone and takes the VIP. The other finds the VIP in use and stays out. One leader. |
| A cut neither can see | Both stand alone, each on its side. When they see each other again, the one whose name sorts later stands down and waits for approval. **Changes made on its side meanwhile are lost.** |

Under systemd with two directors, use `RestartSec=30` (`thelb generate systemd` does this). A crashed director that restarts sooner could create a cluster of its own before the other has stood alone. With a lease longer than 6 seconds, use the lease plus 25 seconds.

## `--bootstrap`

`--bootstrap` lets a director create the cluster when it finds none, after looking for 5 seconds. Put it on the first director. You may put it on others too, so that the cluster does not depend on one machine coming back after every director has stopped.

:::warning When every director stops
The cluster's state lives in the directors' memory. If **every** director stops, it is gone: the next director with `--bootstrap` creates a new cluster, every node must be approved again, and pools, services and assignments must be created again. Keep a copy with [`thelb generate commands`](services.md#keeping-them-thelb-generate-commands).
:::
