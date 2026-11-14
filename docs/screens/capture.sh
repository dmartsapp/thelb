#!/bin/bash
# The documentation's screenshots: the real output of a theLB binary, in the
# lab (test/lab.sh), saved as docs/screens/<name>.txt. docs/build.py draws each
# as a terminal window - docs/assets/screens/<name>.svg - for the pages to show.
# Nothing in them is typed by hand: to change one, capture them all again.
#
#   make screens                        on this Linux machine, as root, with the lab down
#   make remote-screens REMOTE=<host>   on another, over ssh; the files come back here
#
# A file is the window's title on its first line, then each command - "$ ..." -
# and what it printed, or lines of a node's log as they are.
set -eu
cd "$(dirname "$0")/../.."
[ "$(id -u)" = 0 ] || { echo "capture: run as root" >&2; exit 1; }
B=$(readlink -f "${1:?capture.sh <thelb binary>}")
LAB=test/lab.sh
STATE=/tmp/thelb-lab
OUT=docs/screens
NET=198.18.0
METRICS=$NET.11:9100

die() { echo "capture: $*" >&2; exit 1; }
ip netns list | grep -q '^thelb-lab-' && die "the lab is up: sudo $LAB down first"
trap '$LAB down >/dev/null 2>&1 || true' EXIT

cur=
# begin <name> <title>: a new screenshot.
begin() { cur="$OUT/$1.txt"; printf '%s\n' "$2" >"$cur"; }
# show <word...>: the command line, as typed - quoted where a shell needs it.
show() {
	local s="" a
	for a in "$@"; do
		if [[ $a =~ ^[][A-Za-z0-9_./:=,@+%-]+$ ]]; then s+=" $a"; else s+=" '$a'"; fi
	done
	printf '$%s\n' "$s" >>"$cur"
}
# t <node> <args...>: "sudo thelb <args>" on a node, and what it printed.
t() { local n=$1; shift; show sudo thelb "$@"; $LAB run "$n" thelb "$@" >>"$cur" 2>&1 || true; }
# x <node> <command...>: a command on a node, and what it printed; r does the same as root.
x() { local n=$1; shift; show "$@"; $LAB run "$n" "$@" >>"$cur" 2>&1 || true; }
r() { local n=$1; shift; show sudo "$@"; $LAB run "$n" "$@" >>"$cur" 2>&1 || true; }
# lab <args...>: a command of the lab's own.
lab() { show sudo test/lab.sh "$@"; $LAB "$@" >>"$cur" 2>&1 || true; }
# logs <node> <from> [<regex> [<n>]]: a node's log after line <from>, the lines
# matching <regex>, the first <n> of them.
logs() { tail -n +$(($2 + 1)) "$STATE/$1.log" | grep -E -- "${3:-.}" | head -n "${4:-40}" >>"$cur" || true; }
# upto <node> <from> <regex>: a node's log after line <from>, up to the line matching <regex>.
upto() { tail -n +$(($2 + 1)) "$STATE/$1.log" | awk -v re="$3" '{ print } $0 ~ re { exit }' >>"$cur"; }
lines() { wc -l <"$STATE/$1.log"; }
waitfor() { # waitfor <node> <from> <regex>
	for _ in $(seq 1 200); do
		tail -n +$(($2 + 1)) "$STATE/$1.log" 2>/dev/null | grep -qE -- "$3" && return 0
		sleep 0.2
	done
	die "timed out waiting for \"$3\" in $1's log"
}
# wait_offers: until the cluster holds the backends' offers - each is offered
# once its backend's own check passes, a few seconds after it is ready.
wait_offers() {
	for _ in $(seq 1 150); do
		[ "$($LAB run d1 thelb status offers 2>/dev/null | grep -cE '^ b[1-4] ')" -ge 7 ] && return 0
		sleep 0.2
	done
	die "the backends' offers did not arrive"
}
leader() {
	local n best="" t bt=""
	for n in d1 d2 d3; do
		[ -e "$STATE/$n.pid" ] && kill -0 "$(cat "$STATE/$n.pid")" 2>/dev/null || continue
		t=$(grep -h "OK leader node=" "$STATE/$n.log" 2>/dev/null | tail -1 | cut -c1-24)
		if [ -n "$t" ] && { [ -z "$bt" ] || [[ "$t" > "$bt" ]]; }; then best=$n bt=$t; fi
	done
	echo "${best:-d1}"
}

install -d "$OUT"
rm -f "$OUT"/*.txt

echo "capture: the lab, bare, d1 with --debug and --metrics"
BARE=1 D1_FLAGS="--debug --metrics $METRICS" $LAB up "$B" >/dev/null

begin version "d1"
show thelb --version
"$B" --version >>"$cur"

begin help "d1"
show thelb --help
"$B" --help >>"$cur"

begin director-start "d1 - the first director's log"
upto d1 0 "OK leader node="

begin director-join "d2 - a second director's log"
upto d2 0 "OK director ready"

begin backend-join "b1 - a backend's log"
upto b1 0 "OK backend ready"

echo "capture: a pool, services, assignments"
wait_offers
begin services "d1"
t d1 admin pool add lab --vip $NET.20/24 --vippool-count 10
t d1 admin service add web --pool lab --proto tcp --port 80
t d1 status offers
t d1 admin assign web b1:tcp/80 b2:tcp/80 b3:tcp/80

begin port-mapping "d1"
t d1 admin service add mapped --pool lab --proto tcp --port 80
t d1 admin assign mapped b1:tcp/8080 b2:tcp/9090 b3:tcp/7070

begin tunnel "d1"
t d1 admin service add far --pool lab --proto tcp --port 80 --method tunnel
t d1 admin assign far b4:tcp/80 b1:tcp/80

for n in b1 b2 b3; do
	waitfor d1 0 "OK backend serving service=web node=$n"
	waitfor d1 0 "OK backend serving service=mapped node=$n"
done
waitfor d1 0 "OK backend serving service=far node=b4"
waitfor d1 0 "OK backend serving service=far node=b1"
sleep 3

echo "capture: traffic, status"
begin curl "lab"
lab curl 12
lab curl 9 $NET.21
lab curl 8 $NET.22
# The traffic counts lag: IPVS sums its per-CPU counters every 2 seconds.
for _ in $(seq 1 30); do
	$LAB run c curl -s "http://$METRICS/metrics" | grep -qE '^thelb_service_connections_total\{service="far",vip="[^"]*"\} 8$' && break
	sleep 1
done

begin status "d1"
t d1 status
begin status-directors "d1"
t d1 status directors
begin status-backends "d1"
t d1 status backends
begin status-vips "d1"
t d1 status vips
begin status-offers "d1"
t d1 status offers
begin status-b1 "b1"
t b1 status

begin ipvsadm "d1"
r d1 ipvsadm -Ln
begin lease "d1"
x d1 ip -4 address show dev eth0
begin thelb0 "b1"
x b1 ip address show thelb0
begin thelbt0 "b4"
x b4 ip address show thelbt0
begin nft "b2"
r b2 nft list table inet thelb

begin debug "d1 - the leader's log, with --debug"
logs d1 0 "DEBUG connection forwarded" 6

begin metrics "lab"
show curl -s "http://$METRICS/metrics" '|' grep -E '^thelb_(leader|vip_held|service_)'
$LAB run c sh -c "curl -s http://$METRICS/metrics | grep -E '^thelb_(leader|vip_held|service_)'" >>"$cur" 2>&1 || true
# show quoted the pipe: print it bare.
sed -i "s/ '|' / | /" "$cur"

echo "capture: offers, approval"
ip netns exec thelb-lab-b1 python3 -m http.server 5000 --bind 0.0.0.0 --directory "$STATE/b1/www" >/dev/null 2>&1 &
shop=$!
sleep 1
begin offer "b1"
t b1 offer add shop tcp/5001,check=http:/
t b1 offer add shop tcp/5000,check=http:/
t b1 offer remove shop
kill $shop 2>/dev/null || true

$LAB app d2-web up
$LAB start d2-web >/dev/null
waitfor d2-web 0 "OK waiting for approval"
begin pending-log "d2-web - a new backend's log"
upto d2-web 0 "OK waiting for approval"
begin approve "d1"
t d1 status pending
t d1 admin approve d2-web
waitfor d2-web 0 "OK backend ready"
for _ in $(seq 1 100); do
	$LAB run d1 thelb status offers 2>/dev/null | grep -qE '^ d2-web ' && break
	sleep 0.2
done
t d1 admin assign web d2-web:tcp/80

echo "capture: failures"
from=$(lines d1)
$LAB app b2 down
waitfor d1 "$from" "WARN backend not serving.*service=mapped node=b2"
waitfor d1 "$from" "WARN backend not serving.*service=web node=b2"
$LAB app b2 up
waitfor d1 "$from" "OK backend serving service=web node=b2"
waitfor d1 "$from" "OK backend serving service=mapped node=b2"
begin app-down "d1 - the leader's log"
logs d1 "$from" "backend (not )?serving.*node=b2"

from=$(lines d1)
$LAB run d1 ipvsadm -C
waitfor d1 "$from" "drift repaired"
sleep 1
begin drift "d1 - the leader's log"
logs d1 "$from" "drift repaired" 8

begin generate-commands "d1"
t d1 generate commands

begin generate-systemd "d1"
show sudo thelb generate systemd director --vip $NET.10 --port 7443 --bootstrap \
	--secret-file /etc/thelb/director.secret --backend-secret-file /etc/thelb/backend.secret
# In a mount namespace of its own, with the binary at /usr/local/bin/thelb -
# where a unit should run it from - and nothing changed on this machine.
nsenter -t "$(cat "$STATE/d1.pid")" -n unshare -m --propagation private sh -c '
	mount -t tmpfs -o mode=0755 tmpfs /usr/local/bin && cp "$1" /usr/local/bin/thelb && shift &&
	exec /usr/local/bin/thelb "$@"' sh "$B" generate systemd director --vip $NET.10 --port 7443 --bootstrap \
	--secret-file /etc/thelb/director.secret --backend-secret-file /etc/thelb/backend.secret >>"$cur" 2>&1 || true

begin doctor "d2"
t d2 doctor

echo "capture: the leader crashes"
old=$(leader)
declare -A at
for n in d1 d2 d3; do at[$n]=$(lines $n); done
$LAB kill "$old" >/dev/null
new=
for _ in $(seq 1 150); do
	for n in d1 d2 d3; do
		[ "$n" = "$old" ] && continue
		tail -n +$((${at[$n]} + 1)) "$STATE/$n.log" | grep -q "OK control VIP taken" && new=$n
	done
	[ -n "$new" ] && break
	sleep 0.2
done
[ -n "$new" ] || die "no director took the control VIP after $old was killed"
waitfor "$new" "${at[$new]}" "director gone: out of the vote"
begin failover "$new - the new leader's log, after $old was killed"
logs "$new" "${at[$new]}" "leader|control VIP|director gone|lease|takes over" 14
begin failover-status "$new"
t "$new" status directors

back=$(lines "$old")
$LAB start "$old" >/dev/null
waitfor "$old" "$back" "OK director ready"
begin rejoin "$old - started again"
upto "$old" "$back" "OK director ready"

if [ -n "${SUDO_UID:-}" ]; then chown "$SUDO_UID:${SUDO_GID:-$SUDO_UID}" "$OUT" "$OUT"/*.txt; fi
echo "capture: $(ls "$OUT"/*.txt | wc -l) screenshots in $OUT"
