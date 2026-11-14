---
title: Quick start
lead: One director, two backends and one web service, in five steps.
description: Build a theLB cluster - start the first director, join two backends, approve them, and serve a web service on a VIP.
section: Get started
order: 3
nav: Quick start
---

## The example network

In this guide all machines are on `10.0.0.0/24`:

| Machine | Address | Role |
|---|---|---|
| `lb-1` | `10.0.0.11` | the first director |
| `web-1`, `web-2` | `10.0.0.21`, `10.0.0.22` | backends, running nginx on port 80 |
| - | `10.0.0.10` | the **control VIP**: the cluster's own address, port `7443` |
| - | `10.0.0.20` and up | VIPs for services |

Pick addresses that nothing else uses. theLB checks them before it uses them.

:::note About the screenshots
The screenshots on this site come from the [lab](lab.md), a whole cluster on one machine. There the directors are called `d1`-`d3`, the backends `b1`-`b4`, the network is `198.18.0.0/24`, and the control VIP is `198.18.0.10`. The commands are the same.
:::

## 1. Start the first director

On `lb-1`, start a director with `--bootstrap`:

```bash
sudo thelb director --vip 10.0.0.10 --port 7443 --bootstrap \
  --secret-file /etc/thelb/director.secret --backend-secret-file /etc/thelb/backend.secret
```

What happens:

1. It checks that the machine has everything theLB needs (this is called *preflight*).
2. It looks for an existing cluster on the control VIP for 5 seconds, and finds none.
3. It creates the cluster, and - because the files do not exist yet - the two **secret files**, readable by root only.
4. It takes the control VIP and becomes the leader.

:::screen director-start
The first director's log: preflight, the secret files, a new cluster, connection sync, leadership. (The lab's `d1` also runs with `--metrics` and `--debug`: hence their two lines.)
:::

Leave it running. (Later, [run it under systemd](systemd.md) instead.)

## 2. Copy the backend secret

Copy `/etc/thelb/backend.secret` from `lb-1` to each backend, readable by root only:

```bash
sudo install -o root -m 0600 backend.secret /etc/thelb/backend.secret   # on web-1 and web-2
```

The secret lets a backend *ask* to join. It does not let it in: a director still has to approve it.

## 3. Start the backends

On `web-1` and `web-2`, with nginx already running on port 80:

```bash
sudo thelb backend --join 10.0.0.10:7443 --secret-file /etc/thelb/backend.secret \
  --offer web=tcp/80,check=http:/
```

`--offer web=tcp/80,check=http:/` means: "I have an application. Call it `web`. It is on TCP port 80. Check it with an HTTP request for `/`."

The backend joins and waits for approval. Its log shows its **node fingerprint** - a code that identifies this machine:

:::screen pending-log
A new backend's log: it reaches the cluster, and waits to be approved.
:::

## 4. Approve the backends

On the director, list the nodes that are waiting:

```bash
sudo thelb status pending
```

Check that each fingerprint matches the one in the backend's own log. Then approve them, by name:

```bash
sudo thelb admin approve web-1 web-2
```

Or approve many at once with a pattern - theLB shows the matching nodes and asks you to confirm:

```bash
sudo thelb admin approve --match 'web-[0-9]+'
```

:::screen approve
The waiting list, the approval, and assigning the new backend to a service.
:::

Once approved, the backend says `backend ready`:

:::screen backend-join
A backend's log from start to ready: preflight, joining, approval, its offers.
:::

## 5. Create a service

Three commands on the director. A **pool** is a range of addresses for VIPs. A **service** is a VIP, a protocol and a port. An **assignment** connects a backend's offer to a service.

```bash
sudo thelb admin pool add main --vip 10.0.0.20/24 --vippool-count 10
sudo thelb admin service add web --pool main --proto tcp --port 80
sudo thelb status offers
sudo thelb admin assign web web-1:web web-2:web
```

The service takes the pool's first free address, `10.0.0.20`. `status offers` lists each backend's offers; `-` under *service* means "not assigned yet".

:::screen services
The same steps in the lab: a pool, a service, the offers, and the assignment.
:::

Within a few seconds both backends pass their health checks, and the leader brings the VIP up. Check it, then try it from a client:

```bash
sudo thelb status vips
curl http://10.0.0.20/
```

:::screen status-vips
The VIPs, who holds them, and the services on them.
:::

That is a working load balancer. Next:

- add two more directors, so that one can fail: [Directors and failover](directors.md);
- keep it running after a reboot: [Running under systemd](systemd.md);
- save your pools, services and assignments: [Keeping them](services.md#keeping-them-thelb-generate-commands).
