# theLB

**The Load Balancer** - and, in Bengali, *"will you push?"*

theLB is a load balancer for Linux in one binary. Run it on your machines and they balance TCP and UDP traffic to a virtual IP, keep that IP alive when a machine fails, and - in a Kubernetes cluster - serve Services of type `LoadBalancer`. You should not need to be an expert in `sysctl`, `ipvsadm`, `iptables` or ARP to get there.

- **The kernel forwards**: IPVS in direct-routing mode - backends answer clients directly, with no proxy, NAT or `iptables` in the path.
- **IPv4 and IPv6** from the first release.
- **High availability**: a Raft cluster of directors - one works, three are recommended - moves a VIP when its node fails, and a node that loses the cluster gives its VIPs up rather than risk two nodes holding one.
- **One component everywhere**: the same binary configures the directors and the backends, and keeps checking that the kernel still matches what it needs.
- **Nothing left behind**: theLB changes only runtime state and never writes a configuration file. What would make its setup survive a reboot - a systemd unit, sysctl settings, Kubernetes manifests - it prints, for you to install if you choose.
- **Full authority or nothing**: it checks that it can do its whole job before it changes anything, and says exactly what is missing if it cannot.

## Documentation

**[dmartsapp.github.io/thelb](https://dmartsapp.github.io/thelb/docs/)** - in simple English, with screenshots of the real output:

- [Install](https://dmartsapp.github.io/thelb/docs/install.html) and the [quick start](https://dmartsapp.github.io/thelb/docs/quickstart.html): one director, two backends and a service;
- [Try it on one machine](https://dmartsapp.github.io/thelb/docs/lab.html): a whole cluster in network namespaces;
- [Directors and failover](https://dmartsapp.github.io/thelb/docs/directors.html): the VIP lease, and what happens when a machine fails;
- [Troubleshooting](https://dmartsapp.github.io/thelb/docs/troubleshooting.html) and the [commands](https://dmartsapp.github.io/thelb/docs/commands.html);
- [How it works](https://dmartsapp.github.io/thelb/docs/tech-design.html), and the full [design](docs/design.md) with its [diagrams](docs/diagrams.md); [where the code is](docs/build-state.md).

## Status

**v0.1.0, the first release** ([changelog](CHANGELOG.md)): directors forming a cluster with Raft - joining with a secret and an approval, the leader holding the control VIP, failover, two directors surviving either one's crash - and backends serving services through IPVS, by direct routing or IP-in-IP, with port mapping and health checks from both sides, shared across the directors. VIPs leased from the kernel, connection synchronisation, drift repair, `thelb status`, `doctor`, `generate` (systemd units, kernel parameters, the cluster's commands, Kubernetes manifests) and Prometheus metrics. **Kubernetes**: Services of type `LoadBalancer` and class `dmarts.app/thelb` get their VIPs from theLB, which sends their traffic straight to the nodes running their pods - nothing of theLB on the nodes; directors outside the cluster or on its nodes, under systemd or as a DaemonSet ([Kubernetes](https://dmartsapp.github.io/thelb/docs/kubernetes.html)). Linux only.

## Releases

Each [release](https://github.com/dmartsapp/thelb/releases) has the Linux binaries (amd64, arm, arm64), their SHA-256 sums, SBOMs (CycloneDX and SPDX), and attestations of their build provenance and SBOMs, and a container image of the same binaries, `ghcr.io/dmartsapp/thelb:<version>`, for directors in a Kubernetes cluster - see [Install](https://dmartsapp.github.io/thelb/docs/install.html) to download and check one, and [SECURITY.md](SECURITY.md) to report a vulnerability. Releases are tagged `vX.Y.Z` on `release/vX.Y` branches: the tag is the only thing that runs a workflow ([Releasing](https://dmartsapp.github.io/thelb/docs/tech-release.html)). The end-to-end test runs on a development machine before each release, not in CI.

## Building and testing

theLB is Linux only, and built with Go 1.27.4 or later. `make` is the one place to build and check from:

```text
make build                         # bin/thelb.linux.amd64, .arm and .arm64
make check                         # gofmt, go vet, go test -race, golangci-lint v2.15.0, govulncheck
make e2e                           # the kernel tests, and the binary in a lab of network namespaces (Linux, passwordless sudo)
make e2e-k8s                       # the Kubernetes provider in a lab, against a stand-in API server (make e2e runs it too)
make remote-check REMOTE=<host>    # make check on a Linux machine, over ssh
make remote-e2e REMOTE=<host>      # make e2e there
make remote-e2e-k8s REMOTE=<host>  # make e2e-k8s there
make lab                           # a lab to try by hand, on one Linux machine (root)
make lab-down                      # take it away
make remote-screens REMOTE=<host>  # the documentation's screenshots, captured there
python3 docs/build.py              # the documentation site, from docs/src
```

## License

[MIT](LICENSE) &copy; Farhan Sabbir Siddique
