---
title: Testing
lead: Four layers of tests, from plain unit tests to a whole cluster in network namespaces. The slow ones run on a development machine, not in CI.
description: How theLB is tested - make check, the kernel tests, the end-to-end test in network namespaces, the scale test - and where each runs.
section: Technical
order: 3
nav: Testing
---

## The layers

| Layer | Command | Needs | Runs |
|---|---|---|---|
| Format, vet, unit tests, lint, vulnerabilities | `make check` | Go, golangci-lint, govulncheck | anywhere; also in the release workflow |
| Kernel tests | part of `make e2e` | Linux, passwordless `sudo` | on a development machine |
| End to end | `make e2e` | Linux, passwordless `sudo`, `python3`, `curl` | on a development machine, before every release |
| Kubernetes, end to end | `make e2e-k8s` (and part of `make e2e`) | the same | on a development machine, before every release |
| Scale | `make scale N=1000` | the same | on a development machine, when performance matters |

## `make check`

```bash
make check
```

It runs, in order: `gofmt` (fails on any unformatted file), `go vet` for this platform and for Linux, `go test -race` on every package, `golangci-lint` (an exact version, so results do not change under you) and `govulncheck`. Every change must pass it.

The unit tests run on any system - macOS too. Linux-only code is behind build tags, so it is vetted for Linux even on a Mac.

## The kernel tests

Tests named `TestRoot...` need root and a real kernel: they program IPVS, nftables, addresses with lifetimes, and interfaces. Each one runs in a **network namespace of its own** (`internal/rootns`), so it cannot touch the machine's real network. `make e2e` runs them first.

## The end-to-end test

`test/e2e.sh` runs the real binary in a lab of network namespaces: a switch, four director nodes, backends, a router, and a client - all on `198.18.0.0/24`, a range reserved for testing. Each node has its own network and its own `/run`, as if it were a machine of its own.

It checks, among other things:

- the bootstrap, joining, approval, refusal, removal, a wrong secret, and a full stop;
- the leader crashing - the control VIP moves, and connections keep going;
- a planned stop handing leadership over;
- two directors surviving either one's stop and crash, a firewall that rejects their port, and a cut no gateway shows;
- three directors losing two, one after the other;
- the VIP lease: a killed leader's VIP runs out, and is never on two machines at once (a sampler watches for that the whole time);
- services by direct routing, port mapping and IP-in-IP - with strict reverse-path filtering on the backends, as Ubuntu and RHEL have by default;
- drift repair, preflight, `doctor`, `generate systemd` (run under systemd for real), metrics and `--debug`;
- IPv6, where the kernel has it;
- that nothing is left behind.

```bash
make e2e                          # on this Linux machine
make remote-e2e REMOTE=<host>     # on another, over ssh: syncs the tree, runs it there
```

`make e2e` builds its own binary, `bin/e2e/thelb`, so it never replaces a `bin/thelb` someone may be running. If it fails, it keeps its files in `/tmp/thelb-e2e.*` and prints where.

## The Kubernetes test

`test/e2e-k8s.sh` tests the Kubernetes provider the same way, in a lab of its own: two directors, three nodes standing in for a cluster's nodes, a client, and **`test/kubeapi`**, a stand-in Kubernetes API server. It serves the little of the API theLB uses - lists and watches of Services, Nodes and EndpointSlices, a Service's status, access reviews, events - over TLS, from a state file the test edits; it records what theLB writes.

Each node does what kube-proxy or Cilium does for a Service: it takes packets for the Service's VIP, hands them to its "pods" (a web server answering with the node's name), and answers on the Service's health port - `200`, or `503` when the test says so.

It checks the directors' startup checks, which never stop a director (a director on a node that is not labelled, a permission missing: each says why, runs on, and serves Kubernetes by itself once fixed), a Service getting its VIP and status, the nodes with ready pods serving - weighted by their pods - and the others not, a node going unhealthy and coming back, addresses asked for, shared VIPs and a port clash, another provider's Service left alone, status on a follower, the leader killed, and a Service deleted.

```bash
make e2e-k8s                          # on this Linux machine
make remote-e2e-k8s REMOTE=<host>     # on another
```

The unit tests check the rest: `internal/kube` against a fake API server; the director, in-process, against another; and `thelb generate k8s`'s output against the files the documentation shows (`docs/examples/`), so the two never differ.

:::note Why not in CI?
The end-to-end test takes several minutes of a whole machine, with root. Running it on every push in GitHub Actions would cost a lot and slow everything down. Instead, it runs on a development machine **before each release** - see [Releasing](tech-release.md).
:::

## The fuzz tests

`make fuzz` (`FUZZTIME=30s` each, by default) runs Go's fuzzer on every parser of what theLB takes from outside: the join's checks and certificate requests, secret files, health checks, `--offer`, the wire's framing, the Kubernetes reader (kubeconfig YAML, objects, watch streams, error bodies), Raft entries and snapshots, IPVS's connection table, the backend's orders, and what `status` and `safe` make fit for a terminal. Each states what must hold for anything it takes - not just that it does not crash. `make check` runs their seeds; a failing input is kept in the package's `testdata/fuzz`, and runs with every test after.

## The blackbox test

`test/blackbox` runs the binary as a user would, knowing nothing of its code. It finds every command and flag in the binary's own help, and runs each with about 80 hostile values - empty, huge, negative, out of range, other addresses' forms, paths out of the tree, shell and format strings, control characters - as arguments and as flags. Whatever it is given, theLB must end within 10 seconds, exit 0, 1 or 2, never print a panic, a stack trace or a control character, and name the help page on a usage error. As a user, everything but the help must say it runs only as root. As root (in the security lab), each flag's values are tried on a command line that holds every flag it requires, so a hostile value reaches its own check.

## The security test

`make e2e-security` (`test/e2e-security.sh`, as root on Linux): a cluster - two directors, a backend serving a service, a client - and an attacker (`test/attack`), each in a network namespace. The attacker speaks theLB's own protocols with theLB's own packages:

- **the control port**: thousands of connections that connect and close, or never speak; garbage, HTTP, TLS 1.2, wrong or 500 protocols, certificates no cluster signed and another cluster's director certificate; handshakes dripped a byte at a time;
- **the join**: wrong, missing and relayed proofs, the other role's secret, future versions, messages over the size limit; with the secret, every lie a claim can tell (name, role, address, MAC, fingerprint, hostname, version, a request for another name or an RSA key); floods of wrong secrets, and of new names;
- **a member gone bad**: a backend joins, is approved, then tries what only directors may - changes, approvals, removals, Raft, the directors' own listener - sends heartbeats over the limits and hostile offers, renews its certificate as another node; once removed, its certificate is refused;
- **a cluster that is not one**: an impostor with a theLB certificate chain of its own, and a proof it cannot make - a backend sent there stops, never telling it its claim or asking it to sign anything;
- **the metrics endpoint** (methods, paths, a megabyte of headers, slowloris, 500 connections) and **the local socket** (another user);
- **hostile files at start** (secret files and kubeconfigs that are links, FIFOs, directories, open to others, huge, empty, not what they claim; kubeconfigs that run a command, talk in clear, skip the certificate check, name theLB's secret as their token);
- **a Kubernetes API server that lies**: errors with escapes, broken JSON, redirects to a trap, answers dripped a byte a second, an endless list, an endless watch event - the director under a 600 MB memory limit.

After each, the cluster must be unharmed: both directors up, d1 still leading, the client served through the VIP, d1's file descriptors back to what they were, its memory bounded. It ends with the blackbox test as root, and a failover.

## The scale test

`make scale N=1000` builds three directors, three backends and N services, each on a VIP of its own, then measures: the time to create the services and bring their VIPs up, the leader's CPU and memory at rest, how long `thelb status` takes, and a failover. `PROFILE=1` builds with profiling and prints where the leader spends its CPU.

## The lab

`make lab` is the same kind of lab, kept up for you to try things by hand - see [Try it on one machine](lab.md). It uses its own binary, `bin/lab/thelb`.

## The scripts' tests

The release workflow's Python scripts - the Slack notification and the SBOM writer - and the docs builder have their own tests, standard library only:

```bash
python3 -B -m unittest discover -s .github/scripts -p 'test_*.py'
python3 -B docs/test_build.py
```
