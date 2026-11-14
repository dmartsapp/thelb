---
title: Try it on one machine
lead: The lab builds a whole theLB setup inside one Linux machine - three directors, four backends, a router and a client - with nothing on your real network.
description: Try theLB on one Linux machine with make lab - a cluster in network namespaces with three services, direct routing, port mapping and IP-in-IP.
section: Get started
order: 4
nav: Try it on one machine
---

## Start the lab

You need a Linux machine, root (`sudo`), Go, `python3` and `curl`. From the source tree:

```bash
make lab
```

`make lab` builds a binary of its own (`bin/lab/thelb`) and starts the lab. Each machine is a **network namespace** - a separate network stack inside the same Linux kernel. They live on `198.18.0.0/24`, a range reserved for testing, so nothing reaches your own network.

| Node | Address | What |
|---|---|---|
| `d1`, `d2`, `d3` | `198.18.0.11`-`.13` | directors; the control VIP is `198.18.0.10`, port `7443` |
| `b1`, `b2`, `b3` | `198.18.0.31`-`.33` | backends, each with two small web servers: port 80, and 8080, 9090 or 7070 |
| `b4` | `198.19.0.41` | a backend behind a router, in another network |
| `c` | `198.18.0.100` | the client |

And three services:

| Service | VIP | What it shows |
|---|---|---|
| `web` | `198.18.0.20:80` | direct routing to port 80 on `b1`, `b2` and `b3` |
| `mapped` | `198.18.0.21:80` | each backend on a port of its own: it maps port 80 to 8080, 9090 or 7070 |
| `far` | `198.18.0.22:80` | IP-in-IP to `b4`, behind the router, and `b1` |

## Look around

`test/lab.sh` drives the lab. `run <node> <command>` runs a command on a node; `thelb` there is the lab's binary.

```bash
sudo test/lab.sh curl 12                  # the client fetches web 12 times, and counts who answered
sudo test/lab.sh curl 9 198.18.0.21       # the same for mapped
sudo test/lab.sh curl 8 198.18.0.22       # and far, by IP-in-IP
sudo test/lab.sh run d1 thelb status      # the summary; also: status directors, backends, vips, all
sudo test/lab.sh run d1 ipvsadm -Ln       # what theLB programmed into IPVS, on the leader
sudo test/lab.sh run b2 nft list table inet thelb   # b2's port mapping
sudo test/lab.sh logs d1                  # follow a node's log
```

:::screen curl
Each service answered by its backends, in turn.
:::

:::screen ipvsadm
What theLB told the kernel on the leader: one IPVS service per theLB service, with its backends and weights. `Route` is direct routing, `Tunnel` is IP-in-IP.
:::

## Break things

```bash
sudo test/lab.sh app b2 down       # b2's web servers stop: theLB gives b2 weight 0
sudo test/lab.sh app b2 up         # and it gets traffic again
sudo test/lab.sh kill d1           # the leader crashes: another director takes the VIPs
sudo test/lab.sh start d1          # it comes back, and rejoins without approval
sudo test/lab.sh stop b3           # b3 stops cleanly, and gives its VIPs up first
sudo test/lab.sh serve d2          # a backend on d2's machine, beside its director: d2-web
```

When the leader crashes, the new leader's log tells the story - the old leader is gone, the VIP is taken once it is free, and the dead director is taken out of the vote:

:::screen failover
The new leader's log after `kill d1`.
:::

See [When things fail](failures.md) for every case.

`start` takes extra flags, so you can try the metrics and debug logs:

```bash
sudo test/lab.sh stop d1
sudo test/lab.sh start d1 --debug --metrics 198.18.0.11:9100
```

## Take it away

```bash
make lab-down
```

This stops every node and deletes the namespaces. Nothing is left on the machine.

## A thousand services

`make scale N=1000` builds a lab of its own with three directors, three backends and a thousand services, each on its own VIP. It prints what that costs: how long it took to create the services and bring their VIPs up, the leader's CPU and memory at rest, how long `thelb status` takes, and a failover. On one CPU, a thousand services used about 10% of it on the leader, and every VIP was back about 10 seconds after the leader was killed.
