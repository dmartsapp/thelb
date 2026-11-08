# Philosophy

theLB is a load balancer for Linux in one binary. Run it, and the box it runs on balances traffic to a virtual IP (VIP) across a set of backends, keeps that VIP alive across a cluster of nodes, and - in a Kubernetes cluster - serves Services of type `LoadBalancer`. You should not need to know `sysctl`, `ipvsadm`, `iptables` or ARP to get there.

These principles decide every design question. When a choice is unclear, the one that keeps them wins.

## 1. One binary, and nothing left behind

theLB is a single static binary. There is nothing to install, no package, no helper scripts.

**It never makes a change that outlives it.** It writes no unit files, no `/etc/sysctl.d` entries, no `/etc/modules-load.d` entries, no firewall rules, no cron jobs. Everything it changes - kernel parameters, addresses on interfaces, the IPVS table - is runtime state, which a reboot clears.

**Persistence is the user's decision.** What theLB would need to survive a reboot, it prints - a systemd unit, a sysctl configuration file, Kubernetes manifests - for the user to read, change and install the way their machines are managed. `thelb generate` prints; it never writes a file.

**One exception: the cluster's secret files.** The first time a cluster is bootstrapped, theLB writes its secrets to the files the user named with `--secret-file` and `--backend-secret-file` - only if they do not exist, readable by root only, never overwritten - because every node needs the same secret from its very first start. Nothing else is ever written.

## 2. The running kernel is the truth

theLB decides what to do from the state of the running kernel - the current value of every kernel parameter it needs, the addresses on each interface, the IPVS table, the loaded modules - not from configuration files.

**What is already right is left alone.** A parameter that already has the value theLB needs was set by someone - most likely made persistent by the user. theLB does not touch it, and never reverts it.

**What it changes, it owns, and puts back.** A parameter theLB had to change is recorded with its original value, and restored when theLB stops cleanly. Addresses and IPVS entries it created are marked as its own and removed when it stops or gives them up. It never removes or rewrites state it did not create.

`thelb doctor` also reads the system's persistent configuration (`/etc/sysctl.d` and the rest) - read only - to tell the user whether what theLB needs would survive a reboot, and prints what to add if not.

## 3. Full authority, or it does not start

A load balancer that half works is worse than one that refuses to start. Before it changes anything, theLB checks that it can do **all** of its job on this machine:

- the Linux capabilities it needs (`CAP_NET_ADMIN`, `CAP_NET_RAW`, and `CAP_SYS_MODULE` only when a kernel module has to be loaded);
- that the kernel parameters it needs are writable (they are not in an unprivileged container);
- that the kernel has IPVS, the schedulers it will use, and IPv6 if a VIP is IPv6;
- in Kubernetes: that it can reach the API server and holds every permission it needs - checked with the API's own access review, not assumed.

If anything is missing, theLB stops before doing anything, says exactly what is missing, and prints how to grant it.

## 4. Fail closed

Two nodes announcing the same VIP is worse than a VIP that is briefly down: clients flap between them and connections break in ways that are hard to see. So a node holds a VIP only while the cluster agrees it should. A node that loses contact with the majority of the cluster, or cannot renew its claim in time, gives its VIPs up - even if it seems healthy to itself.

## 5. The kernel forwards; theLB decides

Traffic is forwarded by the kernel's IPVS with direct return: the load balancer only rewrites the destination MAC address (or, across subnets, wraps the packet in a tunnel), and the backend answers the client directly. There is no proxy in the data path and **no NAT on the director**. A backend uses NAT only when asked to - to map a service's port to the port its application really listens on - in theLB's own table, at runtime. theLB itself is the control plane: it decides what the kernel should do, and checks that it is still doing it.

A proxying mode is a later, separate capability (see the [design](design.md#phases)). It will not change how direct routing works.

## 6. One component everywhere

The same binary runs on every node involved - the directors that hold the VIPs and the backends behind them - and on each it is its own watchdog: it keeps checking that the kernel still matches what it needs, puts back what drifted (someone ran `ipvsadm -C`, an address was removed), and says so.

## 7. Show everything

Nothing theLB does should be a surprise:

- `thelb doctor` checks the machine, explains what it finds, and prints every change theLB would make, before it makes any;
- `thelb status` shows, in one place, who leads, every node and when it last checked in, who holds which VIP, and the health of every backend - as tables, or as JSON;
- every change it makes is logged with what it changed, from what, and why.

## 8. Plain Linux first, Kubernetes on top

theLB works on plain Linux machines with no Kubernetes anywhere. The Kubernetes provider is built on the same core: Kubernetes becomes one more source of "what to balance", not a requirement.

## 9. IPv4 and IPv6 alike

IPv6 is not a later feature. Every part - VIPs, announcements, health checks, the cluster, Kubernetes - handles both from the first release, and is tested with both.

## 10. A small, honest scope

theLB does one thing - L4 load balancing with direct routing on a shared L2 network - and says plainly where that does not work: across routed networks, and on cloud platforms that do not let a VIP move between machines. It does not pretend otherwise; `thelb doctor` detects those cases.
