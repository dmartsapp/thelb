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
  admin      change the cluster, on a director: approve, refuse, remove, secret show

Every command runs only as root (effective user ID 0); this help and --version
run for anyone.

  thelb <command> --help   a command's flags
  thelb --version          the version, the commit, Go and the platform

The design: https://github.com/dmartsapp/thelb
`

var commandHelps = map[string]string{
	"director": `Usage:
  thelb director --vip <address> --port <port> --secret-file <file> --backend-secret-file <file> [--bootstrap] [flags]

Runs this node as a director. It looks for the cluster on the control VIP and
port: if one answers, it joins it with the directors' secret, and waits for a
director's approval - unless the cluster approved this node before. With
--bootstrap, when nothing answers for longer than a failover takes, it creates
the cluster, and - if the secret files are missing - the cluster's two secrets,
written to those files: the one thing theLB writes to disk, only then, readable
by root only, never overwritten. Everything else is in memory: the cluster's
state, its certificate authority, and every key.

The leader holds the control VIP: it adds it to the interface, announces it,
and gives it up when it no longer leads. Stopped with SIGTERM, a leader first
hands leadership to another director.

Flags:
  --vip <address>                the control VIP: an address, no prefix
  --port <port>                  the control port
  --secret-file <file>           the directors' secret (absolute path)
  --backend-secret-file <file>   the backends' secret (absolute path)
  --bootstrap                    may create the cluster
  --cluster <name>               the cluster's name, when this director creates it (default thelb)
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
  thelb admin approve <name> ...
  thelb admin approve --match '<regular expression>' [--yes]
  thelb admin refuse <name>
  thelb admin remove <name>
  thelb admin secret show --role director|backend

Changes the cluster, on a director; another director's change goes through the
leader, under this director's identity.

  approve         approves pending nodes. With --match, every pending node whose
                  whole name matches (RE2 syntax: 'web-[0-9]+') is shown, and
                  approved after a "y" - or at once with --yes; without a
                  terminal and without --yes, nothing is approved
  refuse          refuses a pending node, which is told so and stops
  remove          removes a node: its certificate is revoked, and a director
                  leaves Raft; it waits for approval if it asks again
  secret show     prints a role's secret, for a node whose secret file is out of
                  date - the one way to see a secret

pool, service, assign, unassign, and secret rotate and retire come later.
`,
}
