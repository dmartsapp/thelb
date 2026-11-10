# Security

## Reporting a vulnerability

Please report it privately, through GitHub: the repository's **Security** tab, **Report a vulnerability**. Do not open a public issue for it.

Say what you found, which version (`thelb --version`) and how to reproduce it. You will hear back within a week; a fix goes into a patch release of every supported line, and the advisory is published with it, crediting you unless you ask otherwise.

## Supported versions

| Version | Supported |
|---|---|
| 0.1.x | yes |

Before 1.0, only the latest minor line gets fixes.

## Verifying a release

Every release carries the Linux binaries, `SHA256SUMS`, and build-provenance attestations made by GitHub Actions from the tagged source:

```text
sha256sum -c SHA256SUMS --ignore-missing
gh attestation verify thelb-0.1.0-linux-amd64 --repo dmartsapp/thelb
```

## What theLB trusts, by design

These are not vulnerabilities; the [manual](docs/manual.md) and the [design](docs/design.md) say how to run theLB with them in mind.

- **The directors' network.** VIPs move by ARP and neighbour discovery, which anyone on the link can answer. IPVS's connection synchronisation - multicast to `224.0.0.81`, UDP `8848` - is not authenticated by the kernel: a host on that link can inject connection entries. Keep the directors on a network only they and their backends share.
- **The secret files.** A role's secret lets a node ask to join; it never admits one by itself - every new node waits for an approval. Keep the files root-owned, mode `0600` (theLB refuses others). After a leak, rotate the secret (not built yet in 0.1: start the cluster again with new secret files) and remove any member that should not be there.
- **Root on the nodes.** theLB runs as root, with only the capabilities it needs under the unit `thelb generate systemd` prints; anyone with root on a director can act as it.

## What is in scope

theLB's own code: the cluster's protocol (mutual TLS 1.3, the join proof, approvals, certificates, revocation), the local socket's authorisation, how secrets are read and kept out of logs, the kernel state it writes, and the metrics endpoint.
