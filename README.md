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

**Design.** There is no code yet. Start with:

- [Philosophy](docs/philosophy.md) - the principles every decision follows;
- [Design](docs/design.md) - how it works, and the questions still open.

## License

[MIT](LICENSE) &copy; Farhan Sabbir Siddique
