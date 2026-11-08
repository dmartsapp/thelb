# theLB

**The Load Balancer** - and, in Bengali, *"will you push?"*

theLB is a load balancer for Linux in one binary. Run it on your machines and they balance TCP and UDP traffic to a virtual IP, keep that IP alive when a machine fails, and - in a Kubernetes cluster - serve Services of type `LoadBalancer`. You should not need to be an expert in `sysctl`, `ipvsadm`, `iptables` or ARP to get there.

- **The kernel forwards**: IPVS in direct-routing mode - backends answer clients directly, with no proxy, NAT or `iptables` in the path.
- **IPv4 and IPv6** from the first release.
- **High availability**: a Raft cluster of directors - one works, three are recommended - moves a VIP when its node fails, and a node that loses the cluster gives its VIPs up rather than risk two nodes holding one.
- **One component everywhere**: the same binary configures the directors and the backends, and keeps checking that the kernel still matches what it needs.
- **Nothing left behind**: theLB changes only runtime state and never writes a configuration file. What would make its setup survive a reboot - a systemd unit, sysctl settings, Kubernetes manifests - it prints, for you to install if you choose.
- **Full authority or nothing**: it checks that it can do its whole job before it changes anything, and says exactly what is missing if it cannot.

## Status

**Design, and the first code.** [v0.1.0, unreleased](CHANGELOG.md) has the command line, the secret files, the node fingerprint, the certificate authority, the local socket and `thelb status` - and the cluster: directors joining with their secret and a director's approval, Raft, the leader holding the control VIP, and failover. Backends, IPVS, pools and services come next.

- [Philosophy](docs/philosophy.md) - the principles every decision follows;
- [Design](docs/design.md) - how it works, and the questions still open;
- [Diagrams](docs/diagrams.md) - both, in pictures.

## Building and testing

theLB is Linux only, and built with Go 1.27.3 or later. `make` is the one place to build and check from:

```text
make build                         # bin/thelb.linux.amd64 and bin/thelb.linux.arm64
make check                         # gofmt, go vet, go test -race, golangci-lint v2.15.0, govulncheck
make e2e                           # the built binary in a lab of network namespaces, as root (Linux, passwordless sudo)
make remote-check REMOTE=<host>    # make check on a Linux machine, over ssh
make remote-e2e REMOTE=<host>      # make e2e there
```

## License

[MIT](LICENSE) &copy; Farhan Sabbir Siddique
