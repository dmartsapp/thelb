package cli

const helpText = `theLB - The Load Balancer: L4 load balancing for Linux, in one binary.

Usage:
  thelb <command> [flags]

Commands:
  director   run this node as a director
  backend    run this node as a backend                       (not in this build yet)
  status     show the cluster, from the theLB running on this node
  doctor     check this node, and what theLB would change     (not in this build yet)
  generate   print a file's content, and write nothing: secret
  admin      change the cluster, on a director: secret show

Every command runs only as root (effective user ID 0); this help and --version
run for anyone.

  thelb <command> --help   a command's flags
  thelb --version          the version, the commit, Go and the platform

The design: https://github.com/dmartsapp/thelb
`

var commandHelps = map[string]string{
	"director": `Usage:
  thelb director --vip <address> --port <port> --secret-file <file> --backend-secret-file <file> [--bootstrap] [flags]

Runs this node as a director. With --bootstrap, and the secret files missing,
it creates the cluster's two secrets and writes them to those files - the one
thing theLB writes to disk: only then, readable by root only, never overwritten.
Everything else is in memory: the cluster, its certificate authority, and the
director's own certificate.

This build creates a cluster of one, and sends nothing on the network: it does
not check, hold or announce the control VIP yet, and a director without
--bootstrap, which would join a cluster, stops.

Flags:
  --vip <address>                the control VIP: an address, no prefix
  --port <port>                  the control port
  --secret-file <file>           the directors' secret (absolute path)
  --backend-secret-file <file>   the backends' secret (absolute path)
  --bootstrap                    may create the cluster
  --cluster <name>               the cluster's name (default thelb)
  --name <name>                  this node's name (default: its hostname)
  --interface <name>             the control VIP's interface (default: the one in its network)
`,
	"backend": `Usage:
  thelb backend --join <control VIP:port> --secret-file <file> --offer <protocol>/<port> [flags]

Runs this node as a backend. Not in this build yet.
`,
	"status": `Usage:
  thelb status [--json] [--watch] [table ...]

Shows the cluster, as the theLB running on this node sees it: tables in the
style of a SQL client's results, or one JSON document with --json. Name tables
to show only those: cluster, nodes, certificates, pending, pools, offers, vips,
services, backends. --watch shows them again every 2 seconds.
`,
	"doctor": `Usage:
  thelb doctor [--json]

Checks this node, and says what theLB would change on it. Not in this build yet.
`,
	"generate": `Usage:
  thelb generate secret

Prints a file's content, and writes nothing.

  secret    a new cluster secret, for a user who wants to create a secret file
            before the first bootstrap: thelb generate secret > file

systemd, sysctl, modules, k8s and config come later.
`,
	"admin": `Usage:
  thelb admin secret show --role director|backend

Changes the cluster from outside, on a director.

  secret show   prints a role's secret, for a node whose secret file is out of
                date - the one way to see a secret

approve, refuse, remove, pool, service, assign, unassign, and secret rotate and
retire come later.
`,
}
