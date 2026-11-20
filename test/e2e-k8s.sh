#!/bin/bash
# The Kubernetes provider, end to end, as root, on Linux: two directors, three
# nodes standing in for a cluster's nodes, a client, and a stand-in API server
# (test/kubeapi), each in a network namespace of its own.
#
#   sudo test/e2e-k8s.sh bin/thelb bin/kubeapi
#
# With SCREENS=<dir>, it also writes the documentation's screenshots of
# Kubernetes there (make screens).
#
# A node here does what a cluster node's proxy does for a Service of
# externalTrafficPolicy Local - kube-proxy or Cilium: it takes packets for the
# Service's VIP (the VIP on its loopback, answering no ARP for it), hands them
# to its "pods" (a web server answering with the node's name), and answers on
# the Service's health port - 200 while it has ready pods, 503 when not.
#
# Checked: the directors' startup checks, which never stop a director - a
# director on a node of the cluster without its label, a permission refused:
# each says why, runs on without Kubernetes, and serves it by itself once
# fixed; a
# Service given its VIP from the default pool, its status written with ipMode
# VIP, an event; the nodes with ready pods serving, weighted by their pods; a
# node excluded by its label, one not ready, a director's node; a node whose
# health port says 503 out of rotation; another provider's Service left alone;
# a Service asking for an address; two Services sharing a VIP, and a third
# whose port clashes; status on a follower; the leader killed - the new one
# serves at once, from its own watch; a Service deleted - its VIP taken back;
# nothing left behind.

set -u
bin="$(readlink -f "${1:?usage: test/e2e-k8s.sh <thelb binary> <kubeapi binary>}")"
api="$(readlink -f "${2:?usage: test/e2e-k8s.sh <thelb binary> <kubeapi binary>}")"
[ "$(id -u)" = 0 ] || { echo "e2e-k8s: run as root"; exit 1; }

P=thelb-k8s
SW=$P-sw
NET=198.18.0
VIP=$NET.10 # the control VIP
PORT=7443
HC=30100    # the Service's health port
tmp=$(mktemp -d /tmp/thelb-k8s.XXXXXX)
chmod 0711 "$tmp"
failures=0
declare -A pid addr

pass() { printf '  ok    %s\n' "$1"; }
fail() { printf '  FAIL  %s\n' "$1"; failures=$((failures + 1)); }
check() { if eval "$2"; then pass "$1"; else fail "$1"; fi; }
# within <seconds> <predicate> [args]: wait until the predicate holds.
within() {
	local end=$((SECONDS + $1))
	shift
	while [ "$SECONDS" -lt "$end" ]; do
		"$@" && return 0
		sleep 0.3
	done
	"$@"
}
logged() { grep -qE -- "$2" "$1" 2>/dev/null; }

cleanup() {
	set +u
	for n in "${!pid[@]}"; do kill -9 "${pid[$n]}" 2>/dev/null; done
	for ns in $(ip netns list | awk '{print $1}'); do
		case $ns in $P-*) for p in $(ip netns pids "$ns"); do kill -9 "$p" 2>/dev/null; done ;; esac
	done
	sleep 1
	for ns in $(ip netns list | awk '{print $1}'); do
		case $ns in $P-*) ip netns del "$ns" ;; esac
	done
	if [ "$failures" -gt 0 ] || [ -n "${KEEP:-}" ]; then
		echo "e2e-k8s: the logs are in $tmp"
	else
		rm -rf "$tmp"
	fi
}
trap cleanup EXIT

echo "== the lab: two directors, three nodes, a client and an API server"
ip netns add $SW
ip -n $SW link add br0 type bridge mcast_snooping 0
ip -n $SW link set br0 up
for n in d1 d2 d3 n1 n2 n3 c api; do
	case $n in d*) i=$((10 + ${n#d})) ;; n*) i=$((30 + ${n#n})) ;; c) i=100 ;; api) i=200 ;; esac
	addr[$n]=$NET.$i
	ip netns add $P-$n
	ip link add eth0 netns $P-$n type veth peer name $n netns $SW
	ip -n $SW link set $n master br0 up
	ip -n $P-$n link set lo up
	ip -n $P-$n link set eth0 up
	ip -n $P-$n addr add ${addr[$n]}/24 dev eth0
	install -d -m 0700 "$tmp/$n"
done
echo "   $NET.0/24: directors d1-d3 at .11-.13, nodes n1-n3 at .31-.33, the client at .100, the API server at .200"

# The nodes: the VIPs a Service may get on their loopback - answering no ARP
# for them, as a node's proxy does not - a web server for their "pods", and
# the Service's health port.
cat >"$tmp/health.py" <<'EOF'
import http.server, sys
state, port = sys.argv[1], int(sys.argv[2])
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        try:
            code = int(open(state).read().strip())
        except Exception:
            code = 200
        body = b'{"localEndpoints":1}'
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def log_message(self, *a):
        pass
http.server.HTTPServer(("0.0.0.0", port), H).serve_forever()
EOF
for n in n1 n2 n3; do
	ip netns exec $P-$n sh -c 'echo 1 >/proc/sys/net/ipv4/conf/all/arp_ignore; echo 2 >/proc/sys/net/ipv4/conf/all/arp_announce'
	for v in 50 51 52 53; do ip -n $P-$n addr add $NET.$v/32 dev lo; done
	install -d -m 0755 "$tmp/$n/www"
	echo "$n" >"$tmp/$n/www/index.html"
	ip netns exec $P-$n python3 -m http.server 80 --bind 0.0.0.0 --directory "$tmp/$n/www" >/dev/null 2>&1 &
	pid[$n-web]=$!
	ip netns exec $P-$n python3 -m http.server 8080 --bind 0.0.0.0 --directory "$tmp/$n/www" >/dev/null 2>&1 &
	pid[$n-web2]=$!
	echo 200 >"$tmp/$n/health"
	ip netns exec $P-$n python3 "$tmp/health.py" "$tmp/$n/health" $HC >/dev/null 2>&1 &
	pid[$n-health]=$!
done

# The cluster's state, as the API server serves it.
node() { # name address ready labels-json
	printf '{"metadata":{"name":"%s","labels":%s},"status":{"addresses":[{"type":"InternalIP","address":"%s"}],"conditions":[{"type":"Ready","status":"%s"}]}}' "$1" "$4" "$2" "$3"
}
service() { # name uid ports-json annotations-json [class]
	printf '{"metadata":{"name":"%s","namespace":"default","uid":"%s","annotations":%s},"spec":{"type":"LoadBalancer","loadBalancerClass":"%s","externalTrafficPolicy":"Local","healthCheckNodePort":%d,"ports":%s}}' \
		"$1" "$2" "$4" "${5:-dmarts.app/thelb}" $HC "$3"
}
slice() { # service, then node:ready pairs
	local svc=$1 eps="" sep=""
	shift
	for e in "$@"; do
		eps="$eps$sep{\"addresses\":[\"10.1.0.$((${#eps} % 200 + 2))\"],\"conditions\":{\"ready\":${e#*:}},\"nodeName\":\"${e%%:*}\"}"
		sep=,
	done
	printf '{"metadata":{"name":"%s-x","namespace":"default","labels":{"kubernetes.io/service-name":"%s"}},"addressType":"IPv4","endpoints":[%s]}' "$svc" "$svc" "$eps"
}
WORKERS="$(node n1 ${addr[n1]} True '{}'),$(node n2 ${addr[n2]} True '{}'),$(node n3 ${addr[n3]} True '{"node.kubernetes.io/exclude-from-external-load-balancers":"true"}')"
# d3's node is not labelled as a director at first; then it is, and d2's is.
NODES="$WORKERS,$(node d3node ${addr[d3]} True '{}')"
WEB=$(service web u-web '[{"name":"http","protocol":"TCP","port":80,"nodePort":30080}]' '{}')
SLICES="$(slice web n1:true n1:true n2:true n3:true)"
state() { # services, slices, [deny] - then a moment for the API server to read it
	printf '{"nodes":[%s],"services":[%s],"endpointslices":[%s],"deny":[%s]}' "$NODES" "$1" "$2" "${3:-}" >"$tmp/state.json.new"
	mv "$tmp/state.json.new" "$tmp/state.json"
	sleep 1
}
state "$WEB" "$SLICES"

ip netns exec $P-api "$api" --listen ${addr[api]}:6443 --state "$tmp/state.json" --out "$tmp/api" >"$tmp/api.log" 2>&1 &
pid[api]=$!
within 10 test -s "$tmp/api/kubeconfig" || { echo "e2e-k8s: the API server did not start"; cat "$tmp/api.log"; exit 1; }
KC=$tmp/api/kubeconfig

on() { local n=$1; shift; nsenter -t "${pid[$n]}" -m -n "$@"; }
start() { # node log [flags]
	local n=$1 log=$2
	shift 2
	ip netns exec $P-$n unshare --mount --propagation private \
		sh -c 'mount -t tmpfs -o mode=0755 tmpfs /run && exec "$@"' sh \
		"$bin" director --vip $VIP --port $PORT --name $n --cluster k8s \
		--secret-file "$tmp/$n/director.secret" --backend-secret-file "$tmp/$n/backend.secret" --kubeconfig "$KC" "$@" >>"$log" 2>&1 &
	pid[$n]=$!
}
stopped() { ! kill -0 "${pid[$1]}" 2>/dev/null; }
json() { on "$1" "$bin" status --json 2>/dev/null | python3 -c "import json, sys; s = json.load(sys.stdin); print($2)" 2>/dev/null; }
is() { [ "$(json "$1" "$2")" = "$3" ]; }
leader_of() { json "$1" "s['cluster']['leader'] if s['cluster'] else ''"; }
ipvs_on() { ip netns exec $P-$1 ipvsadm -Ln 2>/dev/null; }
weight_is() { # director vip port node weight
	ipvs_on "$1" | awk -v s="TCP  $2:$3 " -v d="-> ${addr[$4]}:$3 " -v w="$5" '
		index($0, s) == 1 { on = 1; next } /^(TCP|UDP|SCTP)/ { on = 0 } on && index($0, d) { if ($4 == w) found = 1 } END { exit !found }'
}
in_ipvs() { ipvs_on "$1" | grep -qF -- "-> ${addr[$2]}:"; }
answers() { # n vip
	for _ in $(seq 1 "$1"); do ip netns exec $P-c curl -s --max-time 2 "http://$2/" || echo none; done | sort | uniq -c | awk '{print $2 "=" $1}' | xargs
}
# shot <name> <title>: start a screenshot, when SCREENS says where; then
# shown <command line> <command...> adds a command and what it printed.
shot() { [ -n "${SCREENS:-}" ] && printf '%s\n' "$2" >"$SCREENS/$1.txt" && cur="$SCREENS/$1.txt"; }
shown() { [ -n "${SCREENS:-}" ] || return 0; local line=$1; shift; printf '$ %s\n' "$line" >>"$cur"; "$@" >>"$cur" 2>&1 || true; }
status_is() { [ "$(status_of "$1")" = "$2" ]; }
shared_same() { local a; a=$(status_of default/api); [ -n "$a" ] && [ "$a" = "$(status_of default/admin)" ]; }
unset_status() { [ -z "$(status_of "$1")" ]; }
answers_match() { [[ "$(answers "$2" "$1")" =~ $3 ]]; }
no_ipvs_service() { ! ipvs_on "$1" | grep -qF "$2"; }
nothing_left() { ! ip -n $P-$1 -o addr show | grep -qE "$NET.(10|50)/" && [ -z "$(ipvs_on "$1" | grep -E '^(TCP|UDP)')" ]; }
status_of() { python3 -c "import json; s = json.load(open('$tmp/api/status.json')); print(','.join(i['ip'] + ':' + i.get('ipMode', '') for i in (s.get('$1') or {}).get('loadBalancer', {}).get('ingress', [])))" 2>/dev/null; }

echo "== the startup checks: Kubernetes never stops a director"
start d3 "$tmp/d3.log" --bootstrap
check "a director on a node of the cluster that is not labelled: it says so, and runs on" \
	"within 15 logged $tmp/d3.log 'ERROR Kubernetes not served: .*runs on the Kubernetes node d3node, which is not labelled as a director' && ! stopped d3"
check "meanwhile it leads, and status says what it waits for" \
	"within 20 logged $tmp/d3.log 'OK control VIP taken' && within 10 is d3 \"s['cluster']['kubernetes']\" 'waiting: its node d3node is not labelled as a director'"
LABELLED='{"dmarts.app/thelb-director":"true"}'
NODES="$WORKERS,$(node d3node ${addr[d3]} True "$LABELLED"),$(node d2node ${addr[d2]} True "$LABELLED")"
state "$WEB" "$SLICES"
check "labelled, it serves Kubernetes by itself" "within 40 logged $tmp/d3.log 'OK connected to Kubernetes'"
kill -TERM "${pid[d3]}"
check "it stops when asked" "within 15 stopped d3"
state "$WEB" "$SLICES" '"watch nodes"'
start d1 "$tmp/d1.log" --bootstrap
check "a permission missing: the director says which, and runs on" "within 15 logged $tmp/d1.log 'ERROR Kubernetes not served: .*lacks permissions theLB needs: watch nodes' && ! stopped d1"
check "status says what it waits for" "within 20 is d1 \"s['cluster']['kubernetes']\" 'waiting: lacks permissions: watch nodes'"
state "$WEB" "$SLICES"

echo "== a Service of theLB's class gets its VIP"
check "granted, the director serves Kubernetes by itself" "within 40 logged $tmp/d1.log 'OK connected to Kubernetes: serving Services of class dmarts.app/thelb .*version=v1.36.0-e2e.*permissions=.all 13.'"
check "it says once it has read the cluster" "within 20 logged $tmp/d1.log 'OK watching Kubernetes: the cluster read services=1 nodes=5 ready=5'"
check "it leads" "within 20 logged $tmp/d1.log 'OK control VIP taken'"
copy() { install -m 0600 "$tmp/d1/director.secret" "$tmp/d1/backend.secret" "$tmp/$1/"; }
copy d2
start d2 "$tmp/d2.log"
approve() { on d1 "$bin" admin approve "$1" >/dev/null 2>&1; }
check "d2, on a node labelled as a director, starts and serves Kubernetes" "within 20 logged $tmp/d2.log 'OK this director runs on a Kubernetes node labelled as a director node=d2node'"
within 20 approve d2
check "d2 joins, approved" "within 20 is d1 \"sum(1 for n in s['nodes'] if n['state'] == 'up')\" 2"
on d1 "$bin" admin pool add k8s --vip $NET.50/24 --vippool-count 4 --default >/dev/null
check "status.loadBalancer.ingress: the pool's first address, ipMode VIP" "within 20 status_is default/web $NET.50:VIP"
check "an event says so" "within 10 logged $tmp/api/events.log 'Normal default/web IPAllocated theLB gave it $NET.50'"
check "n3's pod gets no traffic - n3 is excluded - and a warning says why" "within 10 logged $tmp/api/events.log 'Warning default/web NodesExcluded .*1 on n3 - n3 has the label node.kubernetes.io/exclude-from-external-load-balancers'"
check "IPVS: n1 with its two ready pods, weight 2" "within 20 weight_is d1 $NET.50 80 n1 2"
check "IPVS: n2 with one, weight 1" "weight_is d1 $NET.50 80 n2 1"
check "n3, excluded by its label, is not a backend" "! in_ipvs d1 n3"
check "the directors' nodes are not backends" "! in_ipvs d1 d2 && ! in_ipvs d1 d3"
check "the client reaches the Service: n1 and n2 answer" "within 20 answers_match $NET.50 12 '^n1=[0-9]+ n2=[0-9]+$'"
check "status on a follower: the Service, two backends, both up" \
	"within 10 is d2 \"[(v['service'], v['vip'], v['backends'], v['backends_up']) for v in s['services']]\" \"[('default/web', '$NET.50', 2, 2)]\""
check "status: the VIP, a Kubernetes one, held by the leader" "is d2 \"[(v['vip'], v['purpose'], v['held_by']) for v in s['vips'] if v['purpose'] == 'kubernetes']\" \"[('$NET.50', 'kubernetes', 'd1')]\""
check "status: the cluster's watch of Kubernetes" "is d2 \"s['cluster']['kubernetes']\" '1 Services'"
if shot k8s-status "d1 - a Kubernetes Service, and the nodes serving it"; then
	shown "sudo thelb status vips" on d1 "$bin" status vips
	shown "sudo thelb status backends" on d1 "$bin" status backends
fi

echo "== the nodes come and go"
echo 503 >"$tmp/n2/health"
check "n2's health port says 503: weight 0" "within 20 weight_is d1 $NET.50 80 n2 0"
check "only n1 answers" "within 10 answers_match $NET.50 6 '^n1=6$'"
echo 200 >"$tmp/n2/health"
check "n2 healthy again: back" "within 20 weight_is d1 $NET.50 80 n2 1"
if shot k8s-log "d1 - the leader's log: a Service gets its VIP; n2 loses its pod, and gets it back"; then
	shown "journalctl -u thelb-director -o cat" grep -E 'Kubernetes Service given its VIPs|node (not )?serving' "$tmp/d1.log"
fi

echo "== other Services"
THEIRS=$(service theirs u-theirs '[{"protocol":"TCP","port":80}]' '{}' example.com/other)
ASKS=$(service asks u-asks '[{"protocol":"TCP","port":80}]' '{"dmarts.app/thelb-ip":"'$NET.53'"}')
SHARE1=$(service api u-api '[{"protocol":"TCP","port":8080}]' '{"dmarts.app/thelb-shared-vip":"front"}')
SHARE2=$(service admin u-admin '[{"protocol":"TCP","port":9090}]' '{"dmarts.app/thelb-shared-vip":"front"}')
CLASH=$(service clash u-clash '[{"protocol":"TCP","port":8080}]' '{"dmarts.app/thelb-shared-vip":"front"}')
state "$WEB,$THEIRS,$ASKS,$SHARE1,$SHARE2" "$SLICES,$(slice api n1:true),$(slice asks n2:true)"
check "a Service asking for an address gets it" "within 20 status_is default/asks $NET.53:VIP"
check "two Services sharing a VIP get the same one" "within 20 shared_same"
check "another provider's Service is left alone" "unset_status default/theirs"
state "$WEB,$THEIRS,$ASKS,$SHARE1,$SHARE2,$CLASH" "$SLICES,$(slice api n1:true),$(slice asks n2:true)"
check "a third with a port the shared VIP has: refused, with an event" "within 20 logged $tmp/api/events.log 'Warning default/clash AllocationFailed .*which has the port tcp/8080 already'"
state "$WEB" "$SLICES"
check "the Services deleted: their VIPs taken back" "within 20 logged $tmp/d1.log 'OK Kubernetes Service gone: its VIPs taken back service=default/asks'"

echo "== the leader killed"
kill -9 "${pid[d1]}"
ip netns exec $P-d1 ip link set eth0 down
check "d2 leads, and serves the Service from its own watch" "within 40 weight_is d2 $NET.50 80 n1 2"
check "the client reaches the Service through d2" "within 30 answers_match $NET.50 6 n1="

echo "== the Service deleted"
state "" ""
check "its VIP taken back" "within 20 logged $tmp/d2.log 'OK Kubernetes Service gone: its VIPs taken back service=default/web'"
check "its IPVS service gone" "within 20 no_ipvs_service d2 $NET.50:80"

echo "== nothing left behind"
kill -TERM "${pid[d2]}"
check "d2 stops" "within 30 stopped d2"
check "no VIP, no IPVS service on d2" "nothing_left d2"

[ -n "${SCREENS:-}" ] && [ -n "${SUDO_UID:-}" ] && chown "$SUDO_UID:${SUDO_GID:-$SUDO_UID}" "$SCREENS"/k8s-*.txt
if [ "$failures" -gt 0 ]; then
	echo "e2e-k8s: $failures failed"
	for f in d1 d2 d3; do echo "--- $f"; tail -30 "$tmp/$f.log"; done
	exit 1
fi
echo "e2e-k8s: all passed"
