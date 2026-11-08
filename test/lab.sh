#!/bin/bash
# theLB's lab on one machine, to try by hand: a switch, three directors, three
# backends each running two small web servers - one on port 80, one on a port
# of its own (8080, 9090, 7070) - and a client - each a network namespace
# of its own, on 198.18.0.0/24 (RFC 2544's benchmarking network), so nothing
# reaches this machine's own network. Each theLB has its own /run, as if it ran
# on a machine of its own.
#
#   sudo test/lab.sh up [bin/thelb]     build the lab: a cluster, a pool, a service, three backends
#   sudo test/lab.sh run <node> <cmd>   run a command on a node: d1-d3, b1-b3, or c (the client);
#                                       "thelb" is the lab's binary - run d1 thelb status
#   sudo test/lab.sh shell <node>       a shell on a node
#   sudo test/lab.sh curl [n] [vip]     the client fetches a service n times (10), and counts who answered:
#                                       198.18.0.20 (web, port 80 everywhere) or 198.18.0.21 (mapped)
#   sudo test/lab.sh kill <node>        crash a node's theLB (SIGKILL)
#   sudo test/lab.sh stop <node>        stop it (SIGTERM)
#   sudo test/lab.sh start <node>       start it again, as it was started
#   sudo test/lab.sh app <b1-b3> down|up   stop or start a backend's web servers
#   sudo test/lab.sh logs <node>        follow a node's log
#   sudo test/lab.sh down               take it all away

set -u
P=thelb-lab
SW=$P-sw
NET=198.18.0
VIP=$NET.10
PORT=7443
STATE=/tmp/thelb-lab
DIRECTORS="d1 d2 d3"
BACKENDS="b1 b2 b3"

die() { echo "lab: $*" >&2; exit 1; }
[ "$(id -u)" = 0 ] || die "run as root: sudo $0 $*"

addr() {
	case $1 in
	c) echo $NET.100 ;;
	d*) echo $NET.$((10 + ${1#d})) ;;
	b*) echo $NET.$((30 + ${1#b})) ;;
	*) die "no node $1: d1-d3, b1-b3, c" ;;
	esac
}
pidof_node() { cat "$STATE/$1.pid" 2>/dev/null; }
# altport is a backend's own port for the mapped service: nginx on 8080 here,
# 9090 there - and the service on 80.
altport() {
	case $1 in b1) echo 8080 ;; b2) echo 9090 ;; b3) echo 7070 ;; esac
}
alive() { local p; p=$(pidof_node "$1") && kill -0 "$p" 2>/dev/null; }
bin() { cat "$STATE/bin.path"; }

# start_node <node>: start a node's theLB with a /run of its own.
start_node() {
	local n=$1 b
	b=$(bin)
	alive "$n" && die "$n runs already"
	local args
	case $n in
	d1) args="director --vip $VIP --port $PORT --bootstrap --cluster lab" ;;
	d*) args="director --vip $VIP --port $PORT --cluster lab" ;;
	b*) args="backend --join $VIP:$PORT --offer tcp/80,check=http:/ --offer tcp/$(altport $n),check=http:/" ;;
	esac
	local secrets="--secret-file $STATE/$n/director.secret --backend-secret-file $STATE/$n/backend.secret"
	[ "${n#b}" != "$n" ] && secrets="--secret-file $STATE/$n/backend.secret"
	# shellcheck disable=SC2086
	ip netns exec $P-$n unshare --mount --propagation private \
		sh -c 'mount -t tmpfs -o mode=0755 tmpfs /run && exec "$@"' sh \
		"$b" $args --name $n $secrets >>"$STATE/$n.log" 2>&1 &
	echo $! >"$STATE/$n.pid"
}

waitfor() {
	local file=$1 pattern=$2
	for _ in $(seq 1 150); do
		grep -qE -- "$pattern" "$file" 2>/dev/null && return 0
		sleep 0.2
	done
	die "timed out waiting for \"$pattern\" in $file"
}

on() {
	local n=$1
	shift
	if [ "$n" = c ]; then
		ip netns exec $P-c "$@"
		return
	fi
	local p
	p=$(pidof_node "$n") || die "$n has no theLB running: sudo $0 start $n"
	kill -0 "$p" 2>/dev/null || die "$n's theLB is not running: sudo $0 start $n"
	nsenter -t "$p" -m -n "$@"
}

app() {
	local n=$1 what=$2
	case $what in
	up)
		[ -e "$STATE/$n.app.pid" ] && kill -0 "$(cat "$STATE/$n.app.pid")" 2>/dev/null && return
		ip netns exec $P-$n python3 -m http.server 80 --bind 0.0.0.0 --directory "$STATE/$n/www" >"$STATE/$n.app.log" 2>&1 &
		echo $! >"$STATE/$n.app.pid"
		ip netns exec $P-$n python3 -m http.server "$(altport $n)" --bind 0.0.0.0 --directory "$STATE/$n/www-alt" >"$STATE/$n.alt.log" 2>&1 &
		echo $! >"$STATE/$n.alt.pid"
		;;
	down)
		for f in "$STATE/$n.app.pid" "$STATE/$n.alt.pid"; do
			[ -e "$f" ] && kill "$(cat "$f")" 2>/dev/null
			rm -f "$f"
		done
		;;
	*) die "app <b1-b3> up|down" ;;
	esac
}

up() {
	local b=${1:-}
	[ -n "$b" ] || b="$(dirname "$0")/../bin/thelb"
	b="$(readlink -f "$b")"
	[ -x "$b" ] || die "no binary at $b: make build-lab, or pass its path"
	ip netns list | grep -q "^$P-" && die "the lab is up already: sudo $0 down first"
	install -d -m 0711 "$STATE"
	install -m 0755 "$b" "$STATE/thelb"
	echo "$STATE/thelb" >"$STATE/bin.path"

	echo "== the network: a switch, three directors, three backends, a client"
	ip netns add $SW
	ip -n $SW link add br0 type bridge mcast_snooping 0
	ip -n $SW link set br0 up
	for n in $DIRECTORS $BACKENDS c; do
		ip netns add $P-$n
		ip link add eth0 netns $P-$n type veth peer name $n netns $SW
		ip -n $SW link set $n master br0 up
		ip -n $P-$n link set lo up
		ip -n $P-$n link set eth0 up
		ip -n $P-$n addr add "$(addr $n)/24" dev eth0
		install -d -m 0700 "$STATE/$n"
	done

	echo "== d1 bootstraps the cluster; d2 and d3 join, and are approved"
	start_node d1
	waitfor "$STATE/d1.log" "OK control VIP taken"
	for n in d2 d3 $BACKENDS; do
		[ "${n#d}" != "$n" ] && cp -p "$STATE/d1/director.secret" "$STATE/$n/"
		cp -p "$STATE/d1/backend.secret" "$STATE/$n/"
	done
	for n in d2 d3; do start_node $n; done
	for n in d2 d3; do waitfor "$STATE/$n.log" "OK waiting for approval"; done
	on d1 "$(bin)" admin approve --match 'd[23]' --yes >/dev/null
	for n in d2 d3; do waitfor "$STATE/$n.log" "OK director ready"; done

	echo "== b1-b3 serve a web page, enrol as backends, and are approved"
	for n in $BACKENDS; do
		install -d -m 0755 "$STATE/$n/www" "$STATE/$n/www-alt"
		echo "hello from $n" >"$STATE/$n/www/index.html"
		echo "hello from $n, on port $(altport $n)" >"$STATE/$n/www-alt/index.html"
		app $n up
		start_node $n
	done
	for n in $BACKENDS; do waitfor "$STATE/$n.log" "OK waiting for approval"; done
	on d1 "$(bin)" admin approve --match 'b[1-3]' --yes >/dev/null
	for n in $BACKENDS; do waitfor "$STATE/$n.log" "OK backend ready"; done

	echo "== a pool, a service on it, and the backends' offers assigned"
	on d1 "$(bin)" admin pool add lab --vip $NET.20/24 --vippool-count 10
	on d1 "$(bin)" admin service add web --pool lab --proto tcp --port 80
	on d1 "$(bin)" admin assign web b1:tcp/80 b2:tcp/80 b3:tcp/80
	echo "== a second service, on port 80, served from each backend's own port: 8080, 9090, 7070"
	on d1 "$(bin)" admin service add mapped --pool lab --proto tcp --port 80
	on d1 "$(bin)" admin assign mapped b1:tcp/8080 b2:tcp/9090 b3:tcp/7070
	for n in $BACKENDS; do
		waitfor "$STATE/$(leader).log" "OK backend serving service=web node=$n"
		waitfor "$STATE/$(leader).log" "OK backend serving service=mapped node=$n"
	done

	cat <<EOF

The lab is up. Two services, each served by b1, b2 and b3:
  web     $NET.20:80 - from port 80 on each backend
  mapped  $NET.21:80 - from 8080 on b1, 9090 on b2, 7070 on b3: each backend maps the port

  sudo $0 curl 12                 the client fetches web 12 times: who answered
  sudo $0 curl 9 $NET.21     the same for mapped
  sudo $0 run b2 nft list table inet thelb   the port mapping on b2
  sudo $0 run d1 thelb status     the summary, on d1; then: directors, backends, vips, all
  sudo $0 run d2 thelb status backends
  sudo $0 run b1 thelb status     a backend's own view
  sudo $0 run $(leader) ipvsadm -Ln  what theLB programmed in IPVS, on the leader
  sudo $0 run b1 ip address show thelb0

Things to try:
  sudo $0 app b2 down             b2's web server stops: b2 gets weight 0
  sudo $0 app b2 up               and back
  sudo $0 kill $(leader)               the leader crashes: another director takes the VIPs
  sudo $0 start $(leader)              it comes back, and rejoins without approval
  sudo $0 stop b3                 b3 stops: it gives its VIP up; its weight goes to 0
  sudo $0 run d1 thelb admin unassign web b1
  sudo $0 logs d1

  sudo $0 down                    takes it all away
EOF
}

# leader is the director whose log last said it leads.
leader() {
	local n best="" t bt=""
	for n in $DIRECTORS; do
		alive $n || continue
		t=$(grep -h "OK leader node=" "$STATE/$n.log" 2>/dev/null | tail -1 | cut -c1-24)
		if [ -n "$t" ] && { [ -z "$bt" ] || [[ "$t" > "$bt" ]]; }; then best=$n bt=$t; fi
	done
	echo "${best:-d1}"
}

down() {
	for f in "$STATE"/*.pid; do
		[ -e "$f" ] || continue
		kill -TERM "$(cat "$f")" 2>/dev/null
	done
	sleep 2
	for f in "$STATE"/*.pid; do
		[ -e "$f" ] || continue
		kill -KILL "$(cat "$f")" 2>/dev/null
	done
	for ns in $(ip netns list | awk '{print $1}'); do
		case $ns in $P-*) ip netns del "$ns" ;; esac
	done
	rm -rf "$STATE"
	echo "lab: down"
}

curls() {
	local n=${1:-10} vip=${2:-$NET.20} i out
	declare -A got
	for i in $(seq 1 "$n"); do
		out=$(ip netns exec $P-c curl -s --max-time 2 "http://$vip/" || echo "no answer")
		got[$out]=$((${got[$out]:-0} + 1))
	done
	for k in "${!got[@]}"; do printf '%4d  %s\n' "${got[$k]}" "$k"; done | sort -k3
}

cmd=${1:-}
shift || true
case $cmd in
up) up "$@" ;;
down) down ;;
run)
	n=${1:?run <node> <command>}
	shift
	[ "${1:-}" = thelb ] && { shift; set -- "$(bin)" "$@"; }
	on "$n" "$@"
	;;
shell) on "${1:?shell <node>}" bash ;;
curl) curls "$@" ;;
kill) kill -KILL "$(pidof_node "${1:?kill <node>}")" && echo "lab: $1's theLB killed" ;;
stop) kill -TERM "$(pidof_node "${1:?stop <node>}")" && echo "lab: $1's theLB stopped" ;;
start) start_node "${1:?start <node>}" && echo "lab: $1's theLB started; its log: sudo $0 logs $1" ;;
app) app "${1:?app <b1-b3> up|down}" "${2:?up or down}" ;;
logs) tail -n 40 -f "$STATE/${1:?logs <node>}.log" ;;
*) sed -n '2,23p' "$0" | sed 's/^# \{0,1\}//'; exit 2 ;;
esac
