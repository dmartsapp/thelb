# Changelog

Notable changes to theLB, newest first. Versions follow [semantic versioning](https://semver.org); a change that scripts could notice is called out in bold.

## v0.1.0 - unreleased

The first code: everything a director needs on its own node, with nothing sent on the network. Raft, joining a cluster, and the network checks come next.

- **The command line**: `thelb director`, `backend`, `status`, `doctor`, `generate` and `admin`, and `--version`. Every command runs only as root (effective user ID 0); the help and `--version` run for anyone. Exit statuses: `0` done, `1` failed, `2` used wrongly. What this build does not do yet says so, and exits `1`.
- **`thelb director --bootstrap`** creates a cluster of one: it takes the node's local socket, settles the secret files, creates the cluster, its certificate authority and the director's own certificate - in memory - and runs until it is stopped, telling systemd it is ready (`Type=notify`) and alive (`WatchdogSec=`), and renewing its certificate. On `SIGTERM` it stops and leaves nothing behind. It does not probe, hold or announce the control VIP yet; a director without `--bootstrap`, which would join a cluster, stops.
- **The secret files**: at the first bootstrap, a missing `--secret-file` or `--backend-secret-file` is created with a new 256-bit secret - exclusively, mode `0600`, never overwritten: the one thing theLB writes. A secret is read only from a regular file that root owns and no one else can read, and is never logged: its type prints as `[redacted]` everywhere.
- **`thelb generate secret`** prints a new secret; **`thelb admin secret show --role director|backend`** prints the running director's.
- **The node fingerprint**: a hash of the machine's application-specific ID (from `/etc/machine-id`, which never leaves the machine), its SMBIOS UUID where it has one, and the MAC address of the interface the node joins through.
- **The certificate authority**: an ECDSA P-256 CA in memory, and director certificates for `spiffe://dmarts.app/thelb/<cluster>/director/<name>` with the node's addresses, valid for 24 hours and renewed at two thirds.
- **`thelb status`**: the cluster, its nodes with their fingerprints, and its certificates, as tables in the style of a SQL client's results, or as JSON (`--json`); one table by name; `--watch`. The tables still empty in this build - pending, pools, offers, VIPs, services, backends - are shown as such.
- **The local socket** `/run/thelb/thelb.sock`: root only, by its mode and by the kernel's answer for who is calling; one theLB per node; a socket left by a crash is replaced.
- **Logs**: one line per event - `<time> [<module>] OK|WARN|ERROR <what happened> key=value ...` - on standard error.
- **Building and checking**: `make build`, `make check` (format, vet for this platform and Linux, race-enabled tests, lint, `govulncheck`), `make e2e` (the built binary end to end, as root), and `make remote-check` and `make remote-e2e` for a Linux machine over ssh.
