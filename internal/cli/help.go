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
  generate   print a file's content, and write nothing: secret, commands,
             systemd, sysctl, modules, k8s
  admin      change the cluster, on a director: approve, refuse, remove,
             pool, service, assign, unassign, lease, secret show

Every command runs only as root (effective user ID 0); this help and --version
run for anyone.

  thelb <command> --help        a command's flags; or thelb help <command>
  thelb generate k8s --help     the Kubernetes manifests, scripts and Services
  thelb --version               the version, the commit, Go and the platform

The documentation: https://dmartsapp.github.io/thelb/
`

// subcommandHelps are the help of a subcommand with a page of its own, by its
// words: thelb generate k8s --help, thelb help generate k8s.
var subcommandHelps = map[string]string{
	"generate k8s": `Usage:
  thelb generate k8s
  thelb generate k8s kubeconfig
  thelb generate k8s director <the flags thelb director runs with>
  thelb generate k8s node <name>
  thelb generate k8s service <name> [--namespace <ns>] [--port <port> ...] [--selector <key=value>]
                     [--policy Local|Cluster] [--pool <pool>] [--ip <address>] [--shared-vip <key>]
  ... [--from-kubeconfig <file> [--from-context <name>] [--yes]]   on any of them

Prints what theLB needs in a Kubernetes cluster, and writes nothing - to disk or
to the cluster: apply it with kubectl, or run it. A director serves the
cluster's Services of class dmarts.app/thelb with --kubeconfig (thelb director
--help), and sends their traffic to the nodes that run their pods. Those nodes
need nothing: no label, and no thelb backend - the directors read them from the
API, and check them at the Service's health port. Only a node that runs a
director is prepared, with node.

  (alone)      the directors' identity and permissions: the namespace thelb, the
               ServiceAccount thelb-director, a ClusterRole - reading Services,
               Nodes and EndpointSlices, writing a Service's status and events,
               never a Service itself - its binding, and the Secret
               thelb-director-token, the token of directors outside the cluster:
               thelb generate k8s > thelb-k8s.yaml
               kubectl apply -f thelb-k8s.yaml
  kubeconfig   a script to run where kubectl reaches the cluster, once the
               identity is applied: it prints the kubeconfig of directors outside
               the cluster - kubectl's API server, and thelb-director's token:
               thelb generate k8s kubeconfig > thelb-kubeconfig.sh
               sh thelb-kubeconfig.sh > thelb.kubeconfig
               then on each director, readable by root only, and with
               --kubeconfig /etc/thelb/kubeconfig in its flags:
               sudo install -D -m 600 thelb.kubeconfig /etc/thelb/kubeconfig
  director     the identity, and a DaemonSet that runs a director on each node
               labelled dmarts.app/thelb-director, with the flags given - checked
               as thelb director checks them, and these too:
                 --kubeconfig in-cluster    the pod's own ServiceAccount
                 --metrics <address:port>   the pod's readiness probe asks /readyz
                 no --name                  each director is named after its node
                 two secret files of different names: the two keys of the Secret
                 thelb-secrets, which you create from a director's files
                 --bootstrap only with --bootstrap-wait 31s or more
               thelb generate k8s director --vip 10.0.0.10 --port 7443 \
                 --secret-file /etc/thelb/director.secret \
                 --backend-secret-file /etc/thelb/backend.secret \
                 --kubeconfig in-cluster --metrics 127.0.0.1:9180 > thelb-director.yaml
  node         a script to run where kubectl reaches the cluster, before a
               director runs on the node - in the DaemonSet, or under systemd:
               a worker tainted dmarts.app/thelb-director and drained, so that
               nothing else runs there (a control-plane node is left as it is);
               with kube-proxy, kube-proxy's DaemonSet kept off the node, which
               is then rebooted; the label dmarts.app/thelb-director=true; with
               Cilium, theLB's Services touched, so that Cilium on the node
               leaves them to theLB. Its last lines say how to give it back:
               thelb generate k8s node cp1 > thelb-node.sh
               sh thelb-node.sh
  service      a Service of type LoadBalancer and class dmarts.app/thelb, with
               the annotation that keeps Cilium off directors' nodes:
               thelb generate k8s service web > web-service.yaml
               kubectl apply -f web-service.yaml

Reading the cluster, to fit what it prints to it - any form:
  --from-kubeconfig <file>   read the cluster with this kubeconfig - yours, an
                             administrator's. generate first says which context,
                             API server and user, and what it reads, and asks; it
                             only reads. Without it, generate reads nothing, and
                             its scripts find out what they need when they run
  --from-context <name>      the kubeconfig's context (default: its current one)
  --yes                      read it without asking - for scripts
What each form takes from the cluster:
  (alone)      its version, which must be 1.35 or later
  kubeconfig   the kubeconfig itself, not a script: the Secret thelb-director-token's
               token and CA - checked: a director reaches the API server with them,
               and has every permission it needs
  director     with kube-proxy, the API server's address for the pods, which reach
               no Service; and which nodes it runs directors on
  node         just the steps the node needs: a control-plane node or a worker,
               with Cilium, with kube-proxy
  service      the selector and ports of the Deployment or StatefulSet of its name -
               unless --selector and --port say them; a warning when no pod matches;
               a refusal when a Service of that name has another load balancer class
A user who signs in with a plugin (exec) - as on most clouds - cannot be read:
leave out --from-kubeconfig, and run the scripts where kubectl works.

Flags of service:
  --namespace <ns>         its namespace (default default)
  --port <port>[/tcp|udp|sctp][:<pods' port>]
                           a port, as often as needed - 80, 53/udp, 80:8080
                           (port 80 to the pods' 8080), 443/tcp:8443 (default 80)
  --selector <key=value>   the pods it sends to (default app=<name>)
  --policy Local|Cluster   its externalTrafficPolicy (default Local: only nodes
                           with a ready pod get traffic, and the client's
                           address reaches the pod; Cluster: every ready node,
                           and the client's address is lost)
  --pool <pool>            its VIP from this theLB pool, not the default - the
                           annotation dmarts.app/thelb-pool
  --ip <address>           this address of the pool - dmarts.app/thelb-ip
  --shared-vip <key>       one VIP for the Services with this key, on different
                           ports - dmarts.app/thelb-shared-vip

The guide: https://dmartsapp.github.io/thelb/docs/kubernetes.html
`,
}

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
  --metrics <address:port>       serve Prometheus metrics there, at /metrics - an address only your
                                 monitoring reaches; not 0.0.0.0, which would include the VIPs
  --debug                        DEBUG lines too: while it leads, each connection forwarded -
                                 the client, the service, the backend, when
  --kubeconfig <file|in-cluster> serve Kubernetes Services of class dmarts.app/thelb, with
                                 this credential: a kubeconfig file (absolute path), or
                                 in-cluster for a pod's ServiceAccount - see thelb generate k8s
  --bootstrap-wait <duration>    with --bootstrap: look for the cluster this long before creating
                                 one (default 5s); 31s or more for a pod, restarted after 10 s
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
  --metrics <address:port>
                          serve Prometheus metrics there, at /metrics; not 0.0.0.0

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
  thelb doctor [--role director|backend] [--vip <control VIP>] [--port <control port>] [--json]

Checks this node for theLB, and says what to do; it changes nothing. For the
roles running here, the one --role names, or both:

  the kernel, its modules and IPv6; theLB's capabilities; the kernel parameters
  theLB sets while it runs, and what the system sets from boot; how theLB runs
  here - under systemd or by hand; the firewall (ufw, firewalld, nftables), with
  the commands that open what theLB needs; SELinux; software in the way
  (keepalived, kube-proxy, Cilium, kube-router, other IPVS services); on a
  director, what listens on every address - it answers on every VIP; the
  platform - clouds where a VIP cannot move, hypervisors that need a setting;
  the clock; and the cluster this node's director sees. Then the link: whether
  the gateway and the other directors answer ARP probes - a takeover reads a
  director's silence as its machine down - whether connection sync runs, and
  whether a follower's cache points the control VIP at the leader. Then the
  plan: what the role adds, sets and starts here, and what it leaves as it is.

--vip and --port are the control VIP and port, for the plan and the firewall's
commands, when no director runs here to say them. Exit status 1 when theLB could not run, or not do part of its
work.
`,
	"generate": `Usage:
  thelb generate secret
  thelb generate commands
  thelb generate systemd director|backend <the flags it runs with>
  thelb generate sysctl director|backend
  thelb generate modules director|backend
  thelb generate k8s
  thelb generate k8s kubeconfig
  thelb generate k8s director <the flags it runs with>
  thelb generate k8s node <name>
  thelb generate k8s service <name> [flags]

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
  k8s         what theLB needs in a Kubernetes cluster, to apply with kubectl:
              the directors' identity and permissions; with kubeconfig, a script
              that prints the kubeconfig of directors outside the cluster; with
              director, a DaemonSet that runs directors on labelled nodes; with
              node, a script that makes a node a director's node; with service,
              a Service of theLB's class. Their flags and examples:
              thelb generate k8s --help
`,
	"admin": `Usage:
  thelb admin approve <name> ...
  thelb admin approve --match '<regular expression>' [--yes]
  thelb admin refuse <name>
  thelb admin remove <name>
  thelb admin pool add <name> --vip <first address/prefix> --vippool-count <n> [--interface <name>] [--default]
  thelb admin pool remove <name>
  thelb admin service add <name> --proto tcp|udp|sctp --port <port> [--pool <name>] [--vip <address>]
                          [--scheduler wrr|rr|wlc|lc|sh|mh] [--method direct|tunnel]
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
                  director's interface in 10.10.1.0/24 - or --interface; with
                  --default, the pool a service, or a Kubernetes Service, takes
                  its VIP from when it names none: one for each address family
  pool remove     removes a pool that no service or Kubernetes Service uses
  service add     creates a service: the lowest free address of its pool -
                  --pool, or the default pool - or --vip, probed on the network
                  first, a protocol and a port;
                  --scheduler wrr (the default), rr, wlc, lc, sh or mh;
                  --method direct (the default: backends on the director's
                  network) or tunnel (IP-in-IP: backends anywhere routed, IPv4)
  service remove  removes a service: its VIP, its IPVS service, its assignments
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
