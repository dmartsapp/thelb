---
title: theLB
lead: A load balancer for Linux in one small program. Your machines share a virtual IP address, the Linux kernel spreads the traffic over your servers, and the address moves by itself when a machine fails.
description: theLB is a load balancer for Linux in one binary - IPVS direct routing or IP-in-IP, a Raft cluster of directors that moves the virtual IP when a machine fails, health checks from both sides, and nothing left behind.
section: Get started
order: 1
nav: Overview
---

:::html
<div class="hero-actions">
  <a class="btn primary" href="quickstart.html">Quick start</a>
  <a class="btn" href="install.html">Install theLB</a>
  <a class="btn" href="https://github.com/dmartsapp/thelb/releases/latest">Download the latest release</a>
</div>
:::

## What is theLB?

theLB - *The Load Balancer* - lets a group of Linux machines share one address, a **virtual IP** (VIP). Clients send their traffic to the VIP. theLB hands each new connection to one of your servers, and the server answers the client **directly**. When a machine fails, another one takes the VIP over within seconds.

You run the same program, `thelb`, on every machine. You do not need to know `ipvsadm`, `sysctl`, `iptables` or ARP: theLB sets all of that up, checks it, and removes it when it stops.

:::html
<div class="facts">
  <div class="fact"><b>1 file</b><span>one static binary for every machine</span></div>
  <div class="fact"><b>0 config files</b><span>flags and the cluster's own state</span></div>
  <div class="fact"><b>6 s</b><span>VIP lease: a crashed machine lets go by itself</span></div>
  <div class="fact"><b>IPv4 + IPv6</b><span>VIPs of either kind</span></div>
</div>
:::

## How it works

:::html
<div class="diagram">
<svg viewBox="0 0 660 340" role="img" aria-label="A client sends a request to the VIP on the leading director; the director forwards it to one backend; the backend replies straight to the client">
  <defs><marker id="arr" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse"><path d="M0 0L10 5L0 10z"/></marker></defs>
  <rect class="box" x="20" y="120" width="120" height="60" rx="10"/>
  <text class="h" x="80" y="146" text-anchor="middle">Client</text>
  <text class="sub" x="80" y="165" text-anchor="middle">anywhere</text>
  <rect class="box accent" x="240" y="105" width="180" height="90" rx="10"/>
  <text class="h" x="330" y="135" text-anchor="middle">Director (the leader)</text>
  <text class="sub" x="330" y="155" text-anchor="middle">holds the VIP</text>
  <text class="sub" x="330" y="172" text-anchor="middle">the kernel's IPVS forwards</text>
  <rect class="box" x="520" y="30" width="120" height="50" rx="10"/>
  <text class="h" x="580" y="52" text-anchor="middle">Backend 1</text>
  <text class="sub" x="580" y="69" text-anchor="middle">your app</text>
  <rect class="box" x="520" y="125" width="120" height="50" rx="10"/>
  <text class="h" x="580" y="147" text-anchor="middle">Backend 2</text>
  <text class="sub" x="580" y="164" text-anchor="middle">your app</text>
  <rect class="box" x="520" y="220" width="120" height="50" rx="10"/>
  <text class="h" x="580" y="242" text-anchor="middle">Backend 3</text>
  <text class="sub" x="580" y="259" text-anchor="middle">your app</text>
  <path class="arrow" d="M140 150H238"/>
  <text class="sub" x="189" y="140" text-anchor="middle">1. to the VIP</text>
  <path class="arrow" d="M420 140L518 58"/>
  <path class="arrow" d="M420 150H518"/>
  <path class="arrow" d="M420 160L518 242"/>
  <text class="sub" x="330" y="215" text-anchor="middle">2. to one backend</text>
  <path class="arrow" d="M580 270C580 330 80 330 80 182" stroke-dasharray="6 5"/>
  <text class="sub" x="330" y="318" text-anchor="middle">3. the reply goes straight back to the client</text>
</svg>
</div>
:::

1. The client connects to the VIP. The VIP lives on one machine, the **leading director**.
2. The director's kernel (its built-in load balancer, IPVS) picks a healthy **backend** and passes the packet on. It does not change the packet's addresses.
3. The backend answers the client directly, from the VIP's address. Replies never pass back through the director, so one director can carry a lot of traffic.

There are two roles:

- **Directors** receive the traffic and decide where it goes. They form a cluster (with Raft). One of them, the leader, holds the VIPs. One director is enough to start; **three are recommended**, so that one can fail.
- **Backends** run your applications - a web server, an API, a DNS server. Each backend *offers* its applications to the cluster. A director *assigns* them to services.

## See it work

The summary of a lab cluster - three directors, four backends, three services:

:::screen status
`thelb status` on a director: the cluster, and how many of each thing are up.
:::

The client fetches the `web` service twelve times. Three backends share the work:

:::screen curl
The lab's client asking each service a few times, and counting who answered.
:::

## Why people use it

- **The kernel does the forwarding.** theLB programs IPVS, which is part of Linux. There is no proxy in the path, and no address translation: your application sees the client's real address.
- **It survives failures.** If the leader crashes, another director takes the VIP. Open connections keep going, because the directors share their connection tables.
- **It never takes a VIP that is still in use.** The VIP is *leased* from the kernel: if theLB is killed, its kernel removes the VIP a few seconds later. A new leader takes the VIP only after it checks, twice, that no machine still answers for it. See [Directors and failover](directors.md).
- **Health checks from both sides.** A backend checks its own application, and the directors check it too. A backend gets traffic only while both checks pass.
- **Nothing left behind.** theLB changes only the running system. It writes no configuration file, and removes everything when it stops. To make a setup survive a reboot, it *prints* what you need - a systemd unit, kernel settings - for you to install.
- **It tells you what is wrong.** Before it changes anything, theLB checks that it can do its whole job. If it cannot, it says exactly what is missing. `thelb doctor` checks a machine and tells you what to fix.

## Where to go next

:::html
<div class="cards">
  <a class="card" href="install.html"><strong>Install</strong><span>Download one file, check it, and put it on each machine.</span></a>
  <a class="card" href="quickstart.html"><strong>Quick start</strong><span>One director, two backends and a service, in five steps.</span></a>
  <a class="card" href="lab.html"><strong>Try it on one machine</strong><span>A whole cluster in network namespaces, with nothing on your real network.</span></a>
  <a class="card" href="directors.html"><strong>Directors and failover</strong><span>More directors, the VIP lease, and what happens when one fails.</span></a>
  <a class="card" href="troubleshooting.html"><strong>Troubleshooting</strong><span>Start with <code>thelb doctor</code>.</span></a>
  <a class="card" href="commands.html"><strong>Commands</strong><span>Every command and flag.</span></a>
</div>
:::

## Status

theLB {{version}} is the first release. It runs on **Linux only** (amd64, arm64 and 32-bit arm). Kubernetes support - serving Services of type `LoadBalancer` - comes next. See the [changelog](changelog.md) for what each release changed.
