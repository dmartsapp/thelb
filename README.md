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

**Design, and the first code.** [v0.1.0, unreleased](CHANGELOG.md) has the command line, the secret files, the node fingerprint, the certificate authority, the local socket and `thelb status` - and the cluster: directors joining with their secret and a director's approval, Raft, the leader holding the control VIP, and failover. And backends: pools, services, assignments, and the kernel's IPVS forwarding to them by direct routing, with health checks from both sides. A backend maps a service's port to its application's where they differ. `doctor`, `generate` and the configuration file come next.

- [Philosophy](docs/philosophy.md) - the principles every decision follows;
- [Design](docs/design.md) - how it works, and the questions still open;
- [Diagrams](docs/diagrams.md) - both, in pictures;
- [Build state](docs/build-state.md) - where the code is, what is broken, and what comes next.

## Building and testing

theLB is Linux only, and built with Go 1.27.3 or later. `make` is the one place to build and check from:

```text
make build                         # bin/thelb.linux.amd64 and bin/thelb.linux.arm64
make check                         # gofmt, go vet, go test -race, golangci-lint v2.15.0, govulncheck
make e2e                           # the built binary in a lab of network namespaces, as root (Linux, passwordless sudo)
make remote-check REMOTE=<host>    # make check on a Linux machine, over ssh
make remote-e2e REMOTE=<host>      # make e2e there
make lab                           # a lab to try by hand, on one Linux machine (root)
make lab-down                      # take it away
```

### Trying it on one machine

`make lab` builds, in network namespaces on `198.18.0.0/24` - so nothing reaches the machine's own network - a cluster of three directors, three backends each running two small web servers, and a client; then a pool and two services: `web` on `198.18.0.20:80`, from port 80 on every backend, and `mapped` on `198.18.0.21:80`, from 8080, 9090 and 7070 - each backend mapping the port. `test/lab.sh` drives it:

```text
sudo test/lab.sh curl 12                    # the client fetches web 12 times, and counts who answered
sudo test/lab.sh curl 9 198.18.0.21         # the same for mapped
sudo test/lab.sh run b2 nft list table inet thelb   # b2's port mapping: 80 to 9090
sudo test/lab.sh run d1 thelb status        # the summary; then status directors, backends, vips, all
sudo test/lab.sh run d1 ipvsadm -Ln         # what theLB programmed in IPVS, on the leader
sudo test/lab.sh app b2 down                # a backend's application fails: weight 0; app b2 up brings it back
sudo test/lab.sh serve d1                   # a backend beside d1's director, on its host: d1-web
sudo test/lab.sh kill d1                    # the leader crashes: another director takes the VIPs
sudo test/lab.sh start d1                   # it comes back, cleans up what it left, and rejoins
sudo test/lab.sh down                       # takes it all away
```

## License

[MIT](LICENSE) &copy; Farhan Sabbir Siddique
