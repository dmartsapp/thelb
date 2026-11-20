---
title: Philosophy
lead: Ten rules that decide every design question in theLB. When a choice is unclear, the one that keeps them wins.
description: The principles behind theLB - one binary and nothing left behind, the kernel as the truth, full authority or nothing, fail closed, and more.
section: Technical
order: 1
nav: Philosophy
---

## 1. One binary, and nothing left behind

theLB is one static binary. There is no package, no helper script, nothing else to install.

**It never makes a change that outlives it.** It writes no unit files, no `/etc/sysctl.d` or `/etc/modules-load.d` entries, no firewall rules, no cron jobs. Everything it changes - kernel settings, addresses, the IPVS table - is runtime state, which a reboot clears.

**Making things permanent is your decision.** What theLB would need to survive a reboot, it *prints* - a systemd unit, kernel settings - for you to read, change and install your own way. `thelb generate` prints; it never writes a file.

**One exception: the secret files.** The first time a cluster is created, theLB writes its two secrets to the files you named - only if they do not exist, readable by root only, never overwritten - because every node needs them from its very first start.

## 2. The running kernel is the truth

theLB decides what to do from the running kernel - the current settings, the addresses on each interface, the IPVS table - not from configuration files.

- **What is already right is left alone.** A setting that already has the right value was set by someone, probably on purpose. theLB does not touch it, and never reverts it.
- **What it changes, it owns, and puts back.** A setting theLB changes is recorded with its old value and restored when theLB stops. Addresses and IPVS entries it creates are marked as its own, and removed when it stops. It never removes or rewrites what it did not create.

## 3. Full authority, or it does not start

A load balancer that half works is worse than one that refuses to start. Before it changes anything, theLB checks that it can do **all** of its job: its Linux capabilities, that it can write the kernel settings it needs, that the kernel has IPVS (and IPv6 for an IPv6 VIP). If anything is missing, it stops, says exactly what is missing, and how to fix it.

That is this machine's authority. Another machine's is not: when the Kubernetes API server does not answer, or refuses a director, the director still starts, serves every service that does not need Kubernetes, says why, and serves Kubernetes once it can.

## 4. Fail closed

Two machines answering for the same VIP is worse than a VIP that is briefly down: clients flip between them, and connections break in ways that are hard to see. So a director holds a VIP only while the cluster agrees it should. A leader that loses the majority gives its VIPs up, even if it feels healthy. And the VIPs are leased from the kernel: a theLB that is killed loses them by itself, a few seconds later.

## 5. The kernel forwards; theLB decides

The kernel's IPVS forwards the traffic, with direct return: the director only changes the destination MAC address (or wraps the packet in a tunnel), and the backend answers the client directly. There is no proxy in the path and **no address translation on the director**. theLB is the control plane: it decides what the kernel should do, and checks that it still does.

## 6. One component everywhere

The same binary runs on every machine: directors and backends. On each, it watches its own work: it checks that the kernel still matches what it needs, puts back what drifted - someone ran `ipvsadm -C`, an address was removed - and says so.

## 7. Show everything

Nothing theLB does should be a surprise. `thelb doctor` checks a machine and shows every change theLB would make, before it makes any. `thelb status` shows the whole cluster in one place. Every change is logged: what, from what, and why.

## 8. Plain Linux first, Kubernetes on top

theLB works on plain Linux machines with no Kubernetes anywhere. Kubernetes support is built on the same core: Kubernetes becomes one more source of "what to balance", not a requirement.

## 9. IPv4 and IPv6 alike

IPv6 is not a later feature. VIPs, announcements, health checks and the cluster handle both from the first release.

## 10. A small, honest scope

theLB does one thing - layer-4 load balancing with direct return - and says plainly where that does not work: for example, on cloud platforms that do not let an address move between machines. `thelb doctor` detects those cases.
