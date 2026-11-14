---
title: Running under systemd
lead: theLB installs nothing by itself. It prints a systemd unit for you to install, so it starts at boot and restarts if it fails.
description: Run theLB under systemd - thelb generate systemd, the watchdog, the hardening in the unit, and the kernel settings from boot.
section: Guides
order: 6
nav: Running under systemd
---

## Print a unit, then install it

`thelb generate systemd` prints a unit for a role and the flags you give it. It checks the flags the same way the role would:

```bash
sudo thelb generate systemd director --vip 10.0.0.10 --port 7443 --bootstrap \
  --secret-file /etc/thelb/director.secret --backend-secret-file /etc/thelb/backend.secret \
  | sudo tee /etc/systemd/system/thelb-director.service

sudo thelb generate systemd backend --join 10.0.0.10:7443 \
  --secret-file /etc/thelb/backend.secret --offer web=tcp/80,check=http:/ \
  | sudo tee /etc/systemd/system/thelb-backend.service
```

Then start it:

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now thelb-director     # or thelb-backend
journalctl -u thelb-director -f                # follow its log
```

Here is a director's unit:

:::screen generate-systemd
A director's unit, for the binary in `/usr/local/bin`.
:::

## What is in the unit

- **`Type=notify`**: theLB tells systemd when it has started.
- **`WatchdogSec=30`**: theLB tells systemd that it is alive - but only while its main loops keep running. If a loop stops for 30 seconds, systemd restarts theLB.
- **`Restart=always`**, with **`RestartSec=30` for a director** and 1 for a backend. The 30 seconds matter with two directors - see [Two directors](directors.md#two-directors).
- **Hardening that does not get in theLB's way**:
  - its capabilities are limited to `CAP_NET_ADMIN`, `CAP_NET_RAW` and `CAP_NET_BIND_SERVICE`;
  - `ProtectSystem=full`, with the secret files' folder writable only with `--bootstrap` (the first bootstrap writes missing files);
  - `NoNewPrivileges=`, `ProtectHome=`, `PrivateTmp=` and more.
  - It does not set `ProtectKernelTunables=`, because theLB writes to `/proc/sys/net`.

:::tip Keep files in their usual places
Keep the binary in `/usr/local/bin` and the secret files in `/etc/thelb`. A file in a home folder makes the unit use `ProtectHome=read-only` instead of hiding homes; a file in `/tmp` drops `PrivateTmp=` - and `/tmp` does not survive a reboot.
:::

## The flags are the whole setup

There is no configuration file: the flags in the unit are the node's whole setup.

- A backend offers its `--offer` flags at every start.
- Offers made later with `thelb offer add` are kept by the cluster, and a restarted backend takes them back.
- To withdraw an offer, use `thelb offer remove <name>`. Taking a flag out of the unit does not withdraw what the cluster holds.

## Kernel settings and modules from boot

theLB sets the kernel settings it needs when it starts, and puts them back when it stops. If you want them set from boot anyway, theLB prints them:

```bash
sudo thelb generate sysctl director | sudo tee /etc/sysctl.d/90-thelb-director.conf
sudo thelb generate modules director | sudo tee /etc/modules-load.d/thelb-director.conf
```

Use `backend` instead of `director` on a backend. This is optional: theLB works without them.
