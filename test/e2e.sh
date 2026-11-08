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
	for n in "${!pid[@]}"; do kill -9 "${pid[$n]}" 2>/dev/null; done
	wait 2>/dev/null
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
for n in d1 d2 d3 d4 c; do
	case $n in c) i=100 ;; *) i=$((10 + ${n#d})) ;; esac
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
echo "   $NET4.0/24, control VIP $VIP4 port $PORT; directors d1-d4 at $NET4.11-14, the client at $NET4.100"

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
	local t=$1
	shift
	for _ in $(seq 1 $((t * 10))); do
		"$@" && return 0
		sleep 0.1
	done
	return 1
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
check "/run/thelb is root's, mode 0700" "[ \"\$(on d1 stat -c '%U %a' /run/thelb)\" = 'root 700' ]"
check "the socket is root's, mode 0600" "[ \"\$(on d1 stat -c '%U %a' /run/thelb/thelb.sock)\" = 'root 600' ]"
on d1 "$bin" status >"$tmp/status" 2>&1
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
check "the socket refuses a user's process" "! on d1 runuser -u nobody -- python3 -c 'import socket; s=socket.socket(socket.AF_UNIX); s.connect(\"/run/thelb/thelb.sock\")' 2>/dev/null"

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
ip -n $P-d1 link set eth0 up
start d1 "$tmp/d1.rejoin.log" --vip $VIP4 --bootstrap
check "it removes the control VIP its crash left" "within 10 logged $tmp/d1.rejoin.log 'WARN removed the control VIP an earlier run left vip=$VIP4 interface=eth0'"
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
ip -n $P-$cut link set eth0 up
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
