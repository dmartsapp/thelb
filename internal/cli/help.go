package cli

import (
	"sort"
	"strings"
)

// The help is a tree of pages, in the layout most command-line tools use:
// thelb --help lists the commands; thelb <command> --help a command's
// subcommands, or its options; thelb <command> <subcommand> --help one
// subcommand's usage, options and examples. thelb help <words> shows the same.

const docsURL = "https://dmartsapp.github.io/thelb/"

const helpText = `Usage: thelb <command> [<subcommand>] [options]

theLB - The Load Balancer: L4 load balancing for Linux, in one binary.

Commands:
  director    Run this node as a director
  backend     Run this node as a backend
  status      Show the cluster, as this node sees it
  doctor      Check this node for theLB, and say what to fix
  offer       Offer an application to the cluster, or withdraw it (on a backend)
  generate    Print a file theLB needs - it writes nothing
  admin       Change the cluster (on a director)
  help        Show the help of a command

Options:
  -h, --help       Show this help
  -v, --version    Show the version, the commit, Go and the platform

Run 'thelb <command> --help' for more about a command.
Every command but help and --version runs as root.

Documentation: ` + docsURL + `
`

// helpPages are the pages, by their command words.
var helpPages = map[string]string{
	"director": `Usage: thelb director --vip <address> --port <port> --secret-file <file>
                      --backend-secret-file <file> [options]

Run this node as a director. It looks for the cluster on the control VIP and
port, and joins it - then waits for a director's approval, unless the cluster
approved this node before. With --bootstrap, it creates the cluster when none
answers. The leader holds the control VIP; on SIGTERM it hands over first.

The first bootstrap writes the two secret files if they are missing (root
only, never overwritten). Everything else stays in memory.

Options:
  --vip <address|name>          The control VIP: an address, or a name resolved
                                at start (required)
  --port <port>                 The control port (required)
  --secret-file <file>          The directors' secret, absolute path (required)
  --backend-secret-file <file>  The backends' secret, absolute path (required)
  --bootstrap                   May create the cluster
  --bootstrap-wait <duration>   With --bootstrap: how long to look for the
                                cluster first (default 5s; 31s or more in a pod)
  --cluster <name>              The cluster's name, when it creates it
                                (default thelb)
  --name <name>                 This node's name (default: its hostname)
  --interface <name>            The control VIP's interface (default: the one
                                in its network)
  --metrics <address:port>      Serve Prometheus metrics at /metrics - an
                                address of this machine, not 0.0.0.0
  --kubeconfig <file|in-cluster>
                                Serve Kubernetes Services of class
                                dmarts.app/thelb with this credential
  --debug                       Log each connection forwarded, too

Examples:
  # The first director: creates the cluster and the secret files
  thelb director --vip 10.0.0.10 --port 7443 --bootstrap \
    --secret-file /etc/thelb/director.secret \
    --backend-secret-file /etc/thelb/backend.secret

  # Another director, with copies of the two files
  thelb director --vip 10.0.0.10 --port 7443 \
    --secret-file /etc/thelb/director.secret \
    --backend-secret-file /etc/thelb/backend.secret

See also: thelb generate systemd, thelb generate k8s
Documentation: ` + docsURL + `docs/directors.html
`,

	"backend": `Usage: thelb backend --join <address:port> --secret-file <file> [options]

Run this node as a backend. It joins on the control VIP with the backends'
secret, and waits for a director's approval. Then it offers its applications,
checks them, sends a heartbeat every 2 seconds, and holds the VIPs of the
services it serves on its own interface, thelb0. A director assigns its offers
to services; a backend never chooses one.

Options:
  --join <address:port>     The control VIP - an address or a name - and port
                            (required)
  --secret-file <file>      The backends' secret, absolute path (required)
  --offer <offer>           An application to offer, as often as needed:
                            [<name>=]<tcp|udp|sctp>/<port>[,check=<check>]
                            check: tcp (TCP's default), http:<path>, or none
                            (UDP's and SCTP's default)
  --name <name>             This node's name (default: its hostname)
  --interface <name>        The control VIP's interface (default: the one in
                            its network)
  --metrics <address:port>  Serve Prometheus metrics at /metrics

Examples:
  thelb backend --join 10.0.0.10:7443 --secret-file /etc/thelb/backend.secret \
    --offer nginx=tcp/8080,check=http:/healthz --offer udp/53

Where a service's port differs from its offer's, the backend maps it in its
own nftables table, inet thelb. On a director's host, start the backend after
the director, with a name of its own.

See also: thelb offer, thelb admin assign
Documentation: ` + docsURL + `docs/backends.html
`,

	"status": `Usage: thelb status [options] [<view>|<table> ...]

Show the cluster, as the theLB running on this node sees it. Without a view, a
summary. On a backend, only that backend.

Views:
  directors    The directors, those waiting for approval, their certificates
  backends     The backends, those waiting, their offers, who serves what
  vips         The VIPs and who holds them, the services, the pools
  all          Everything

Tables:
  cluster, nodes, certificates, pending, pools, offers, services

Options:
  --json                       Print it as JSON (everything, without a view)
  --watch                      Show it again every 2 seconds
  --role director|backend      Ask that role, where a node runs both
                               (default: the director)

Examples:
  thelb status
  thelb status backends
  thelb status --watch services offers

Documentation: ` + docsURL + `docs/status.html
`,

	"doctor": `Usage: thelb doctor [options]

Check this node for theLB, and say what to fix. It changes nothing.

It checks the kernel, modules and IPv6; theLB's capabilities and kernel
parameters; systemd; the firewall, with the commands that open what theLB
needs; SELinux; software in the way (keepalived, kube-proxy, Cilium); what
listens on every address; the platform; the clock; the cluster as this node
sees it; and the link to the gateway and the other directors. Then it says
what theLB would add, set and start here.

Options:
  --role director|backend   Check for this role (default: the roles running
                            here, or both)
  --vip <address>           The control VIP, when no director runs here
  --port <port>             The control port, when no director runs here
  --json                    Print the findings as JSON

The exit status is 1 when theLB could not run here, or not do all its work.

Examples:
  thelb doctor
  thelb doctor --role director --vip 10.0.0.10 --port 7443

Documentation: ` + docsURL + `docs/troubleshooting.html
`,

	"offer": `Usage: thelb offer <subcommand> [arguments]

Offer one of this backend's applications to the cluster, or withdraw it. Runs
on a backend that is a member of a cluster.

Subcommands:
  add       Offer an application
  remove    Withdraw an offer

Run 'thelb offer <subcommand> --help' for details.
`,

	"offer add": `Usage: thelb offer add <name> <tcp|udp|sctp>/<port>[,check=<check>]

Offer an application under a name of your choice. The leader checks it first,
then records the offer, unassigned, for a director to assign to a service. The
same name again, with another port or check, replaces the offer.

The backend checks what it offers every 2 seconds: while the check fails, the
offer serves no service.

Checks:
  tcp              A TCP connection (TCP's default)
  http:<path>      An HTTP GET of the path: 2xx or 3xx passes
  none             No check (UDP's and SCTP's default)

Examples:
  thelb offer add shop tcp/8080,check=http:/healthz
  thelb offer add dns udp/53

See also: thelb admin assign
`,

	"offer remove": `Usage: thelb offer remove <name>

Withdraw an offer. The services it is assigned to lose it.

Example:
  thelb offer remove shop
`,

	"generate": `Usage: thelb generate <subcommand> [arguments]

Print a file theLB needs, for you to keep or install. It writes nothing.

Subcommands:
  secret      A new cluster secret
  commands    The admin commands that create this cluster's setup again
  systemd     A systemd unit for a director or a backend
  sysctl      A sysctl.d file with the kernel parameters theLB sets
  modules     A modules-load.d file with the kernel modules theLB uses
  k8s         The Kubernetes manifests and scripts theLB needs

Run 'thelb generate <subcommand> --help' for details.
`,

	"generate secret": `Usage: thelb generate secret

Print a new cluster secret: for a secret file made before the first bootstrap.
The first bootstrap makes the files it lacks, so this is optional.

Example:
  thelb generate secret > /etc/thelb/director.secret
`,

	"generate commands": `Usage: thelb generate commands

On a director: print the thelb admin commands that create the cluster's pools,
services and assignments again, as they are now - a shell script to keep, and
to run on a cluster created again after every director stopped. theLB never
reads it.

Example:
  thelb generate commands > /root/thelb-restore.sh
`,

	"generate systemd": `Usage: thelb generate systemd director|backend <the options it runs with>

Print a systemd unit that runs this binary as a director or a backend, with
those options - checked as the role checks them. The unit uses Type=notify,
the watchdog and Restart=always, and bounds theLB's capabilities and what it
may write.

Example:
  thelb generate systemd director --vip 10.0.0.10 --port 7443 \
    --secret-file /etc/thelb/director.secret \
    --backend-secret-file /etc/thelb/backend.secret |
    sudo tee /etc/systemd/system/thelb-director.service
  sudo systemctl daemon-reload && sudo systemctl enable --now thelb-director

Documentation: ` + docsURL + `docs/systemd.html
`,

	"generate sysctl": `Usage: thelb generate sysctl director|backend

Print a sysctl.d file with the kernel parameters theLB sets while it runs, for
them from boot. theLB itself never writes one.

Example:
  thelb generate sysctl backend | sudo tee /etc/sysctl.d/90-thelb-backend.conf
`,

	"generate modules": `Usage: thelb generate modules director|backend

Print a modules-load.d file with the kernel modules theLB uses, for them from
boot.

Example:
  thelb generate modules director | sudo tee /etc/modules-load.d/thelb-director.conf
`,

	"generate k8s": `Usage: thelb generate k8s [<form>] [arguments] [options]

Print what theLB needs in a Kubernetes cluster. It writes nothing - to disk or
to the cluster: apply it with kubectl, or run it. The nodes that run the
Services' pods need nothing; only a node that runs a director is prepared.

Forms:
  (none)        The directors' identity: namespace, ServiceAccount, RBAC and
                token Secret
  kubeconfig    A script that prints the kubeconfig of directors outside the
                cluster
  director      A DaemonSet that runs directors on labelled nodes
  node <name>   A script that makes a node a director's node
  service <name>
                A Service of class dmarts.app/thelb

Options, on every form:
  --from-kubeconfig <file>   Read the cluster with this kubeconfig, to fit the
                             output to it - it asks first, and only reads
  --from-context <name>      The kubeconfig's context (default: its current one)
  --yes                      Read it without asking, for scripts

Examples:
  thelb generate k8s > thelb-k8s.yaml && kubectl apply -f thelb-k8s.yaml
  thelb generate k8s service web --port 80:8080 > web.yaml

Run 'thelb generate k8s <form> --help' for details.
Documentation: ` + docsURL + `docs/kubernetes.html
`,

	"generate k8s kubeconfig": `Usage: thelb generate k8s kubeconfig [--from-kubeconfig <file> [--from-context <name>] [--yes]]

Print a script that prints the kubeconfig of directors outside the cluster:
kubectl's API server, and the token of the Secret thelb-director-token. Run it
where kubectl reaches the cluster, once the identity is applied.

With --from-kubeconfig, it prints the kubeconfig itself, and checks that the
token has every permission a director needs.

Examples:
  thelb generate k8s kubeconfig > thelb-kubeconfig.sh
  sh thelb-kubeconfig.sh > thelb.kubeconfig
  sudo install -D -m 600 thelb.kubeconfig /etc/thelb/kubeconfig

Then give the directors --kubeconfig /etc/thelb/kubeconfig.
`,

	"generate k8s director": `Usage: thelb generate k8s director <the options thelb director runs with>

Print the identity, and a DaemonSet that runs a director on each node labelled
dmarts.app/thelb-director, with those options - checked as thelb director
checks them, and these too:

  --kubeconfig in-cluster     The pod's own ServiceAccount
  --metrics <address:port>    The readiness probe asks /readyz there
  no --name                   Each director is named after its node
  two secret files            Of different names: the two keys of the Secret
                              thelb-secrets, made from a director's files
  --bootstrap                 Only with --bootstrap-wait 31s or more

With --from-kubeconfig, it also reads whether kube-proxy runs, and then points
the pods at the API server's address.

Example:
  thelb generate k8s director --vip 10.0.0.10 --port 7443 \
    --secret-file /etc/thelb/director.secret \
    --backend-secret-file /etc/thelb/backend.secret \
    --kubeconfig in-cluster --metrics 127.0.0.1:9180 > thelb-director.yaml
`,

	"generate k8s node": `Usage: thelb generate k8s node <name> [--from-kubeconfig <file> [--from-context <name>] [--yes]]

Print a script that makes a node a director's node, before a director runs
there (in the DaemonSet, or under systemd). It taints and drains a worker (a
control-plane node is left as it is), keeps kube-proxy off the node, labels it
dmarts.app/thelb-director=true, and with Cilium touches theLB's Services so
Cilium leaves them alone there. Its last lines say how to undo it.

Without --from-kubeconfig, the script finds out at run time what the node
needs; with it, it holds only those steps.

Example:
  thelb generate k8s node cp1 > thelb-node.sh && sh thelb-node.sh
`,

	"generate k8s service": `Usage: thelb generate k8s service <name> [options]

Print a Service of type LoadBalancer and class dmarts.app/thelb, with the
annotation that keeps Cilium off directors' nodes.

Options:
  --namespace <ns>         Its namespace (default default)
  --port <port>[/<proto>][:<pods' port>]
                           A port, as often as needed: 80, 53/udp, 80:8080,
                           443/tcp:8443 (default 80)
  --selector <key=value>   The pods it sends to (default app=<name>)
  --policy Local|Cluster   Its externalTrafficPolicy (default Local: only nodes
                           with a ready pod, and the client's address kept)
  --pool <pool>            Its VIP from this pool, not the default
  --ip <address>           This address of the pool
  --shared-vip <key>       One VIP for the Services with this key
  --from-kubeconfig <file> Take the selector and ports from the Deployment or
                           StatefulSet of the same name (see generate k8s)

Example:
  thelb generate k8s service web --port 80:8080 > web.yaml
  kubectl apply -f web.yaml
`,

	"admin": `Usage: thelb admin <subcommand> [arguments] [options]

Change the cluster. Runs on a director; a change made on another director goes
through the leader.

Subcommands:
  approve     Approve nodes waiting to join
  refuse      Refuse a node waiting to join
  remove      Remove a node from the cluster
  pool        Add or remove a pool of service VIPs
  service     Add or remove a service
  assign      Assign backends' offers to a service
  unassign    Take an offer back from a service
  lease       Set how long the control VIP outlives its leader
  secret      Show a role's secret

Run 'thelb admin <subcommand> --help' for details.
`,

	"admin approve": `Usage: thelb admin approve <name> ...
       thelb admin approve --match <regex> [--yes]

Approve nodes waiting to join. Compare their fingerprints first: thelb status
pending lists them.

Options:
  --match <regex>   Every waiting node whose whole name matches (RE2 syntax),
                    shown first, then approved after a "y"
  --yes             Approve the matches without asking, for scripts

Examples:
  thelb admin approve web-1 web-2
  thelb admin approve --match 'web-[0-9]+'

Documentation: ` + docsURL + `docs/approval.html
`,

	"admin refuse": `Usage: thelb admin refuse <name>

Refuse a node waiting to join. It is told so, and stops.
`,

	"admin remove": `Usage: thelb admin remove <name>

Remove a node from the cluster: its certificate is revoked, a director leaves
the vote, a backend's assignments end. If it asks again, it waits for approval.
`,

	"admin pool": `Usage: thelb admin pool <subcommand> [arguments] [options]

Manage the pools that services and Kubernetes Services take their VIPs from.

Subcommands:
  add       Add a pool
  remove    Remove a pool no service uses

Run 'thelb admin pool <subcommand> --help' for details.
`,

	"admin pool add": `Usage: thelb admin pool add <name> --vip <first address/prefix> --vippool-count <n> [options]

Add a pool of addresses for service VIPs, on the director's interface in that
network.

Options:
  --vip <address/prefix>   The first address, and its network's prefix
                           (required)
  --vippool-count <n>      How many addresses, 1 to 4096 (required)
  --interface <name>       The interface (default: the one in that network)
  --default                The pool a service takes its VIP from when it names
                           none - one for each address family

Example:
  # 10.10.1.20 to 10.10.1.29
  thelb admin pool add main --vip 10.10.1.20/24 --vippool-count 10 --default
`,

	"admin pool remove": `Usage: thelb admin pool remove <name>

Remove a pool that no service, and no Kubernetes Service, uses.
`,

	"admin service": `Usage: thelb admin service <subcommand> [arguments] [options]

Manage services: a VIP, a protocol and a port, served by backends' offers.

Subcommands:
  add       Add a service
  remove    Remove a service

Run 'thelb admin service <subcommand> --help' for details.
`,

	"admin service add": `Usage: thelb admin service add <name> --proto <tcp|udp|sctp> --port <port> [options]

Add a service. Its VIP is the lowest free address of its pool, or the one
asked for - probed on the network first. It comes up once a backend serves it.

Options:
  --proto tcp|udp|sctp           The protocol (required)
  --port <port>                  The port (required)
  --pool <name>                  Take the VIP from this pool (default: the
                                 default pool)
  --vip <address>                This address of the pool
  --scheduler wrr|rr|wlc|lc|sh|mh
                                 How IPVS picks a backend (default wrr)
  --method direct|tunnel         direct: backends on the director's network
                                 (default); tunnel: IP-in-IP, backends anywhere
                                 routed (IPv4)

Example:
  thelb admin service add web --proto tcp --port 80
`,

	"admin service remove": `Usage: thelb admin service remove <name>

Remove a service: its VIP, its IPVS service and its assignments.
`,

	"admin assign": `Usage: thelb admin assign <service> <node>:<offer> ... [--weight <node>=<n>,...]

Assign backends' offers to a service. Name each offer by the name its backend
gave it, or by its protocol and port: thelb status offers lists them. The
offer's protocol must be the service's; where its port differs, the backend
maps the service's port to it.

Options:
  --weight <node>=<n>,...   Weights (default 1)

Examples:
  thelb admin assign web web-1:nginx web-2:tcp/8080
  thelb admin assign web web-1:nginx web-2:nginx --weight web-1=3
`,

	"admin unassign": `Usage: thelb admin unassign <service> <node>

Take a backend's offer back from a service.
`,

	"admin lease": `Usage: thelb admin lease <seconds>

Set how long the kernel keeps the control VIP after the leader's last renewal:
3 to 3600 seconds (default 6). A leader that dies or hangs loses the VIP
within it, and a director taking over waits it out. thelb status cluster shows
it.

Example:
  thelb admin lease 10
`,

	"admin secret": `Usage: thelb admin secret show --role director|backend

Print a role's secret, for a node whose secret file is out of date - the one
way to see a secret. Rotating secrets comes later.
`,
}

// helpGroups are the pages of commands that need a subcommand: the command
// alone shows its page, as --help would.
var helpGroups = map[string]bool{"offer": true, "generate": true, "admin": true, "admin pool": true, "admin service": true}

// helpFor is the page for the command words: the longest run of them, before
// the first option, that has a page - and that run's length; "" and 0 when the
// first word is no command.
func helpFor(words []string) (string, int) {
	var path []string
	for _, w := range words {
		if strings.HasPrefix(w, "-") {
			break
		}
		path = append(path, w)
	}
	for n := len(path); n > 0; n-- {
		if h, ok := helpPages[strings.Join(path[:n], " ")]; ok {
			return h, n
		}
	}
	return "", 0
}

// commands is the command names, sorted: for tests and checks.
func commands() []string {
	var out []string
	for k := range helpPages {
		if !strings.Contains(k, " ") {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
