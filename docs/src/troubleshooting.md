---
title: Troubleshooting
lead: Start with thelb doctor. It checks the machine, changes nothing, and tells you what to fix.
description: Find and fix problems with theLB - thelb doctor, and the most common problems with their fixes.
section: Guides
order: 9
nav: Troubleshooting
---

## `thelb doctor`

```bash
sudo thelb doctor
```

`doctor` changes nothing. It checks:

- the kernel, its modules and IPv6, and theLB's capabilities;
- the kernel settings theLB sets, and what the system sets at boot;
- whether theLB runs under systemd - with a long enough `RestartSec=` - or by hand;
- the firewall (ufw, firewalld or nftables), with the commands that open the control port and connection sync;
- SELinux, and software that gets in the way: keepalived, kube-proxy, Cilium, kube-router, other IPVS services;
- on a director, programs that listen on every address - they would answer on every VIP too;
- clouds where a VIP cannot move, and hypervisors that need a setting;
- the clock, and the cluster this machine's director sees;
- the network link: whether the gateway and the other directors answer, whether connection sync works, and whether this machine sees the leader's announcements;
- the plan: everything theLB would add, set and start on this machine.

It prints a table, then what to do. `--json` gives the same for scripts. It exits with `1` when theLB could not run on the machine, or could not do part of its work.

:::screen doctor
`thelb doctor` on a director in the lab.
:::

Where no director runs yet, give `--vip` and `--port`, so that doctor can show the plan and the firewall commands:

```bash
sudo thelb doctor --role director --vip 10.0.0.10 --port 7443
```

## Common problems

### A node waits for approval, and nothing happens

Approve it on a director: `sudo thelb admin approve <name>`. Check its fingerprint first - see [Secrets and approval](approval.md).

### "did not accept the secret"

The node's secret file is not this cluster's. On a director, print the right one, put it in the node's file, and start the node again:

```bash
sudo thelb admin secret show --role backend      # or --role director
```

### "nothing accepts a connection on port 7443"

The control VIP answers on the network, but its port does not. Most often a firewall on the leader drops it. Open the control port to the directors and backends:

```bash
sudo ufw allow from 10.0.0.0/24 to any port 7443 proto tcp
```

If no firewall is in the way, another machine has the address: choose another control VIP.

### A backend gets no traffic

Look at the leader's log for `backend not serving`. Its `reason=` says which condition failed:

| Reason | What to do |
|---|---|
| `no heartbeat` | the backend's theLB is not running, or cannot reach the control VIP |
| `its own check fails` | the application is down on the backend, or listens on another port |
| `it does not hold the VIP yet` | wait a moment; if it stays, look at the backend's log |
| `the director's check fails` | the directors cannot reach the application: check that it listens on all addresses, and that the firewall lets the directors in |
| `it is stopping` | the backend is shutting down |

### Clients get answers, but theLB counts nothing

`thelb status backends` counts no packets, though clients reach the service - or it counted a few at first, then stopped. The clients' packets no longer come to the leader: something sends them straight to a backend. Look at a client's ARP table (`arp -an`, or `ip neigh`):

```text
? (10.0.0.11) at 3a:78:95:bc:89:82 on en0     <- a director
? (10.0.0.21) at 3a:78:95:bc:89:82 on en0     <- a backend
? (10.0.0.50) at 3a:78:95:bc:89:82 on en0     <- the VIP
```

**Many machines behind one MAC address** - often one with the second hex digit 2, 6, A or E - is a bridge that translates MAC addresses: a Wi-Fi extender or wireless bridge that the directors' and backends' machines - or their hypervisor - are plugged into. It learns which address lives behind which machine from what they send. A backend's reply comes from the VIP's address, so the bridge moves the VIP to that backend, and sends it the clients' next packets directly. A capture on the leader (`sudo tcpdump -ni eth0 host 10.0.0.50`) shows nothing while a client connects.

Direct routing cannot work through such a bridge. Connect the machines by cable, or set the bridge to a mode that keeps MAC addresses (WDS, 4-address, access point). Clients on the same side of the bridge as the backends are not affected. Afterwards, the client's ARP table shows each machine's own MAC address, and the VIP at the leader's.

### A tunnelled service does not answer

- Check that the backend has the `ipip` module (`sudo modprobe ipip`) and that IP protocol 4 from the directors is allowed.
- Check that the backends' routers allow replies **from the VIPs' addresses**. Source-address checks (uRPF, cloud source/destination checks) drop them. See [Direct routing and tunnels](forwarding.md#what-a-tunnelled-backend-needs).

### `WARN drift repaired` again and again

Something else keeps changing theLB's kernel setup - a script, a person, another network tool - and theLB keeps putting it back. Find what does it and stop it, or the two will undo each other every 10 seconds.

### Two directors, and no leader

The log says `not standing alone reason=...`. The reason tells you why:

- `a partition, not a crash`: the other director answers on the network but not on its control port. Open the control port between them.
- `waiting out the control VIP's ... lease`: wait - it has not waited long enough yet.
- `... still answers for the control VIP`: something still holds the VIP. Check `ip address` on the other machine.
- `does not reach its network`: this director's own link or gateway is down.

See [Directors and failover](directors.md#two-directors) for the rules.

## More help

- [Log messages](messages.md) lists the messages that need action, and what to do.
- [When things fail](failures.md) says what theLB does in every failure.
- `thelb status` on each node, and the logs, are the first places to look.
- Still stuck? [Open an issue](https://github.com/dmartsapp/thelb/issues) with the output of `thelb doctor` and the log around the problem.
