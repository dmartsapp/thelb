#!/usr/bin/env bash
# Scale, as root, on Linux: N services - each on a VIP of its own, served by
# three backends - on three directors, in network namespaces as test/e2e.sh
# builds them. It prints what it costs: the time to create the services and to
# hold their VIPs, the leader's memory and CPU at rest, the address events the
# VIPs' leases make, the time `thelb status` takes, and a failover - the leader
# killed, its machine up - until another holds every VIP again.
#
#   sudo test/scale.sh bin/scale/thelb [N]       (make scale N=1000)

set -u
bin="$(readlink -f "${1:?usage: test/scale.sh <thelb binary> [N]}")"
N=${2:-1000}
[ "$(id -u)" = 0 ] || { echo "scale: run as root"; exit 1; }
[ "$N" -ge 1 ] && [ "$N" -le 16000 ] || { echo "scale: N is 1 to 16000"; exit 1; }

P=thelb-scale
SW=$P-sw
NET=198.18 # a /16 of RFC 2544's benchmarking network: room for the VIPs
VIP=$NET.0.10
PORT=7443
tmp=$(mktemp -d /tmp/thelb-scale.XXXXXX)
chmod 0711 "$tmp"
declare -A pid addr
failures=0

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
	if [ -n "${KEEP:-}" ] || [ "$failures" -gt 0 ]; then echo "scale: the logs are in $tmp"; else rm -rf "$tmp"; fi
}
trap cleanup EXIT

fail() { echo "  FAIL  $1"; failures=$((failures + 1)); }
now() { date +%s.%N; }
since() { echo "$(now) - $1" | bc | xargs printf '%.1f'; }
on() {
	local n=$1
	shift
	nsenter -t "${pid[$n]}" -m -n "$@"
}
within() {
	local end=$((SECONDS + $1))
	shift
	while [ "$SECONDS" -lt "$end" ]; do
		"$@" && return 0
		sleep 0.5
	done
	"$@"
}
logged() { grep -qE -- "$2" "$1" 2>/dev/null; }
# vip <i>: the i-th service's VIP, from 198.18.16.0 on.
vip() { echo "$NET.$((16 + $1 / 256)).$(($1 % 256))"; }
# held <node>: how many service VIPs the node holds on eth0.
held() { ip -n $P-$1 -o addr show dev eth0 | grep -c " $NET\.\(1[6-9]\|[2-9][0-9]\|1[0-9][0-9]\|2[0-5][0-9]\)\.[0-9]*/32"; }
# serving <node>: how many IPVS destinations the node has at weight 1.
serving() { ip netns exec $P-$1 ipvsadm -Ln 2>/dev/null | grep -cE -- '-> .*:80 +Route +1 '; }
all_held() { [ "$(held "$1")" -ge "$N" ] && [ "$(serving "$1")" -ge $((3 * N)) ]; }
thelb_pid() {
	local p
	for p in $(ip netns pids $P-$1); do
		[ "$(cat /proc/$p/comm 2>/dev/null)" = thelb ] && echo "$p" && return
	done
}
cpu_ticks() { awk '{print $14 + $15}' /proc/$1/stat; }
rss_mb() { awk '/^VmRSS/ {printf "%.0f", $2 / 1024}' /proc/$1/status; }
# cpu_percent <pid> <seconds>: its CPU use over that long.
cpu_percent() {
	local a b
	a=$(cpu_ticks "$1")
	sleep "$2"
	b=$(cpu_ticks "$1")
	echo "($b - $a) * 100 / ($(getconf CLK_TCK) * $2)" | bc
}
# batch <command...>: run the commands from stdin, one per line, 16 at a time.
batch() { xargs -P 16 -I{} sh -c '{}' >/dev/null 2>>"$tmp/batch.err"; }

echo "== scale: $N services, each its own VIP, three backends each, three directors"
ip netns add $SW
ip -n $SW link add br0 type bridge mcast_snooping 0
ip -n $SW link set br0 up
for n in d1 d2 d3 b1 b2 b3 c; do
	case $n in c) i=100 ;; b*) i=$((30 + ${n#b})) ;; *) i=$((10 + ${n#d})) ;; esac
	addr[$n]=$NET.0.$i
	ip netns add $P-$n
	ip link add eth0 netns $P-$n type veth peer name $n netns $SW
	ip -n $SW link set $n master br0 up
	ip -n $P-$n link set lo up
	ip -n $P-$n link set eth0 up
	ip -n $P-$n addr add ${addr[$n]}/16 dev eth0
	install -d -m 0700 "$tmp/$n"
done

start() {
	local n=$1
	shift
	ip netns exec $P-$n unshare --mount --propagation private \
		sh -c 'mount -t tmpfs -o mode=0755 tmpfs /run && exec "$@"' sh \
		"$bin" director --port $PORT --name $n --cluster scale --vip $VIP \
		--secret-file "$tmp/$n/director.secret" --backend-secret-file "$tmp/$n/backend.secret" "$@" >>"$tmp/$n.log" 2>&1 &
	pid[$n]=$!
}
startb() {
	local n=$1
	ip netns exec $P-$n unshare --mount --propagation private \
		sh -c 'mount -t tmpfs -o mode=0755 tmpfs /run && exec "$@"' sh \
		"$bin" backend --join $VIP:$PORT --name $n --secret-file "$tmp/$n/backend.secret" --offer web=tcp/80 >>"$tmp/$n.log" 2>&1 &
	pid[$n]=$!
}

start d1 --bootstrap
within 30 logged "$tmp/d1.log" 'OK control VIP taken' || fail "d1 creates the cluster"
for n in d2 d3 b1 b2 b3; do cp -p "$tmp/d1/director.secret" "$tmp/d1/backend.secret" "$tmp/$n/"; done
start d2
start d3
within 30 eval "logged $tmp/d2.log 'waiting for approval' && logged $tmp/d3.log 'waiting for approval'" || fail "d2 and d3 ask to join"
on d1 "$bin" admin approve --match 'd[23]' --yes >/dev/null
within 30 eval "logged $tmp/d2.log 'director ready' && logged $tmp/d3.log 'director ready'" || fail "d2 and d3 join"

# Each backend: a server that only accepts, for the checks, and theLB.
cat >"$tmp/accept.py" <<'PY'
import socket
s = socket.socket()
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("0.0.0.0", 80))
s.listen(4096)
while True:
    c, _ = s.accept()
    c.close()
PY
for n in b1 b2 b3; do
	ip netns exec $P-$n python3 "$tmp/accept.py" &
	startb $n
done
within 30 eval "logged $tmp/b1.log 'waiting for approval' && logged $tmp/b2.log 'waiting for approval' && logged $tmp/b3.log 'waiting for approval'" || fail "the backends ask to join"
on d1 "$bin" admin approve --match 'b[1-3]' --yes >/dev/null
within 30 eval "logged $tmp/b1.log 'backend ready' && logged $tmp/b2.log 'backend ready' && logged $tmp/b3.log 'backend ready'" || fail "the backends join"
within 30 eval "[ \$(on d1 $bin status offers 2>/dev/null | grep -cE '^ b[1-3] +\| web ') = 3 ]" || fail "the backends offer web"
on d1 "$bin" admin pool add big --vip $NET.16.0/16 --vippool-count "$N" >/dev/null || fail "the pool"

t=$(now)
for i in $(seq 0 $((N - 1))); do
	echo "nsenter -t ${pid[d1]} -m -n $bin admin service add s$i --pool big --vip $(vip $i) --proto tcp --port 80"
done | batch
create=$(since "$t")
made=$(on d1 "$bin" status --json 2>/dev/null | python3 -c 'import json,sys; print(len(json.load(sys.stdin).get("services") or []))')
[ "$made" = "$N" ] || fail "$N services created: $made"

t=$(now)
for i in $(seq 0 $((N - 1))); do
	echo "nsenter -t ${pid[d1]} -m -n $bin admin assign s$i b1:web b2:web b3:web"
done | batch
assign=$(since "$t")

t=$(now)
within 600 all_held d1 || fail "d1 holds every VIP, with IPVS: $(held d1) VIPs, $(serving d1) destinations"
converge=$(since "$t")

leader=$(thelb_pid d1)
backend=$(thelb_pid b1)
sleep 10
# A build with -tags profile writes the leader's CPU profile over the 30 s.
[ -n "${PROFILE:-}" ] && kill -USR1 "$leader"
cpu=$(cpu_percent "$leader" 30)
if [ -n "${PROFILE:-}" ]; then
	sleep 2
	cp "/tmp/thelb-$leader.cpu" "$PROFILE" 2>/dev/null && chown "${SUDO_UID:-0}:${SUDO_GID:-0}" "$PROFILE"
	rm -f "/tmp/thelb-$leader.cpu"
fi
bcpu=$(cpu_percent "$backend" 10)
rss=$(rss_mb "$leader")
brss=$(rss_mb "$backend")
events=$(timeout 10 ip -n $P-d1 monitor address 2>/dev/null | grep -cE '^[0-9]+: ')
t=$(now)
on d1 "$bin" status >/dev/null
status=$(since "$t")
t=$(now)
on d1 "$bin" status all >/dev/null
statusall=$(since "$t")
probe=$(ip netns exec $P-c timeout 3 bash -c "exec 3<>/dev/tcp/$(vip $((N - 1)))/80" 2>/dev/null && echo yes || echo no)
[ "$probe" = yes ] || fail "the client reaches the last service"

t=$(now)
kill -9 "$(thelb_pid d1)"
new=""
for _ in $(seq 1 1200); do
	for n in d2 d3; do [ "$(held $n)" -gt 0 ] && new=$n; done
	[ -n "$new" ] && break
	sleep 0.1
done
first=$(since "$t")
within 300 all_held "${new:-d2}" || fail "${new:-a director} holds every VIP again: $(held "${new:-d2}") VIPs, $(serving "${new:-d2}") destinations"
failover=$(since "$t")
lapsed=$(held d1)

echo
printf '  %-58s %s\n' "services created ($N, 16 at a time)" "${create} s"
printf '  %-58s %s\n' "assignments made ($((3 * N)))" "${assign} s"
printf '  %-58s %s\n' "until the leader holds every VIP, IPVS programmed" "${converge} s"
printf '  %-58s %s\n' "leader at rest: CPU, memory" "${cpu}% of a CPU, ${rss} MB"
printf '  %-58s %s\n' "a backend at rest: CPU, memory ($N VIPs on thelb0)" "${bcpu}% of a CPU, ${brss} MB"
printf '  %-58s %s\n' "address events on the leader (lease renewals)" "$((events / 10)) a second"
printf '  %-58s %s\n' "thelb status, thelb status all" "${status} s, ${statusall} s"
printf '  %-58s %s\n' "failover, the leader killed: first VIP, every VIP ($new)" "${first} s, ${failover} s"
printf '  %-58s %s\n' "the killed leader's VIPs left on its interface after that" "$lapsed"
printf '  %-58s %s\n' "the machine" "$(nproc) CPU, $(uname -r)"
[ -s "$tmp/batch.err" ] && { echo "  errors from the admin commands:"; sort "$tmp/batch.err" | uniq -c | sort -rn | head -5 | sed 's/^/    /'; }
echo
if [ "$failures" -gt 0 ]; then
	echo "scale: $failures check(s) failed"
	exit 1
fi
echo "scale: done"
