package cli

const helpText = `theLB - The Load Balancer: L4 load balancing for Linux, in one binary.

Usage:
  thelb <command> [flags]

Commands:
  director   run this node as a director
  backend    run this node as a backend
  status     show the cluster, from the theLB running on this node
  doctor     check this node, and what theLB would change     (not in this build yet)
  generate   print a file's content, and write nothing: secret
  admin      change the cluster, on a director: approve, refuse, remove,
             pool, service, assign, unassign, secret show

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
  thelb backend --join <control VIP:port> --secret-file <file> --offer <offer> ... [flags]

Runs this node as a backend. It enrols on the control VIP with the backends'
secret - the file a bootstrap created, copied from a director - and waits for a
director's approval. Then it sends its heartbeat every 2 seconds, checks its
applications, and holds the VIPs of the services its offers are assigned to on
its own interface, thelb0, without answering ARP for them. It never chooses a
service: a director assigns its offers (thelb admin assign).

Flags:
  --join <address:port>   the control VIP and port
  --secret-file <file>    the backends' secret (absolute path)
  --offer <offer>         what this node serves, as often as needed: tcp/8080,
                          udp/53 or sctp/2905, with ,check=tcp (TCP's default),
                          ,check=http:/healthz, or ,check=none (UDP's and
                          SCTP's default)
  --name <name>           this node's name (default: its hostname)
  --interface <name>      the control VIP's interface (default: the one in its network)

Where a service's port differs from its offer's - the service on 80, the
application on 8080 - the backend maps it, in theLB's own nftables table,
inet thelb.
`,
	"status": `Usage:
  thelb status [--json] [--watch] [view or table ...]

Shows the cluster, as the theLB running on this node sees it. Without a name, a
summary: the cluster, and how many directors, backends, services and VIPs there
are, and how many are up. Name a view for one part of it:

  directors   the directors, those waiting for approval, their certificates
  backends    the backends, those waiting, their offers, and who serves what -
              each backend's checks, weight, and what IPVS sent it
  vips        the VIPs and who holds them, the services, the pools
  all         everything

or one table: cluster, nodes, certificates, pending, pools, offers, services.
--json prints the same as JSON - everything, without a name; --watch shows it
again every 2 seconds. On a backend, status shows that backend alone.
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
  thelb admin pool add <name> --vip <first address/prefix> --vippool-count <n> [--interface <name>] [--default]
  thelb admin pool remove <name>
  thelb admin service add <name> --proto tcp|udp|sctp --port <port> [--pool <name>] [--vip <address>] [--scheduler wrr]
  thelb admin service remove <name>
  thelb admin assign <service> <node>:<proto>/<port> ... [--weight <node>=<n>,...]
  thelb admin unassign <service> <node>
  thelb admin secret show --role director|backend

Changes the cluster, on a director; another director's change goes through the
leader, under this director's identity.

  approve         approves pending nodes. With --match, every pending node whose
                  whole name matches (RE2 syntax: 'web-[0-9]+') is shown, and
                  approved after a "y" - or at once with --yes; without a
                  terminal and without --yes, nothing is approved
  refuse          refuses a pending node, which is told so and stops
  remove          removes a node: its certificate is revoked, a director leaves
                  Raft, a backend's assignments end; it waits for approval if it
                  asks again
  pool add        registers addresses for service VIPs: --vip 10.10.1.20/24
                  --vippool-count 10 is 10.10.1.20 to 10.10.1.29, on the
                  director's interface in 10.10.1.0/24
  service add     creates a service: the lowest free address of its pool (or
                  --vip), probed on the network first, a protocol and a port;
                  --scheduler wrr (the default), rr, wlc, lc, sh or mh
  assign          assigns backends' offers to a service, weight 1 unless
                  --weight says otherwise; the offer's protocol must be the
                  service's, and the director must reach the backend directly;
                  where its port differs, the backend maps the service's to it
  unassign        takes an offer back
  secret show     prints a role's secret, for a node whose secret file is out of
                  date - the one way to see a secret

secret rotate and retire come later.
`,
}
