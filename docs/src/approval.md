---
title: Secrets and approval
lead: Two secrets let machines ask to join. A person approves each one. Together they decide who is in the cluster.
description: theLB's secret files, approving and removing nodes, node fingerprints, and what to do after a secret leaks.
section: Guides
order: 5
nav: Secrets and approval
---

## The two secret files

The cluster has two secrets, each in a file of its own:

| File | Goes to | Lets a machine |
|---|---|---|
| `director.secret` | every director | ask to join as a director |
| `backend.secret` | every director and every backend | ask to join as a backend |

- **Created once.** The first `--bootstrap` writes any missing file - mode `0600`, owned by root - and never overwrites one. You can also create them yourself first: `sudo thelb generate secret | sudo tee /etc/thelb/director.secret >/dev/null`, then `sudo chmod 0600` it.
- **Read only from files**, never from the command line. theLB refuses a file that anyone but root can read, or that is a symbolic link.
- **Never logged or printed** - except by `thelb generate secret`, and by `sudo thelb admin secret show --role director|backend` on a director, to repair a node whose file is out of date.
- **Not enough on their own.** A machine with the right secret can only *ask* to join. A person must approve it.

When every director has stopped, the next bootstrap creates the cluster again, with a new certificate authority and **the same secrets**.

## Approving nodes

Every new director and backend waits for approval:

```bash
sudo thelb status pending                         # who is waiting, with fingerprints
sudo thelb admin approve web-1 web-2              # by name
sudo thelb admin approve --match 'web-[0-9]+'     # by pattern; shows the matches and asks y/N
sudo thelb admin approve --match 'web-[0-9]+' --yes    # the same, for scripts
sudo thelb admin refuse web-9                     # it is told so, and stops
sudo thelb admin remove web-2                     # its certificate is revoked; it stops or leaves
```

:::screen approve
A backend waiting, approved, then assigned to a service.
:::

- **Compare the fingerprint.** A waiting node shows its **node fingerprint** in its own log and in `thelb status`. It identifies the machine. Check that the fingerprint on the director matches the one on the machine before you approve.
- **Approved once, a node rejoins by itself** after a restart - unless its fingerprint, address or MAC address changed.
- After **every** director has stopped, the cluster is new, and every node waits for approval again. `approve --match` approves many at once.
- A request that stops asking is dropped after an hour.
- `--match` takes a regular expression ([RE2](https://github.com/google/re2/wiki/Syntax) syntax) that must match the **whole** name.

## Inside the cluster

All traffic between nodes uses mutual TLS 1.3. Each approved node gets a certificate from the cluster's own certificate authority, and `thelb admin remove` revokes it.

## If a secret leaks

A leaked secret lets a machine ask to join, but not get in: someone still has to approve it. So:

1. Do not approve nodes you do not expect. Check `thelb status pending` and the fingerprints.
2. Remove any member that should not be there: `sudo thelb admin remove <name>`.
3. Replace the secret. Secret rotation (`thelb admin secret rotate`) is not built yet in {{minor}}. Until it is, start the cluster again with new secret files: stop every director, create new files with `thelb generate secret`, copy them to every node, start a director with `--bootstrap`, approve the nodes, and run your [saved commands](services.md#keeping-them-thelb-generate-commands).

See [Security](security.md) for what theLB trusts, and how to report a problem.
