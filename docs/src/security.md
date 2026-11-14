---
title: Security
lead: How to check a release, what theLB trusts by design, and how to report a vulnerability.
description: theLB security - verifying releases with checksums, attestations and SBOMs, the trust model, and reporting vulnerabilities privately.
section: Reference
order: 7
nav: Security
---

## Report a vulnerability

Please report it **privately**, through GitHub: the repository's **Security** tab, **Report a vulnerability**. Do not open a public issue for it.

Say what you found, which version (`thelb --version`), and how to reproduce it. You will hear back within a week. The fix goes into a patch release of every supported line, and the advisory is published with it, crediting you unless you ask otherwise.

| Version | Supported |
|---|---|
| {{minor}}.x | yes |

Before 1.0, only the latest minor line gets fixes.

## Check a release

Every release has, for each binary:

| File | What it is |
|---|---|
| `thelb-<version>-linux-<arch>` | the binary |
| `....sha256` | its SHA-256 checksum; `SHA256SUMS` has all three |
| `....cdx.json`, `....spdx.json` | its software bill of materials (SBOM), CycloneDX 1.6 and SPDX 2.3: the Go release and every module it was built from |

And GitHub-signed attestations: one saying which workflow built the binary from which commit (build provenance), and one for each SBOM.

```bash
sha256sum -c SHA256SUMS --ignore-missing
gh attestation verify thelb-{{version}}-linux-amd64 --repo dmartsapp/thelb
gh attestation verify thelb-{{version}}-linux-amd64 --repo dmartsapp/thelb --predicate-type https://cyclonedx.org/bom
gh attestation verify thelb-{{version}}-linux-amd64 --repo dmartsapp/thelb --predicate-type https://spdx.dev/Document/v2.3
```

The SBOMs list exactly what is inside the binary, so you can check it against vulnerability databases with your own tools.

## What theLB trusts, by design

These are not vulnerabilities, but you should run theLB with them in mind.

- **The directors' network.** VIPs move by ARP and neighbour discovery, which anyone on the network can answer. IPVS connection sync - multicast to `224.0.0.81`, UDP `8848` - is not authenticated by the kernel: a machine on that network can inject connection entries. Keep the directors on a network that only they and their backends share.
- **The secret files.** A secret lets a machine ask to join; it never lets one in by itself - every new node waits for approval. Keep the files owned by root, mode `0600` (theLB refuses others). After a leak, see [If a secret leaks](approval.md#if-a-secret-leaks).
- **Root on the nodes.** theLB runs as root, with only the capabilities it needs under the unit `thelb generate systemd` prints. Anyone with root on a director can act as that director.
- **The metrics endpoint** is plain HTTP without authentication. Give `--metrics` an address only your monitoring reaches.

## What is in scope

theLB's own code: the cluster's protocol (mutual TLS 1.3, the join proof, approvals, certificates, revocation), the local socket's authorisation, how secrets are read and kept out of logs, the kernel state it writes, and the metrics endpoint.

The same policy is in the repository's [SECURITY.md](https://github.com/dmartsapp/thelb/blob/main/SECURITY.md).
