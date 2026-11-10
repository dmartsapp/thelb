package cli

const helpText = `theLB - The Load Balancer: L4 load balancing for Linux, in one binary.

Usage:
  thelb <command> [flags]

Commands:
  director   run this node as a director
  backend    run this node as a backend
  status     show the cluster, from the theLB running on this node
  doctor     check this node for theLB, and say what to do
  offer      on a backend: offer an application to the cluster, or withdraw it
  generate   print a file's content, and write nothing: secret, commands
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
  --vip <address or name>        the control VIP: an address (no prefix), or a name,
                                 resolved at start to its one address in this
                                 node's networks
  --port <port>                  the control port
  --secret-file <file>           the directors' secret (absolute path)
  --backend-secret-file <file>   the backends' secret (absolute path)
  --bootstrap                    may create the cluster
  --cluster <name>               the cluster's name, when this director creates it (default thelb)
  --name <name>                  this node's name (default: its hostname)
  --interface <name>             the control VIP's interface (default: the one in its network)
`,
	"backend": `Usage:
  thelb backend --join <control VIP:port> --secret-file <file> [--offer <offer> ...] [flags]

Runs this node as a backend. It enrols on the control VIP with the backends'
secret - the file a bootstrap created, copied from a director - and waits for a
director's approval. Then it offers its applications - each checked by the
leader first - sends its heartbeat every 2 seconds, checks its applications,
and holds the VIPs of the services its offers are assigned to on its own
interface, thelb0, without answering ARP for them. It never chooses a service:
a director assigns its offers (thelb admin assign).

What it offers: its --offer flags - each offered once its own check of it
passes - and what is offered while it runs with thelb offer add. The cluster
keeps its offers: a backend that starts again takes them up again, and offers
them to a cluster created again. An offer the leader refuses is offered again
when its check passes again, or 10 s later.

Flags:
  --join <address:port>   the control VIP - an address or a name - and port
  --secret-file <file>    the backends' secret (absolute path)
  --offer <offer>         an application this node serves, as often as needed:
                          [name=]tcp/8080, udp/53 or sctp/2905 - named tcp-8080
                          without a name - with ,check=tcp (TCP's default),
                          ,check=http:/healthz, or ,check=none (UDP's and
                          SCTP's default)
  --name <name>           this node's name (default: its hostname)
  --interface <name>      the control VIP's interface (default: the one in its network)

Where a service's port differs from its offer's - the service on 80, the
application on 8080 - the backend maps it, in theLB's own nftables table,
inet thelb.

On a director's own host, a backend runs beside the director: start it after
the director, with its own --name, and offer the service's port itself.
`,
	"offer": `Usage:
  thelb offer add <name> <proto>/<port>[,check=tcp|none|http:<path>]
  thelb offer remove <name>

On a backend that is a member of a cluster: offers one of its applications to
the cluster, under a name of the backend's choosing, or withdraws it.

  add      the leader checks the application - the offer's check, tcp by
           default for TCP - and records the offer, unassigned, once it
           answers; a director then assigns it to a service:
           thelb admin assign <service> <node>:<name>. The same name again
           with another port or check replaces the offer.
  remove   withdraws it; the services it is assigned to lose it

The backend checks what it offers every 2 seconds: while its check fails, the
offer serves no service, and it serves again when the check passes.
`,
	"status": `Usage:
  thelb status [--json] [--watch] [--role director|backend] [view or table ...]

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
again every 2 seconds. On a backend, status shows that backend alone; on a
host that runs both, status asks the director, and --role backend the backend.
`,
	"doctor": `Usage:
  thelb doctor [--role director|backend] [--port <control port>] [--json]

Checks this node for theLB, and says what to do; it changes nothing. For the
roles running here, the one --role names, or both:

  the kernel, its modules and IPv6; theLB's capabilities; the kernel parameters
  theLB sets while it runs, and what the system sets from boot; how theLB runs
  here - under systemd or by hand; the firewall (ufw, firewalld, nftables), with
  the commands that open what theLB needs; SELinux; software in the way
  (keepalived, kube-proxy, Cilium, kube-router, other IPVS services); on a
  director, what listens on every address - it answers on every VIP; the
  platform - clouds where a VIP cannot move, hypervisors that need a setting;
  the clock; and the cluster this node's director sees.

--port is the control port, for the firewall's commands, when no director runs
here to say it. Exit status 1 when theLB could not run, or not do part of its
work.
`,
	"generate": `Usage:
  thelb generate secret
  thelb generate commands
  thelb generate systemd director|backend <the flags it runs with>
  thelb generate sysctl director|backend
  thelb generate modules director|backend

Prints a file's content, and writes nothing.

  secret      a new cluster secret, for a user who wants to create a secret file
              before the first bootstrap: thelb generate secret > file
  commands    on a director: the thelb admin commands that create the cluster's
              pools, services and assignments again, as they are - a script to
              keep, and run after every director has stopped; theLB never
              reads it: thelb generate commands > /root/thelb-restore.sh
  systemd     a unit that runs thelb director or backend with the flags given -
              checked as the role checks them - and this binary: Type=notify,
              the watchdog, Restart=always, its capabilities bounded and the
              system read-only but for what it writes:
              thelb generate systemd director --vip 10.0.0.10 --port 7443 ... |
                sudo tee /etc/systemd/system/thelb-director.service
  sysctl      a sysctl.d file with the kernel parameters theLB sets while it
              runs, for them from boot
  modules     a modules-load.d file with the modules it uses

k8s comes later.
`,
	"admin": `Usage:
  thelb admin approve <name> ...
  thelb admin approve --match '<regular expression>' [--yes]
  thelb admin refuse <name>
  thelb admin remove <name>
  thelb admin pool add <name> --vip <first address/prefix> --vippool-count <n> [--interface <name>] [--default]
  thelb admin pool remove <name>
  thelb admin service add <name> --proto tcp|udp|sctp --port <port> [--pool <name>] [--vip <address>] [--scheduler wrr] [--method direct|tunnel]
  thelb admin service remove <name>
  thelb admin assign <service> <node>:<offer> ... [--weight <node>=<n>,...]
  thelb admin unassign <service> <node>
  thelb admin lease <seconds>
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
                  --scheduler wrr (the default), rr, wlc, lc, sh or mh;
                  --method direct (the default: backends on the director's
                  network) or tunnel (IP-in-IP: backends anywhere routed, IPv4)
  assign          assigns backends' offers to a service - each by the name its
                  backend gave it (web-1:nginx), or its protocol and port
                  (web-1:tcp/8080); thelb status offers lists them - weight 1
                  unless --weight says otherwise; the offer's protocol must be
                  the service's, and the director must reach the backend
                  directly; where its port differs, the backend maps the
                  service's to it. The service's VIP comes up once a backend
                  serves it, after IPVS has it, and goes when none does
  unassign        takes an offer back
  lease           how long the kernel keeps the control VIP after the leader's
                  last renewal (6 s unless set; 3 s to 1 h): a leader killed, or
                  hung, loses the VIP within it, and a director taking over
                  waits it out. Kept in the cluster's state; thelb status
                  cluster shows it
  secret show     prints a role's secret, for a node whose secret file is out of
                  date - the one way to see a secret

secret rotate and retire come later.
`,
}
