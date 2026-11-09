#!/bin/bash
# End to end, as root, on Linux: the built binary in a lab of network
# namespaces - a switch (a bridge in a namespace of its own), four director
# nodes and a client - so nothing reaches this machine's own network. Each
# director runs in its own network namespace with its own /run (a mount
# namespace with a tmpfs), as if it were a machine of its own.
#
#   sudo test/e2e.sh bin/thelb
#
# Checked: the bootstrap - secret files, the control VIP added with theLB's
# label and address protocol, status, admin secret show, one theLB per node,
# root only; directors joining and waiting for approval, approve --match; the
# leader crashing - the control VIP moves, and its gratuitous ARP updates the
# client's neighbour table - and the crashed director coming back, rejoining
# without approval; a planned stop handing leadership over; remove, after which
# a director waits for approval again; refuse; a wrong secret; a full stop,
# after which every director is approved again; and IPv6, where the kernel has
# it. Nothing is left behind.

set -u
bin="$(readlink -f "${1:?usage: test/e2e.sh <thelb binary>}")"
[ "$(id -u)" = 0 ] || { echo "e2e: run as root"; exit 1; }
[ -x "$bin" ] || { echo "e2e: $bin is not executable"; exit 1; }

P=thelb-e2e   # every namespace's prefix
SW=$P-sw
NET4=198.18.0 # RFC 2544's benchmarking network: nobody's real addresses
VIP4=$NET4.10
NET6=fd74:6c62: # a unique local prefix: fd74:6c62::/64
VIP6=$NET6:10
PORT=7443

failures=0
pass() { printf '  ok    %s\n' "$1"; }
fail() { printf '  FAIL  %s\n' "$1"; failures=$((failures + 1)); }
check() { if eval "$2"; then pass "$1"; else fail "$1"; fi; }

for ns in $(ip netns list | awk '{print $1}'); do
	case $ns in $P-*)
		echo "e2e: namespace $ns exists: an earlier run left it, or one is running"
		exit 1
		;;
	esac
done

tmp="$(mktemp -d /tmp/thelb-e2e.XXXXXX)"
# Another user must be able to run the binary for the "root only" checks: a copy
# in a directory anyone may enter, though not list (the secret files are 0600).
chmod 0711 "$tmp"
install -d -m 0755 "$tmp/bin"
install -m 0755 "$bin" "$tmp/bin/thelb"
bin="$tmp/bin/thelb"

declare -A pid addr4 addr6 mac
cleanup() {
	set +u
	for n in "${!pid[@]}"; do kill -9 "${pid[$n]}" 2>/dev/null; done
	# Every process left in the lab's namespaces - web servers included - then
	# the namespaces. No bare wait: a process it would wait for may be one the
	# script never started.
	for ns in $(ip netns list | awk '{print $1}'); do
		case $ns in $P-*) for p in $(ip netns pids "$ns"); do kill -9 "$p" 2>/dev/null; done ;; esac
	done
	sleep 1
	for ns in $(ip netns list | awk '{print $1}'); do
		case $ns in $P-*) ip netns del "$ns" ;; esac
	done
	if [ "$failures" -gt 0 ] || [ -n "${KEEP:-}" ]; then
		echo "e2e: the logs are in $tmp"
	else
		rm -rf "$tmp"
	fi
}
trap cleanup EXIT

have6=0
[ -d /proc/sys/net/ipv6 ] && have6=1

echo "== the lab: a switch, four director nodes and a client, in network namespaces"
ip netns add $SW
ip -n $SW link add br0 type bridge mcast_snooping 0
ip -n $SW link set br0 up
for n in d1 d2 d3 d4 b1 b2 b3 c; do
	case $n in c) i=100 ;; b*) i=$((30 + ${n#b})) ;; *) i=$((10 + ${n#d})) ;; esac
	addr4[$n]=$NET4.$i
	addr6[$n]=$NET6:$i
	ip netns add $P-$n
	ip link add eth0 netns $P-$n type veth peer name $n netns $SW
	ip -n $SW link set $n master br0 up
	ip -n $P-$n link set lo up
	ip -n $P-$n link set eth0 up
	ip -n $P-$n addr add ${addr4[$n]}/24 dev eth0
	[ $have6 = 1 ] && ip -n $P-$n addr add ${addr6[$n]}/64 dev eth0 nodad
	mac[$n]=$(ip netns exec $P-$n cat /sys/class/net/eth0/address)
	install -d -m 0700 "$tmp/$n"
done
# A second network behind a router - 198.19.0.0/24, RFC 2544's other half -
# with a backend, b4, that only a tunnelled service can reach. The router lets
# replies from the VIP through: a network that checks source addresses would
# not, which is what the return path is about.
NET4B=198.19.0
ip netns add $P-sw2
ip -n $P-sw2 link add br0 type bridge mcast_snooping 0
ip -n $P-sw2 link set br0 up
ip netns add $P-r
ip link add eth0 netns $P-r type veth peer name r netns $SW
ip link add eth1 netns $P-r type veth peer name r netns $P-sw2
ip -n $SW link set r master br0 up
ip -n $P-sw2 link set r master br0 up
for i in lo eth0 eth1; do ip -n $P-r link set $i up; done
ip -n $P-r addr add $NET4.1/24 dev eth0
ip -n $P-r addr add $NET4B.1/24 dev eth1
for k in ip_forward conf/all/rp_filter conf/eth0/rp_filter conf/eth1/rp_filter; do
	v=0
	[ $k = ip_forward ] && v=1
	ip netns exec $P-r sh -c "echo $v >/proc/sys/net/ipv4/$k"
done
addr4[b4]=$NET4B.41
ip netns add $P-b4
ip link add eth0 netns $P-b4 type veth peer name b4 netns $P-sw2
ip -n $P-sw2 link set b4 master br0 up
ip -n $P-b4 link set lo up
ip -n $P-b4 link set eth0 up
ip -n $P-b4 addr add ${addr4[b4]}/24 dev eth0
ip -n $P-b4 route add default via $NET4B.1
mac[b4]=$(ip netns exec $P-b4 cat /sys/class/net/eth0/address)
install -d -m 0700 "$tmp/b4"
# link_up <director>: its link up again - and its route to the second network,
# which went with the link.
link_up() {
	ip -n $P-$1 link set eth0 up
	ip -n $P-$1 route replace $NET4B.0/24 via $NET4.1
}
for n in d1 d2 d3 d4; do ip -n $P-$n route add $NET4B.0/24 via $NET4.1; done
echo "   $NET4.0/24, control VIP $VIP4 port $PORT; directors d1-d4 at $NET4.11-14, backends b1-b3 at $NET4.31-33, the client at $NET4.100"
echo "   $NET4B.0/24 behind a router at $NET4.1: the backend b4 at ${addr4[b4]}"

# start <node> <log> [flags]: a director on the node, with a /run of its own.
start() {
	local n=$1 log=$2
	shift 2
	ip netns exec $P-$n unshare --mount --propagation private \
		sh -c 'mount -t tmpfs -o mode=0755 tmpfs /run && exec "$@"' sh \
		"$bin" director --port $PORT --name $n --cluster e2e \
		--secret-file "$tmp/$n/director.secret" --backend-secret-file "$tmp/$n/backend.secret" "$@" >>"$log" 2>&1 &
	pid[$n]=$!
}
# startb <node> <log> [flags]: a backend on the node, offering its web server.
startb() {
	local n=$1 log=$2
	shift 2
	ip netns exec $P-$n unshare --mount --propagation private \
		sh -c 'mount -t tmpfs -o mode=0755 tmpfs /run && exec "$@"' sh \
		"$bin" backend --join $VIP4:$PORT --name $n --secret-file "$tmp/$n/backend.secret" --offer web=tcp/80,check=http:/ "$@" >>"$log" 2>&1 &
	pid[$n]=$!
}
declare -A web
# app <node> up|down: the backend's web server, answering with its name.
# altport is a backend's own port for the mapped service: 8080 on b1, 9090 on
# b2, 7070 on b3 - and the service on 80.
altport() { case $1 in b1) echo 8080 ;; b2) echo 9090 ;; b3) echo 7070 ;; esac; }
declare -A webalt
app() {
	if [ "$2" = up ]; then
		install -d -m 0755 "$tmp/$1/www" "$tmp/$1/www-alt"
		echo "$1" >"$tmp/$1/www/index.html"
		echo "$1-$(altport $1)" >"$tmp/$1/www-alt/index.html"
		ip netns exec $P-$1 python3 -m http.server 80 --bind 0.0.0.0 --directory "$tmp/$1/www" >/dev/null 2>&1 &
		web[$1]=$!
		ip netns exec $P-$1 python3 -m http.server "$(altport $1)" --bind 0.0.0.0 --directory "$tmp/$1/www-alt" >/dev/null 2>&1 &
		webalt[$1]=$!
	else
		kill "${web[$1]:-}" "${webalt[$1]:-}" 2>/dev/null
		wait "${web[$1]:-}" "${webalt[$1]:-}" 2>/dev/null
		unset "web[$1]" "webalt[$1]"
	fi
}
# answers <n> [vip]: which backends answered n requests from the client to a
# service - the web service's VIP unless another is named.
answers() {
	local vip=${2:-$SVC}
	for _ in $(seq 1 "$1"); do ip netns exec $P-c curl -s --max-time 2 "http://$vip/" || echo none; done | sort | uniq -c | awk '{print $2 "=" $1}' | xargs
}
nft_on() { ip netns exec $P-$1 nft list table inet thelb 2>/dev/null; }
ipvs_on() { ip netns exec $P-$1 ipvsadm -Ln 2>/dev/null; }
weight_is() { ipvs_on "$1" | grep -qE -- "-> ${addr4[$2]}:80 +Route +$3 "; }
answers_include() { answers 9 | grep -q "$1="; }
# stop <node> [signal]: and return its exit status.
stop() {
	local n=$1 sig=${2:-TERM} code
	kill -"$sig" "${pid[$n]}" 2>/dev/null
	wait "${pid[$n]}" 2>/dev/null
	code=$?
	unset "pid[$n]"
	return $code
}
# ended <node> [seconds]: wait for a director to end by itself; return its status.
ended() {
	local n=$1 t=${2:-20} code
	for _ in $(seq 1 $((t * 5))); do
		if ! kill -0 "${pid[$n]}" 2>/dev/null; then
			wait "${pid[$n]}" 2>/dev/null
			code=$?
			unset "pid[$n]"
			return $code
		fi
		sleep 0.2
	done
	return 255
}
# on <node> <command>: run in the director's namespaces - its network and its /run.
on() {
	local n=$1
	shift
	nsenter -t "${pid[$n]}" -m -n "$@"
}
# within <seconds> <predicate> [args]: wait until the predicate holds.
within() {
	local end=$((SECONDS + $1))
	shift
	# A deadline, not a count: a predicate may take seconds itself.
	while [ "$SECONDS" -lt "$end" ]; do
		"$@" && return 0
		sleep 0.1
	done
	"$@"
}
logged() { grep -qE -- "$2" "$1" 2>/dev/null; }
json() { on "$1" "$bin" status --json 2>/dev/null | python3 -c "import json, sys; s = json.load(sys.stdin); print($2)" 2>/dev/null; }
leader_of() { json "$1" "s['cluster']['leader'] if s['cluster'] else ''"; }
up() { json "$1" "sum(1 for n in s['nodes'] if n['state'] == 'up')"; }
holds() { ip -n $P-$1 -o addr show dev eth0 | grep -qF " $2/"; }
# Predicates, for within.
is() { [ "$(json "$1" "$2")" = "$3" ]; }
isnt() { [ "$(json "$1" "$2")" != "$3" ]; }
up_is() { [ "$(up "$1")" = "$2" ]; }
both_logged() { logged "$1" "$3" && logged "$2" "$3"; }
leader_is() { [ "$(leader_of "$1")" = "$2" ]; }
held_not_by() { local h; h=$(holder "$1"); [ -n "$h" ] && [ "$h" != "$2" ]; }
held() { [ -n "$(holder "$1")" ]; }
neigh_is() { [ "$(neigh_of "$1")" = "$2" ]; }
same_view() { [ "$(json "$1" "s['cluster']['leader'], s['cluster']['term']")" = "$(json "$2" "s['cluster']['leader'], s['cluster']['term']")" ]; }
# holder: the director holding the VIP on a link that is up - a crashed
# machine's address, on a link that is down, holds nothing.
holder() {
	local n
	for n in d1 d2 d3 d4; do
		ip -n $P-$n -o link show dev eth0 | grep -q '[<,]UP[,>]' && holds $n "$1" && echo $n
	done
}
neigh_of() { ip -n $P-c neigh show "$1" dev eth0 | awk '{for (i = 1; i < NF; i++) if ($i == "lladdr") print $(i+1)}'; }
reach() { ip netns exec $P-c timeout 3 bash -c "exec 3<>/dev/tcp/$1/$PORT" 2>/dev/null; }
copy_secrets() {
	cp -p "$tmp/d1/director.secret" "$tmp/d1/backend.secret" "$tmp/$1/"
}

echo "== bootstrap"
start d1 "$tmp/d1.log" --vip $VIP4 --bootstrap
check "the leader takes the control VIP" "within 20 logged $tmp/d1.log 'OK control VIP taken'"
for f in director backend; do
	check "the $f secret file is root's, mode 0600" "[ \"\$(stat -c '%U %a' $tmp/d1/$f.secret)\" = 'root 600' ]"
	check "the $f secret file holds a secret" "grep -qE '^thelb1:[A-Za-z0-9_-]{43}$' $tmp/d1/$f.secret"
done
line=$(ip -n $P-d1 -o addr show dev eth0 | grep -F " $VIP4/32")
check "the control VIP is on d1's eth0 as $VIP4/32" "[ -n \"\$line\" ]"
check "with theLB's label, eth0:thelb" "echo \"\$line\" | grep -q 'eth0:thelb'"
check "and theLB's address protocol, 116" "echo \"\$line\" | grep -q 'proto 0x74'"
check "/run/thelb and the director's directory in it are root's, mode 0700" "[ \"\$(on d1 stat -c '%U %a' /run/thelb /run/thelb/director | xargs)\" = 'root 700 root 700' ]"
check "the socket is root's, mode 0600" "[ \"\$(on d1 stat -c '%U %a' /run/thelb/director/thelb.sock)\" = 'root 600' ]"
on d1 "$bin" status all >"$tmp/status" 2>&1
check "status shows the cluster, led by d1" "grep -qE '^ e2e +\\| [0-9a-f]{8} \\| .* \\| d1 +\\| +[0-9]+ \\|' $tmp/status"
check "status shows the CA and d1's certificate" "grep -q 'theLB e2e CA | ca' $tmp/status && grep -qE '^ d1 +\\| director' $tmp/status"
check "status shows the control VIP, held by d1" "grep -qE '^ $VIP4 +\\| control +\\| - +\\| eth0 +\\| d1' $tmp/status"
cluster1=$(json d1 "s['cluster']['id']")
check "the client reaches the control VIP" "reach $VIP4"
check "the client's neighbour table has d1's MAC address for it" "[ \"\$(neigh_of $VIP4)\" = ${mac[d1]} ]"
for f in director backend; do
	check "admin secret show --role $f is the file's" "[ \"\$(on d1 $bin admin secret show --role $f)\" = \"\$(cat $tmp/d1/$f.secret)\" ]"
done
on d1 "$bin" director --vip $VIP4 --port $PORT --name x --secret-file "$tmp/x.secret" --backend-secret-file "$tmp/y.secret" >"$tmp/second" 2>&1
code=$?
check "a second director on d1 exits 1: already running" "[ $code = 1 ] && grep -q 'already running' $tmp/second"
check "and creates no secret file" "[ ! -e $tmp/x.secret ] && [ ! -e $tmp/y.secret ]"
check "help runs for anyone" "runuser -u nobody -- $bin --help >/dev/null"
check "--version runs for anyone" "runuser -u nobody -- $bin --version | grep -q '^thelb '"
check "status refuses a user" "on d1 runuser -u nobody -- $bin status 2>&1 | grep -q 'runs only as root'"
check "the socket refuses a user's process" "! on d1 runuser -u nobody -- python3 -c 'import socket; s=socket.socket(socket.AF_UNIX); s.connect(\"/run/thelb/director/thelb.sock\")' 2>/dev/null"

echo "== two more directors join, and wait for approval"
for n in d2 d3; do
	copy_secrets $n
	start $n "$tmp/$n.log" --vip $VIP4
done
check "d2 and d3 wait for approval" "within 20 both_logged $tmp/d2.log $tmp/d3.log 'OK waiting for approval'"
check "d1 lists both as pending" "within 10 is d1 'len(s[\"pending\"])' 2"
fp2=$(json d2 "s['nodes'][0]['node_fingerprint']")
check "the fingerprint d1 shows for d2 is the one d2 shows" "[ -n \"$fp2\" ] && on d1 $bin status pending | grep -qE '^ d2 +\\| director \\| ${addr4[d2]} \\| ${mac[d2]} \\| $fp2 '"
on d1 "$bin" admin approve --match 'd[23]' </dev/null >"$tmp/approve" 2>&1
code=$?
check "approve --match without a terminal or --yes approves nothing" "[ $code = 1 ] && grep -q 'nothing approved' $tmp/approve && is d1 'len(s[\"pending\"])' 2"
check "approve --match 'd[23]' --yes approves both" "on d1 $bin admin approve --match 'd[23]' --yes | grep -q 'approved: d2, d3'"
check "both join" "within 20 both_logged $tmp/d2.log $tmp/d3.log 'OK director ready'"
check "d1 shows three directors up" "within 20 up_is d1 3"
check "d2, a follower, sees the same leader and term" "within 10 same_view d2 d1"
check "and three directors up" "within 10 up_is d2 3"
on d1 "$bin" status nodes certificates pending vips >"$tmp/status3"
cat "$tmp/status3" | sed 's/^/      /'

echo "== the leader crashes: its process is killed, its link goes down"
before=$(date +%s.%N)
stop d1 KILL
ip -n $P-d1 link set eth0 down
check "another director takes the control VIP" "within 20 held_not_by $VIP4 d1"
after=$(date +%s.%N)
new=$(holder $VIP4)
awk -v a="$after" -v b="$before" -v n="$new" 'BEGIN { printf "      failover: %.1f s, to %s\n", a - b, n }'
check "its gratuitous ARP moved the client's neighbour entry to $new's MAC address" "within 5 neigh_is $VIP4 ${mac[$new]}"
check "the client reaches the control VIP" "reach $VIP4"
check "the survivors agree on $new as leader" "within 10 leader_is d2 $new && within 10 leader_is d3 $new"
check "and show d1 down" "within 10 is $new '[n[\"state\"] for n in s[\"nodes\"] if n[\"node\"] == \"d1\"][0]' down"

echo "== d1 comes back - with --bootstrap, as it was started - and rejoins without approval"
link_up d1
start d1 "$tmp/d1.rejoin.log" --vip $VIP4 --bootstrap
check "it removes the control VIP its crash left" "within 10 logged $tmp/d1.rejoin.log 'WARN removed the control VIP an earlier run left vip=$VIP4 interface=eth0'"
check "the VIP is no longer on d1's eth0" "! holds d1 $VIP4"
check "it finds the cluster and rejoins" "within 30 logged $tmp/d1.rejoin.log 'OK joined the cluster cluster=e2e id=$cluster1'"
check "without waiting for approval" "! logged $tmp/d1.rejoin.log 'waiting for approval'"
check "the leader says so" "within 10 eval \"grep -q 'OK director rejoined: approved before node=d1' $tmp/$new*.log\""
check "and removes its old member identity" "within 20 eval \"grep -q 'OK old member identity removed node=d1' $tmp/$new*.log\""
check "three directors up" "within 20 up_is $new 3"

echo "== a planned stop: the leader hands leadership over"
old=$new
stop $old
code=$?
check "$old stops cleanly on SIGTERM" "[ $code = 0 ] && logged $tmp/$old.log 'OK stopped'"
check "after handing leadership over" "logged $tmp/$old.log 'OK leadership handed over to=d'"
check "and giving the control VIP up" "! holds $old $VIP4"
check "another director holds it" "within 10 held $VIP4"
new=$(holder $VIP4)
check "the client's neighbour entry follows, to $new" "within 5 neigh_is $VIP4 ${mac[$new]}"
start $old "$tmp/$old.planned.log" --vip $VIP4
check "$old rejoins without approval" "within 30 logged $tmp/$old.planned.log 'OK director ready' && ! logged $tmp/$old.planned.log 'waiting for approval'"
check "three directors up" "within 20 up_is $new 3"

echo "== a partition: the leader is cut off, and gives the control VIP up by itself"
cut=$new
ip -n $P-$cut link set eth0 down
check "the cut-off leader $cut gives the VIP up" "within 10 eval '! holds $cut $VIP4'"
check "saying it lost leadership" "grep -qF 'OK control VIP released vip=$VIP4 reason=\"leadership lost\"' $tmp/$cut*.log"
check "the others elect a leader, who takes the VIP" "within 20 held_not_by $VIP4 $cut"
new=$(holder $VIP4)
link_up $cut
check "back on the network, $cut follows $new - the same process, the same member" "within 20 leader_is $cut $new && within 20 up_is $new 3"

echo "== remove: the removed director stops, and waits for approval if it asks again"
victim=""
admin=$new
for n in d1 d2 d3; do [ $n != "$new" ] && [ -z "$victim" ] && victim=$n; done
check "admin remove $victim, on the leader" "on $admin $bin admin remove $victim | grep -q 'removed: $victim'"
ended $victim
code=$?
check "$victim stops: removed from the cluster" "[ $code = 1 ] && grep -q 'removed from the cluster' $tmp/$victim*.log"
check "two directors left" "within 10 is $new 'len(s[\"nodes\"])' 2"
start $victim "$tmp/$victim.removed.log" --vip $VIP4
check "$victim, started again, waits for approval" "within 20 logged $tmp/$victim.removed.log 'OK waiting for approval'"
check "and joins once approved" "on $admin $bin admin approve $victim >/dev/null && within 20 logged $tmp/$victim.removed.log 'OK director ready'"

echo "== a director with another secret, and a refused one"
"$bin" generate secret >"$tmp/d4/director.secret"
"$bin" generate secret >"$tmp/d4/backend.secret"
chmod 0600 "$tmp/d4/director.secret" "$tmp/d4/backend.secret"
start d4 "$tmp/d4.log" --vip $VIP4
ended d4
code=$?
check "a director with another secret stops: out of date" "[ $code = 1 ] && grep -q 'did not accept the secret in $tmp/d4/director.secret: it is out of date, or belongs to another cluster' $tmp/d4.log"
check "the leader logs the refusal" "grep -qE 'WARN join refused: the directors. secret was not proved from=${addr4[d4]}' $tmp/$new*.log"
copy_secrets d4
start d4 "$tmp/d4.right.log" --vip $VIP4
check "with the right secret, it waits for approval" "within 20 logged $tmp/d4.right.log 'OK waiting for approval'"
check "admin refuse d4" "on $admin $bin admin refuse d4 | grep -q 'refused: d4'"
ended d4
code=$?
check "d4 stops: refused" "[ $code = 1 ] && grep -q 'refused this director: refused by $admin' $tmp/d4.right.log"

echo "== a full stop: the cluster is created again, and everyone is approved again"
for n in d1 d2 d3; do [ -n "${pid[$n]:-}" ] && stop $n; done
check "nothing holds the control VIP" "[ -z \"\$(holder $VIP4)\" ]"
start d1 "$tmp/d1.full.log" --vip $VIP4 --bootstrap
check "d1 creates a cluster again" "within 30 logged $tmp/d1.full.log 'OK control VIP taken'"
check "a new one: a new ID, the same secrets" "isnt d1 's[\"cluster\"][\"id\"]' $cluster1 && ! logged $tmp/d1.full.log 'secret file created'"
start d2 "$tmp/d2.full.log" --vip $VIP4
check "d2 joins it, and waits for approval again" "within 20 logged $tmp/d2.full.log 'OK waiting for approval'"
check "and is a member once approved" "on d1 $bin admin approve d2 >/dev/null && within 20 logged $tmp/d2.full.log 'OK director ready'"
for n in d1 d2; do stop $n; done

echo "== services: three backends, a pool, a service, and traffic through IPVS"
SVC=$NET4.20
start d1 "$tmp/d1.svc.log" --vip $VIP4 --bootstrap
check "d1 creates a cluster" "within 30 logged $tmp/d1.svc.log 'OK control VIP taken'"
for n in d2 d3; do start $n "$tmp/$n.svc.log" --vip $VIP4; done
check "d2 and d3 join, once approved" "within 20 both_logged $tmp/d2.svc.log $tmp/d3.svc.log 'OK waiting for approval' && on d1 $bin admin approve --match 'd[23]' --yes >/dev/null && within 20 both_logged $tmp/d2.svc.log $tmp/d3.svc.log 'OK director ready'"
for n in b1 b2 b3; do
	cp -p "$tmp/d1/backend.secret" "$tmp/$n/"
	app $n up
	startb $n "$tmp/$n.log" --offer "alt=tcp/$(altport $n),check=http:/"
done
check "the backends enrol, and wait for approval" "within 20 eval 'logged $tmp/b1.log \"OK waiting for approval\" && logged $tmp/b2.log \"OK waiting for approval\" && logged $tmp/b3.log \"OK waiting for approval\"'"
check "approve --match 'b[1-3]' --yes" "on d1 $bin admin approve --match 'b[1-3]' --yes | grep -q 'approved: b1, b2, b3'"
check "the backends are members" "within 20 eval 'logged $tmp/b1.log \"OK backend ready\" && logged $tmp/b2.log \"OK backend ready\" && logged $tmp/b3.log \"OK backend ready\"'"
check "a backend's own status shows only itself: its offers, in the cluster, waiting for an assignment" "within 20 eval 'on b1 $bin status | grep -qE \"^ backend \\| e2e +\\| a member of the cluster \\| web +\\| tcp/80 +\\| http / +\\| (ok|fail|-) +\\| yes +\\| - \"'"
check "status offers on a director: each backend's two, by name, unassigned" "within 20 eval '[ \$(on d1 $bin status offers | grep -cE \"^ b[1-3] +\\| (web|alt) +\\| .*\\| - +\\|\") = 6 ]'"
check "admin pool add: a pool that would hold a backend's address is refused" "! on d2 $bin admin pool add bad --vip $NET4.30/24 --vippool-count 5 2>/dev/null"
check "admin pool add lab, from a follower" "on d2 $bin admin pool add lab --vip $SVC/24 --vippool-count 10 | grep -q 'pool lab: $SVC - $NET4.29, on eth0'"
check "admin service add web: the pool's first address" "on d1 $bin admin service add web --pool lab --proto tcp --port 80 | grep -q 'service web: $SVC tcp/80'"
check "its VIP waits for a backend: no director holds it yet" "sleep 2; [ -z \"\$(holder $SVC)\" ]"
check "admin assign web b1 b2 b3" "on d1 $bin admin assign web b1:tcp/80 b2:tcp/80 b3:tcp/80 | grep -q 'assigned to web: b1, b2, b3'"
check "the leader holds the service VIP, once a backend serves it" "within 20 holds d1 $SVC"
check "each backend holds it on thelb0, with theLB's address protocol" "within 10 eval 'ip -n $P-b1 -o addr show dev thelb0 | grep -q \" $SVC/32 .*proto 0x74\" && ip -n $P-b3 -o addr show dev thelb0 | grep -q \" $SVC/32\"'"
check "and answers no ARP for it: arp_ignore 1, arp_announce 2" "[ \"\$(ip netns exec $P-b1 cat /proc/sys/net/ipv4/conf/all/arp_ignore /proc/sys/net/ipv4/conf/all/arp_announce | xargs)\" = '1 2' ]"
check "the leader pins each backend's address to its MAC address" "within 10 eval '[ \$(ip -n $P-d1 neigh show nud permanent | grep -c lladdr) = 3 ]'"
if command -v ipvsadm >/dev/null; then
	check "IPVS on the leader: direct routing to the three, weight 1 (ipvsadm)" "within 20 eval 'weight_is d1 b1 1 && weight_is d1 b2 1 && weight_is d1 b3 1'"
	ipvs_on d1 | sed 's/^/      /'
fi
check "the client's requests are spread across b1, b2 and b3" "[ \"\$(answers 12)\" = 'b1=4 b2=4 b3=4' ]"
check "status: the service up, 3 of 3" "on d2 $bin status vips | grep -qE '^ web +\| $SVC +\| tcp/80 +\| wrr +\| direct +\| 3 of 3'"
check "status backends: both checks ok" "[ \$(on d1 $bin status backends | grep -c '| ok / ok |') = 3 ]"
on d1 "$bin" status >"$tmp/summary"
check "status, alone, is the summary" "grep -qE '^ directors +\| +3 \| +3 \|' $tmp/summary && grep -qE '^ backends +\| +3 \| +3 \|' $tmp/summary && grep -q 'More: thelb status' $tmp/summary"
on d1 "$bin" status backends | sed 's/^/      /'

echo "== offers made while a backend runs: checked by the leader first, kept by the backend"
check "thelb offer add on b1: refused while nothing answers" "! on b1 $bin offer add extra tcp/6060 2>$tmp/offer.err && grep -q 'the leader.s check of extra (tcp) at ${addr4[b1]}:6060 fails' $tmp/offer.err"
ip netns exec $P-b1 python3 -m http.server 6060 --bind 0.0.0.0 --directory "$tmp/b1/www" >/dev/null 2>&1 &
extra=$!
check "accepted once it answers, unassigned" "within 10 eval 'on b1 $bin offer add extra tcp/6060 >$tmp/offer.out 2>&1' && grep -qx 'offer extra: tcp/6060 at ${addr4[b1]}:6060, unassigned; it answers the leader.s check (tcp)' $tmp/offer.out"
check "another name for the same port is refused" "on b1 $bin offer add other tcp/6060 2>&1 | grep -q 'offers tcp/6060 already, as extra'"
check "status offers on a director: extra, unassigned, its own check ok" "within 10 eval 'on d1 $bin status offers | grep -qE \"^ b1 +\\| extra +\\| ${addr4[b1]} +\\| tcp/6060 +\\| tcp +\\| ok +\\| - \"'"
check "a director's host has no thelb offer: it runs on a backend" "on d1 $bin offer add x tcp/1 2>&1 | grep -q 'no backend runs on this node'"
on d1 "$bin" status offers | sed 's/^/      /'

echo "== generate commands: every director stops, and the services come back from the script"
on d1 "$bin" generate commands >"$tmp/restore.sh"
check "generate commands prints the pool, the service on its VIP, and the assignment" "grep -qx 'thelb admin pool add lab --vip $SVC/24 --vippool-count 10' $tmp/restore.sh && grep -qx 'thelb admin service add web --pool lab --vip $SVC --proto tcp --port 80 --scheduler wrr --method direct' $tmp/restore.sh && grep -qx 'thelb admin assign web b1:tcp/80 b2:tcp/80 b3:tcp/80' $tmp/restore.sh"
sed 's/^/      /' "$tmp/restore.sh"
approve_line=$(sed -n 's/^#   \(thelb admin approve --match .*\)$/\1/p' "$tmp/restore.sh")
check "and the nodes to approve, as a comment" "[ \"\$approve_line\" = \"thelb admin approve --match 'b1|b2|b3|d2|d3'\" ]"
for n in d1 d2 d3; do stop $n; done
check "every director stopped: no VIP is held" "[ -z \"\$(holder $SVC)\" ] && [ -z \"\$(holder $VIP4)\" ]"
start d1 "$tmp/d1.restore.log" --vip $VIP4 --bootstrap
check "d1 creates the cluster again - empty" "within 30 logged $tmp/d1.restore.log 'OK control VIP taken' && is d1 'len(s[\"services\"])' 0"
for n in d2 d3; do start $n "$tmp/$n.restore.log" --vip $VIP4; done
check "the directors ask again; the script's approve line approves them" "within 20 both_logged $tmp/d2.restore.log $tmp/d3.restore.log 'OK waiting for approval' && on d1 env PATH=$tmp/bin:\$PATH sh -c \"\$approve_line --yes\" >/dev/null && within 20 both_logged $tmp/d2.restore.log $tmp/d3.restore.log 'OK director ready'"
check "the backends find the new cluster, and enrol again" "within 60 eval 'logged $tmp/b1.log \"another cluster answers\" && logged $tmp/b2.log \"another cluster answers\" && logged $tmp/b3.log \"another cluster answers\"'"
# The line approves whichever of them ask: those that asked before the
# directors were approved are members already.
approve_again() { on d1 env "PATH=$tmp/bin:$PATH" sh -c "$approve_line --yes" >/dev/null 2>&1; is d1 'len({o["node"] for o in s["offers"]})' "$1"; }
check "the script's approve line approves them too: the three backends' offers are listed" "within 60 approve_again 3"
check "b1 offers extra again, by itself: a backend keeps its offers through the directors' outage" "within 30 eval 'on d1 $bin status offers | grep -qE \"^ b1 +\\| extra +\\|\"'"
check "the script creates the pool, the service and the assignment again" "on d1 env PATH=$tmp/bin:\$PATH sh $tmp/restore.sh >$tmp/restore.out 2>&1 && grep -q 'assigned to web: b1, b2, b3' $tmp/restore.out"
check "on the VIP it had: the client is served as before" "within 30 eval '[ \"\$(answers 12)\" = \"b1=4 b2=4 b3=4\" ]'"
check "thelb offer remove extra: withdrawn" "on b1 $bin offer remove extra | grep -qx 'withdrawn: extra (tcp/6060)' && ! on d1 $bin status offers | grep -q ' extra '"
kill $extra 2>/dev/null
wait $extra 2>/dev/null

echo "== port mapping: a service on port 80, served from 8080 on b1, 9090 on b2, 7070 on b3"
MAPPED=$NET4.21
check "admin service add mapped: the pool's next address" "on d1 $bin admin service add mapped --pool lab --proto tcp --port 80 | grep -q 'service mapped: $MAPPED tcp/80'"
check "its VIP waits for a backend" "sleep 2; ! holds d1 $MAPPED"
check "admin assign mapped b1:alt b2:alt b3:alt - each backend's alt offer, by name: 8080, 9090, 7070" "on d1 $bin admin assign mapped b1:alt b2:alt b3:alt | grep -q 'assigned to mapped: b1, b2, b3'"
check "the leader holds the VIP once a backend serves it" "within 20 holds d1 $MAPPED"
if command -v nft >/dev/null; then
	check "b2 maps $MAPPED tcp/80 to 9090, in theLB's nftables table" "within 10 eval 'nft_on b2 | grep -q \"ip daddr $MAPPED tcp dport 80 dnat ip to :9090\"'"
	check "and only that: web's port is its application's, so no rule" "[ \$(nft_on b2 | grep -c dnat) = 1 ]"
	nft_on b2 | sed 's/^/      /'
fi
if command -v ipvsadm >/dev/null; then
	check "IPVS sends the mapped service's packets to the backends unchanged, at port 80" "within 20 eval 'ipvs_on d1 | grep -A3 \"TCP  $MAPPED:80\" | grep -cE \"Route +1 \" | grep -qx 3'"
fi
check "the client's requests are answered from the three ports" "within 20 eval '[ \"\$(answers 9 $MAPPED)\" = \"b1-8080=3 b2-9090=3 b3-7070=3\" ]'"
check "status backends: each at its own port" "on d1 $bin status backends | grep -qE '^ mapped +\| $MAPPED:80 +\| b2 +\| ${addr4[b2]}:9090 +\| ok / ok'"
check "a backend's own status names the service and its port" "on b2 $bin status | grep -qE '\| alt +\| tcp/9090 +\| http / +\| ok +\| yes +\| mapped \(port 80\)'"
check "admin unassign mapped b2: b2's rule goes, and with it the table" "on d1 $bin admin unassign mapped b2 >/dev/null && within 10 eval '! nft_on b2 >/dev/null'"
check "the client is answered by b1 and b3" "within 10 eval '[ \"\$(answers 6 $MAPPED)\" = \"b1-8080=3 b3-7070=3\" ]'"
check "admin service remove mapped: every backend's table goes" "on d1 $bin admin service remove mapped >/dev/null && within 10 eval '! nft_on b1 >/dev/null && ! nft_on b3 >/dev/null'"
check "and the VIP" "within 10 eval '! holds d1 $MAPPED && ! ip -n $P-b1 -o addr show dev thelb0 | grep -q \" $MAPPED/\"'"

echo "== a backend's application fails, and comes back"
app b2 down
check "b2's weight goes to 0" "within 20 weight_is d1 b2 0"
check "the leader says why" "logged $tmp/d1.restore.log 'WARN backend not serving: weight 0 service=web node=b2'"
check "the client is served by b1 and b3 only" "! answers 9 | grep -q b2="
app b2 up
check "b2 serves again" "within 20 weight_is d1 b2 1 && within 10 answers_include b2"

echo "== the leader crashes, with the services"
stop d1 KILL
ip -n $P-d1 link set eth0 down
check "another director takes the service VIP" "within 20 held_not_by $SVC d1"
new=$(holder $SVC)
check "and programs IPVS the same way" "within 20 eval 'weight_is $new b1 1 && weight_is $new b2 1 && weight_is $new b3 1'"
check "the client's neighbour entry follows, to $new" "within 5 neigh_is $SVC ${mac[$new]}"
check "the client is served by all three again" "within 10 eval '[ \"\$(answers 12)\" = \"b1=4 b2=4 b3=4\" ]'"
link_up d1
start d1 "$tmp/d1.svc2.log" --vip $VIP4 --bootstrap
check "d1, back, removes the service VIP its crash left" "within 20 logged $tmp/d1.svc2.log 'WARN removed a VIP an earlier run left vip=$SVC'"
check "and the IPVS service" "within 20 logged $tmp/d1.svc2.log 'WARN removed an IPVS service an earlier run left service=tcp/$SVC:80'"
check "so d1's kernel holds none of it" "! holds d1 $SVC && ! ipvs_on d1 | grep -q Route"

echo "== a director that is also a backend: one host, two daemons, two names"
host=$new
# The backend on the leader's host: the same command as any backend's, started
# later, in the director's namespaces - its own process, its own socket.
# Its web server serves a directory of its own - never the director's, which
# holds the secret files.
install -d -m 0755 "$tmp/$host-www"
echo "$host-web" >"$tmp/$host-www/index.html"
ip netns exec $P-$host python3 -m http.server 80 --bind 0.0.0.0 --directory "$tmp/$host-www" >/dev/null 2>&1 &
web[$host]=$!
nsenter -t "${pid[$host]}" -m -n "$bin" backend --join $VIP4:$PORT --name $host --secret-file "$tmp/$host/backend.secret" --offer web=tcp/80,check=http:/ >"$tmp/$host-web.name.log" 2>&1 &
cohost_try=$!
check "a backend with its director's name is refused, and told to name itself" "{ wait $cohost_try; [ \$? = 1 ]; } && grep -q 'is a director.s: give this backend a name of its own, with --name' $tmp/$host-web.name.log"
nsenter -t "${pid[$host]}" -m -n "$bin" backend --join $VIP4:$PORT --name $host-web --secret-file "$tmp/$host/backend.secret" --offer web=tcp/80,check=http:/ >"$tmp/$host-web.log" 2>&1 &
pid[$host-web]=$!
check "the director's host runs a backend beside it: its own socket" "within 20 logged $tmp/$host-web.log 'OK waiting for approval' && on $host stat /run/thelb/backend/thelb.sock >/dev/null"
check "approved, it is a member" "on $host $bin admin approve $host-web >/dev/null && within 20 logged $tmp/$host-web.log 'OK backend ready'"
check "status there is the director's view, which lists the host twice" "on $host $bin status backends | grep -qE '^ $host-web +\| backend' && on $host $bin status directors | grep -qE '^ $host +\| leader'"
check "and --role backend is the backend's own view" "on $host $bin status --role backend | grep -qE '^ backend \| e2e +\| a member of the cluster'"
check "admin assign web $host-web:web" "within 20 eval 'on $host $bin admin assign web $host-web:web 2>/dev/null | grep -q \"assigned to web: $host-web\"'"
if command -v ipvsadm >/dev/null; then
	# Direct routing to one of its own addresses ends in its own stack: ipvsadm
	# shows it as Route, or Local.
	check "while it leads, IPVS gives it its share too" "within 20 eval 'ipvs_on $host | grep -qE -- \"-> ${addr4[$host]}:80 +(Local|Route) +1 \"'"
	ipvs_on $host | sed 's/^/      /'
fi
check "the client is served by the director's host too" "within 20 answers_include $host-web"
stop $host
code=$?
check "its director stops - the backend goes on" "[ $code = 0 ] && kill -0 ${pid[$host-web]} && within 20 held_not_by $SVC $host"
new=$(holder $SVC)
check "now the backend holds the VIP on thelb0 instead" "within 10 eval 'ip -n $P-$host -o addr show dev thelb0 | grep -q \" $SVC/32\"'"
if command -v ipvsadm >/dev/null; then
	check "and the new leader, $new, sends it its share by direct routing" "within 20 eval 'ipvs_on $new | grep -qE -- \"-> ${addr4[$host]}:80 +Route +1 \"'"
	ipvs_on $new | sed 's/^/      /'
fi
check "the client is still served by it" "within 20 answers_include $host-web"
check "admin unassign web $host-web, and it stops cleanly" "on $new $bin admin unassign web $host-web >/dev/null && stop $host-web && logged $tmp/$host-web.log 'OK stopped'"
app $host down
start $host "$tmp/$host.back.log" --vip $VIP4
check "its director starts again, and rejoins" "within 30 logged $tmp/$host.back.log 'OK director ready'"

echo "== IP-in-IP: a service whose backends are reached by tunnel - one in another network"
cp -p "$tmp/d1/backend.secret" "$tmp/b4/"
install -d -m 0755 "$tmp/b4/www"
echo b4 >"$tmp/b4/www/index.html"
ip netns exec $P-b4 python3 -m http.server 80 --bind 0.0.0.0 --directory "$tmp/b4/www" >/dev/null 2>&1 &
web[b4]=$!
startb b4 "$tmp/b4.log"
check "b4 joins through the router, and says it can serve only tunnelled services" "within 20 logged $tmp/b4.log 'OK the control VIP is behind a router: this backend can serve only tunnelled services'"
check "and is a member once approved" "within 20 logged $tmp/b4.log 'OK waiting for approval' && on $new $bin admin approve b4 >/dev/null && within 20 logged $tmp/b4.log 'OK backend ready'"
check "and offers its web server, once its own check passes - checked by the leader through the router" "within 20 eval 'on $new $bin status offers | grep -qE \"^ b4 +\\| web +\\| ${addr4[b4]} \"'"
check "direct routing to b4 is refused: no interface in its network" "on $new $bin admin assign web b4:tcp/80 2>&1 | grep -q 'a service with --method tunnel reaches backends in other networks'"
out=$(on $new "$bin" admin service add far --pool lab --proto tcp --port 80 --method tunnel)
FAR=$(echo "$out" | sed -n 's/^service far: \([0-9.]*\) tcp\/80, by IP-in-IP$/\1/p')
check "admin service add far --method tunnel" "[ -n \"$FAR\" ]"
check "admin assign far b4 b1: one behind the router, one beside the director" "on $new $bin admin assign far b4:tcp/80 b1:tcp/80 | grep -q 'assigned to far: b4, b1'"
if command -v ipvsadm >/dev/null; then
	check "IPVS sends both by tunnel" "within 20 eval 'ipvs_on $new | grep -A3 \"TCP  $FAR:80\" | grep -cE -- \"(${addr4[b4]}|${addr4[b1]}):80 +Tunnel +1 \" | grep -qx 2'"
	ipvs_on $new | sed -n "/TCP  $FAR:80/,/^TCP/p" | sed 's/^/      /'
fi
check "b4 unwraps them on thelbt0, and holds the VIP on thelb0" "within 10 eval 'ip -n $P-b4 -o link show thelbt0 | grep -q ipip && ip -n $P-b4 -o addr show dev thelb0 | grep -q \" $FAR/32\"'"
if command -v nft >/dev/null; then
	check "and clamps its handshakes' MSS, so wrapped packets fit" "nft_on b4 | grep -q \"ip saddr $FAR tcp flags syn tcp option maxseg size > 1440 tcp option maxseg size set 1440\""
fi
check "status: the service's method is tunnel" "on $new $bin status vips | grep -qE '^ far +\| $FAR +\| tcp/80 +\| wrr +\| tunnel '"
check "the client is served by both - b4's replies coming back through the router" "within 20 eval '[ \"\$(answers 8 $FAR)\" = \"b1=4 b4=4\" ]'"
check "admin unassign far b1: b1's tunnel goes" "on $new $bin admin unassign far b1 >/dev/null && within 10 eval '! ip -n $P-b1 link show thelbt0 >/dev/null 2>&1'"
check "admin service remove far: b4's tunnel, clamp and VIP go" "on $new $bin admin service remove far >/dev/null && within 10 eval '! ip -n $P-b4 link show thelbt0 >/dev/null 2>&1 && ! nft_on b4 >/dev/null && ! ip -n $P-b4 -o addr show dev thelb0 | grep -q \" $FAR/\"'"
stop b4
code=$?
check "b4 stops cleanly" "[ $code = 0 ] && logged $tmp/b4.log 'OK stopped'"
app b4 down

echo "== a backend stops, one is removed, the service is taken down"
stop b3
code=$?
check "b3 stops cleanly on SIGTERM" "[ $code = 0 ] && logged $tmp/b3.log 'OK stopped'"
check "removing thelb0, and with it the VIP" "! ip -n $P-b3 link show thelb0 >/dev/null 2>&1"
check "and restoring arp_ignore and arp_announce" "[ \"\$(ip netns exec $P-b3 cat /proc/sys/net/ipv4/conf/all/arp_ignore /proc/sys/net/ipv4/conf/all/arp_announce | xargs)\" = '0 0' ]"
check "after telling the leader, which drained it: weight 0 at once" "weight_is $new b3 0 && logged $tmp/b3.log 'OK the leader drains this backend'"
check "admin remove b2: b2 stops, removed" "on $new $bin admin remove b2 >/dev/null && { ended b2; [ \$? = 1 ]; } && grep -q 'removed from the cluster' $tmp/b2.log"
check "its destination leaves IPVS" "within 10 eval '! ipvs_on $new | grep -q \"${addr4[b2]}:80\"'"
check "admin unassign web b1" "on $new $bin admin unassign web b1 | grep -q 'unassigned: b1 from web' && within 10 eval '! ipvs_on $new | grep -q \"${addr4[b1]}:80\"'"
check "admin service remove web: the VIP is given up, the IPVS service removed" "on $new $bin admin service remove web >/dev/null && within 10 eval '! holds $new $SVC && ! ipvs_on $new | grep -q $SVC'"
for n in b1 d1 d2 d3; do [ -n "${pid[$n]:-}" ] && stop $n; done
for n in b1 b2 b3; do app $n down; done

if [ $have6 = 1 ]; then
	echo "== IPv6: the control VIP $VIP6"
	start d1 "$tmp/d1.v6.log" --vip $VIP6 --bootstrap
	check "d1 takes the control VIP" "within 30 logged $tmp/d1.v6.log 'OK control VIP taken'"
	check "as $VIP6/128" "ip -n $P-d1 -o addr show dev eth0 | grep -qF ' $VIP6/128'"
	for n in d2 d3; do start $n "$tmp/$n.v6.log" --vip $VIP6; done
	check "d2 and d3 wait for approval" "within 20 both_logged $tmp/d2.v6.log $tmp/d3.v6.log 'OK waiting for approval'"
	check "and join once approved" "on d1 $bin admin approve d2 d3 >/dev/null && within 20 both_logged $tmp/d2.v6.log $tmp/d3.v6.log 'OK director ready'"
	check "three up" "within 20 up_is d1 3"
	check "the client reaches the control VIP" "reach [$VIP6]"
	stop d1 KILL
	ip -n $P-d1 link set eth0 down
	check "the leader crashes: another takes the VIP" "within 20 held_not_by $VIP6 d1"
	new=$(holder $VIP6)
	check "its neighbor advertisement moved the client's entry to $new's MAC address" "within 5 neigh_is $VIP6 ${mac[$new]}"
	for n in d2 d3; do stop $n; done
else
	echo "== IPv6: skipped - this kernel has no IPv6 (booted with ipv6.disable=1?)"
fi

echo "== nothing secret in any log, nothing left behind"
key="$(cut -d: -f2 "$tmp/d1/director.secret")"
bkey="$(cut -d: -f2 "$tmp/d1/backend.secret")"
check "no secret in any director's log" "! grep -qF -e \"$key\" -e \"$bkey\" $tmp/*.log"
check "no director of the lab is left running" "! pgrep -f '^$bin ' >/dev/null"

echo
if [ "$failures" -gt 0 ]; then
	echo "e2e: $failures check(s) failed"
	exit 1
fi
echo "e2e: all checks passed"
