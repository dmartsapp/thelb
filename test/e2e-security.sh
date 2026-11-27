#!/bin/bash
# Security, end to end, as root, on Linux: a cluster - two directors, a backend
# serving a service, a client - and an attacker, each in a network namespace of
# its own. The attacker (test/attack) goes at the control port, the join, a
# member gone bad, the metrics endpoint and the local socket; directors are
# started with hostile files, and against a Kubernetes API server that lies.
#
#   sudo test/e2e-security.sh bin/e2e/thelb bin/e2e/attack
#
# Each attack checks its own outcome (attack prints PASS or FAIL). Around them,
# this checks the cluster is unharmed: both directors alive, d1 still leading -
# no failover - the client still served through the VIP, and d1's file
# descriptors and memory back to what they were.

set -u
bin="$(readlink -f "${1:?usage: test/e2e-security.sh <thelb binary> <attack binary> [<blackbox test binary>]}")"
atk="$(readlink -f "${2:?usage: test/e2e-security.sh <thelb binary> <attack binary> [<blackbox test binary>]}")"
bbx="${3:+$(readlink -f "$3")}"
[ "$(id -u)" = 0 ] || { echo "e2e-security: run as root"; exit 1; }

P=thelb-sec
SW=$P-sw
NET=198.18.0
VIP=$NET.10
PORT=7443
tmp=$(mktemp -d /tmp/thelb-sec.XXXXXX)
chmod 0711 "$tmp"
failures=0
declare -A pid addr

pass() { printf '  ok    %s\n' "$1"; }
fail() { printf '  FAIL  %s\n' "$1"; failures=$((failures + 1)); }
info() { printf '  info  %s\n' "$1"; }
check() { if eval "$2"; then pass "$1"; else fail "$1"; fi; }
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
		echo "e2e-security: the logs are in $tmp"
	else
		rm -rf "$tmp"
	fi
}
trap cleanup EXIT
trap "exit 130" INT TERM HUP # so that cleanup runs then too

echo "== the lab: two directors, a backend, a client, an attacker"
ip netns add $SW
ip -n $SW link add br0 type bridge mcast_snooping 0
ip -n $SW link set br0 up
for n in d1 d2 b1 c x z; do
	case $n in d*) i=$((10 + ${n#d})) ;; b1) i=21 ;; c) i=100 ;; x) i=66 ;; z) i=77 ;; esac
	addr[$n]=$NET.$i
	ip netns add $P-$n
	ip link add eth0 netns $P-$n type veth peer name $n netns $SW
	ip -n $SW link set $n master br0 up
	ip -n $P-$n link set lo up
	ip -n $P-$n link set eth0 up
	ip -n $P-$n addr add ${addr[$n]}/24 dev eth0
	install -d -m 0700 "$tmp/$n"
done
echo "   $NET.0/24: d1 .11, d2 .12, b1 .21, the client .100, the attacker .66, a spare director's machine .77"

on() { local n=$1; shift; nsenter -t "${pid[$n]}" -m -n "$@"; }
# run <node> <log> <thelb arguments...>: theLB in the node's namespace, with a
# /run of its own.
run() {
	local n=$1 log=$2
	shift 2
	ip netns exec $P-$n unshare --mount --propagation private \
		sh -c 'mount -t tmpfs -o mode=0755 tmpfs /run && exec "$@"' sh "$bin" "$@" >>"$log" 2>&1 &
	pid[$n]=$!
}
director() { # node log [flags]
	local n=$1 log=$2
	shift 2
	run "$n" "$log" director --vip $VIP --port $PORT --name "$n" --cluster sec \
		--secret-file "$tmp/$n/director.secret" --backend-secret-file "$tmp/$n/backend.secret" "$@"
}
alive() { kill -0 "${pid[$1]}" 2>/dev/null; }
json() { on "$1" "$bin" status --json 2>/dev/null | python3 -c "import json, sys; s = json.load(sys.stdin); print($2)" 2>/dev/null; }
leader_of() { json "$1" "s['cluster']['leader'] if s['cluster'] else ''"; }
served() { [ "$(ip netns exec $P-c curl -s --max-time 2 http://$NET.50/)" = b1 ]; }
fds() { ls "/proc/${pid[d1]}/fd" 2>/dev/null | wc -l; }
rss_mb() { awk '/^VmRSS/ { print int($2 / 1024) }' "/proc/${pid[d1]}/status" 2>/dev/null; }
attack() { ip netns exec $P-x "$atk" "$@"; }
# attacked <what> <attack arguments...>: run an attack; its PASS and FAIL lines
# become checks.
attacked() {
	local what=$1 out
	shift
	out=$(attack "$@" 2>&1)
	while IFS= read -r line; do
		case $line in
		PASS*) pass "$what: ${line#PASS }" ;;
		FAIL*) fail "$what: ${line#FAIL }" ;;
		INFO*) info "$what: ${line#INFO }" ;;
		esac
	done <<<"$out"
}
# Conditions within retries: functions, so each try looks again.
d1_leads() { test "$(leader_of d1)" = d1 && test "$(leader_of d2)" = d1; }
fds_back() { test "$(fds)" -le $((FDS + 20)); }
# healthy <after what>: the cluster unharmed.
healthy() {
	check "after $1: d1 and d2 are up" "alive d1 && alive d2"
	check "after $1: d1 still leads - no failover" "within 10 d1_leads"
	check "after $1: the client is still served through the VIP" "within 10 served"
	local f r
	f=$(fds) r=$(rss_mb)
	check "after $1: d1's file descriptors back to what they were ($f, at first $FDS)" "within 20 fds_back"
	check "after $1: d1's memory bounded (${r} MB)" "test \"\$(rss_mb)\" -lt 300"
}

echo "== the cluster"
director d1 "$tmp/d1.log" --bootstrap --metrics ${addr[d1]}:9732
check "d1 creates the cluster and leads" "within 20 logged $tmp/d1.log 'OK control VIP taken'"
install -m 0600 "$tmp/d1/director.secret" "$tmp/d1/backend.secret" "$tmp/d2/"
install -m 0600 "$tmp/d1/backend.secret" "$tmp/b1/"
install -m 0600 "$tmp/d1/director.secret" "$tmp/d1/backend.secret" "$tmp/x/"
director d2 "$tmp/d2.log"
install -d -m 0755 "$tmp/b1/www"
echo b1 >"$tmp/b1/www/index.html"
ip netns exec $P-b1 python3 -m http.server 80 --bind 0.0.0.0 --directory "$tmp/b1/www" >/dev/null 2>&1 &
pid[b1-web]=$!
run b1 "$tmp/b1.log" backend --join $VIP:$PORT --name b1 --secret-file "$tmp/b1/backend.secret" --offer web=tcp/80
pending_has() { on d1 "$bin" status pending 2>/dev/null | grep -qw "$1"; }
within 20 pending_has d2 && within 20 pending_has b1
on d1 "$bin" admin approve d2 b1 >/dev/null
states() { json d1 "sorted((n['node'], n['state']) for n in s['nodes'])"; }
all_up() { test "$(states)" = "[('b1', 'up'), ('d1', 'up'), ('d2', 'up')]"; }
check "d2 and b1 join, approved" "within 20 all_up"
on d1 "$bin" admin pool add main --vip $NET.50/24 --vippool-count 4 --default >/dev/null
on d1 "$bin" admin service add web --proto tcp --port 80 >/dev/null
assign_web() { on d1 "$bin" admin assign web b1:web >/dev/null 2>&1; }
within 20 assign_web
check "the client is served through the VIP" "within 20 served"
sleep 2
FDS=$(fds)
info "d1 at rest: $FDS file descriptors, $(rss_mb) MB"

echo "== the control port, before any proof"
attacked "probe" probe $VIP:$PORT 2000
attacked "garbage" garbage $VIP:$PORT
attacked "garbage on d1's own address" garbage ${addr[d1]}:$PORT
attacked "slow" slow $VIP:$PORT
healthy "probes, garbage and slow handshakes"

echo "== a flood of connections that never speak"
attack tcpflood $VIP:$PORT 3000 12s >"$tmp/flood.out" 2>&1 &
flood=$!
sleep 4
check "during the flood: d1 still leads, and d2 sees it" "test \"\$(leader_of d2)\" = d1"
check "during the flood: the client is still served" "served"
info "during the flood: d1 holds $(fds) file descriptors, $(rss_mb) MB"
wait $flood
while IFS= read -r line; do case $line in PASS*) pass "tcpflood: ${line#PASS }" ;; FAIL*) fail "tcpflood: ${line#FAIL }" ;; INFO*) info "tcpflood: ${line#INFO }" ;; esac; done <"$tmp/flood.out"
healthy "3000 silent connections"
attack tcpflood ${addr[d1]}:$PORT 3000 12s >"$tmp/flood2.out" 2>&1 &
flood=$!
sleep 4
check "during a flood of d1's own address (Raft's): d2 still sees d1 lead" "test \"\$(leader_of d2)\" = d1"
wait $flood
healthy "3000 silent connections to d1's own address"

echo "== the join"
attacked "join" join $VIP:$PORT "$tmp/x/backend.secret" "$tmp/x/director.secret"
pending_count() { json d1 'len(s.get("pending") or [])'; }
pools() { json d1 '[p["pool"] for p in (s.get("pools") or [])]'; }
check "no lie was recorded: nothing waits for approval" "test \"\$(pending_count)\" = 0"
check "the refusals are in the log, with who tried" "logged $tmp/d1.log 'WARN join refused.*from=${addr[x]}'"
warns=$(grep -c "join refused" "$tmp/d1.log")
attacked "joinflood" joinflood $VIP:$PORT 500
lines=$(($(grep -c "join refused" "$tmp/d1.log") - warns))
check "joinflood: 500 wrong secrets from one address are one log line a minute, not 500 ($lines)" "test $lines -le 2"
healthy "500 joins with a wrong secret"
attacked "pending" pending $VIP:$PORT "$tmp/x/backend.secret" 300
check "pending: one machine gets at most 4 names waiting for approval, not 300 ($(pending_count))" "test \"\$(pending_count)\" -le 4"
check "pending: the rest were told to ask again later, and the log says why" "logged $tmp/d1.log 'join put off: too many nodes wait for approval'"
healthy "300 new names asking to join"
for n in $(json d1 '" ".join(p["pending"] for p in (s.get("pending") or []))'); do on d1 "$bin" admin refuse "$n" >/dev/null; done
none_waiting() { test "$(pending_count)" = 0; }
check "the waiting names refused, nothing waits" "within 10 none_waiting"

echo "== a member gone bad"
attack insider $VIP:$PORT "$tmp/x/backend.secret" evil-1 "$tmp/x" ${addr[d1]}:$PORT >"$tmp/insider.out" 2>&1 &
ins=$!
within 20 grep -q WAITING "$tmp/insider.out"
on d1 "$bin" admin approve evil-1 >/dev/null
wait $ins
while IFS= read -r line; do case $line in PASS*) pass "insider: ${line#PASS }" ;; FAIL*) fail "insider: ${line#FAIL }" ;; esac; done <"$tmp/insider.out"
check "the cluster holds no pool, service or change the insider asked for" "test \"\$(pools)\" = \"['main']\""
on d1 "$bin" admin remove evil-1 >/dev/null
attacked "revoked" revoked $VIP:$PORT "$tmp/x"
healthy "a backend gone bad"

z=$tmp/z
echo "== a cluster that is not one"
# An impostor on the attacker's machine: a theLB chain from its own CA, and a
# proof it cannot make. A backend with the real secret, sent there, stops - and
# never tells it its claim, or asks it to sign anything.
install -d -m 0700 "$tmp/x/impostor"
ip netns exec $P-x "$atk" impostor ${addr[x]}:$PORT "$tmp/x/impostor" >"$tmp/x/impostor/out" 2>&1 &
pid[impostor]=$!
within 10 grep -q READY "$tmp/x/impostor/out"
install -m 0600 "$tmp/d1/backend.secret" "$z/impostor.backend"
out=$(timeout -k 5 20 ip netns exec $P-z unshare --mount --propagation private \
	sh -c 'mount -t tmpfs -o mode=0755 tmpfs /run && exec "$@"' sh "$bin" backend --join ${addr[x]}:$PORT --name victim --secret-file "$z/impostor.backend" 2>&1)
code=$?
check "a backend sent to an impostor stops, saying it did not prove the secret (exit $code)" "test $code = 1 && grep -q 'did not prove it knows the backends' <<<\"\$out\""
check "and it never sent the impostor its claim or a certificate request" "! grep -q 'claim\|csr' $tmp/x/impostor/after-hello 2>/dev/null"
check "and it left nothing on its machine: no thelb0" "! ip -n $P-z link show thelb0 >/dev/null 2>&1"
kill "${pid[impostor]}" 2>/dev/null

echo "== the metrics endpoint"
attacked "metrics" metrics ${addr[d1]}:9732
healthy "the metrics endpoint attacked"

echo "== the local socket"
check "a user other than root cannot reach the director's socket" \
	"! on d1 runuser -u nobody -- python3 -c 'import socket; s = socket.socket(socket.AF_UNIX); s.connect(\"/run/thelb/director/thelb.sock\")' 2>/dev/null"
check "nor read its directory" "! on d1 runuser -u nobody -- ls /run/thelb/director >/dev/null 2>&1"
install -m 0755 "$bin" "$tmp/thelb-anyone"
check "thelb status as another user says so, and nothing more" "on d1 runuser -u nobody -- $tmp/thelb-anyone status 2>&1 | grep -q 'runs only as root'"

echo "== a director started with hostile files"
# starts <what> <want exit|hang> <grep> <thelb director flags...>: a director
# on z, with its own VIP, that must stop at once - saying why.
starts() {
	local what=$1 want=$2 says=$3 out code
	shift 3
	out=$(timeout -k 5 15 ip netns exec $P-z unshare --mount --propagation private \
		sh -c 'mount -t tmpfs -o mode=0755 tmpfs /run && exec "$@"' sh "$bin" director --vip $NET.78 --port $PORT --name z "$@" 2>&1)
	code=$?
	if [ $code = 124 ]; then
		fail "$what: it hangs (killed after 15 s)"
	elif [ $code != "$want" ] || ! grep -qE -- "$says" <<<"$out"; then
		fail "$what: exit $code, $(tail -1 <<<"$out")"
	else
		pass "$what: exit $code, $(grep -E -- "$says" <<<"$out" | head -1 | cut -c1-140)"
	fi
}
install -m 0600 "$tmp/d1/director.secret" "$z/good.secret"
install -m 0600 "$tmp/d1/backend.secret" "$z/good.backend"
sec() { echo --secret-file "$1" --backend-secret-file "$z/good.backend"; }
ln -s "$z/good.secret" "$z/link.secret"
mkfifo -m 0600 "$z/fifo.secret"
install -d -m 0700 "$z/dir.secret"
install -m 0644 "$tmp/d1/director.secret" "$z/open.secret"
install -m 0600 /dev/null "$z/empty.secret"
printf 'thelb1:not-base64!\n' >"$z/garbage.secret" && chmod 0600 "$z/garbage.secret"
head -c 2000000 /dev/zero >"$z/big.secret" && chmod 0600 "$z/big.secret"
install -m 0600 "$tmp/d1/backend.secret" "$z/nobody.secret" && chown nobody "$z/nobody.secret"
starts "a secret file that is a symbolic link" 1 "symbolic link" $(sec "$z/link.secret")
starts "a secret file that is a FIFO" 1 "not a regular file" $(sec "$z/fifo.secret")
starts "a secret file that is a directory" 1 "not a regular file" $(sec "$z/dir.secret")
starts "a secret file others can read" 1 "can be read by group or others" $(sec "$z/open.secret")
starts "an empty secret file" 1 "not a theLB secret" $(sec "$z/empty.secret")
starts "a secret file that is not a secret" 1 "not a theLB secret" $(sec "$z/garbage.secret")
starts "a secret file of 2 MB" 1 "too big" $(sec "$z/big.secret")
starts "a secret file owned by another user" 1 "owned by|owner" --secret-file "$z/good.secret" --backend-secret-file "$z/nobody.secret"
starts "a secret file that does not exist" 1 "there is no secret" $(sec "$z/none.secret")
starts "the backends' secret file missing" 1 "the backends' secret too" --secret-file "$z/good.secret" --backend-secret-file "$z/none.secret"
# Kubeconfigs: the files a director reads at start, then the API server.
kc() { # name, then the file's text on stdin
	cat >"$z/$1.kubeconfig"
	chmod 0600 "$z/$1.kubeconfig"
}
mkfifo -m 0600 "$z/fifo.kubeconfig"
head -c 2000000 /dev/zero | tr '\0' 'a' >"$z/big.kubeconfig"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -keyout "$z/ca.key" -out "$z/ca.pem" -days 1 -subj /CN=lab >/dev/null 2>&1
kc exec <<EOF
apiVersion: v1
clusters:
  - name: c
    cluster:
      server: https://198.18.0.66:6443
      certificate-authority: $z/ca.pem
users:
  - name: u
    user:
      exec:
        command: /bin/sh
        args:
          - -c
          - touch /tmp/thelb-sec-exec-ran
contexts:
  - name: x
    context:
      cluster: c
      user: u
current-context: x
EOF
kc http <<'EOF'
apiVersion: v1
clusters:
  - name: c
    cluster:
      server: http://198.18.0.66:6443
users:
  - name: u
    user:
      token: abc
contexts:
  - name: x
    context:
      cluster: c
      user: u
current-context: x
EOF
mkfifo -m 0600 "$z/token.fifo"
for f in tokenzero tokenfifo cafifo tokensecret insecure; do
	case $f in tokenzero) u="tokenFile: /dev/zero" c="certificate-authority: $z/ca.pem" ;;
	tokenfifo) u="tokenFile: $z/token.fifo" c="certificate-authority: $z/ca.pem" ;;
	cafifo) u="token: abc" c="certificate-authority: $z/token.fifo" ;;
	tokensecret) u="tokenFile: $z/good.secret" c="certificate-authority: $z/ca.pem" ;;
	insecure) u="token: abc" c="insecure-skip-tls-verify: true" ;; esac
	kc $f <<EOF
apiVersion: v1
clusters:
  - name: c
    cluster:
      server: https://198.18.0.66:6443
      $c
users:
  - name: u
    user:
      $u
contexts:
  - name: x
    context:
      cluster: c
      user: u
current-context: x
EOF
done
starts "a kubeconfig that is a FIFO" 1 "not a regular file" $(sec "$z/good.secret") --kubeconfig "$z/fifo.kubeconfig"
starts "a kubeconfig of 2 MB" 1 "larger than" $(sec "$z/good.secret") --kubeconfig "$z/big.kubeconfig"
starts "a kubeconfig that runs a command (exec)" 1 "exec|does not do|signs in with" $(sec "$z/good.secret") --kubeconfig "$z/exec.kubeconfig"
check "and the command never ran" "test ! -e /tmp/thelb-sec-exec-ran"
starts "a kubeconfig to an API server in clear (http)" 1 "https|http" $(sec "$z/good.secret") --kubeconfig "$z/http.kubeconfig"
starts "a kubeconfig whose token file is /dev/zero" 1 "not a regular file" $(sec "$z/good.secret") --kubeconfig "$z/tokenzero.kubeconfig"
starts "a kubeconfig whose token file is a FIFO" 1 "not a regular file" $(sec "$z/good.secret") --kubeconfig "$z/tokenfifo.kubeconfig"
starts "a kubeconfig whose CA file is a FIFO" 1 "not a regular file" $(sec "$z/good.secret") --kubeconfig "$z/cafifo.kubeconfig"
starts "a kubeconfig whose token file is theLB's secret" 1 "secret" $(sec "$z/good.secret") --kubeconfig "$z/tokensecret.kubeconfig"
starts "a kubeconfig that skips the server's certificate check" 1 "insecure-skip-tls-verify" $(sec "$z/good.secret") --kubeconfig "$z/insecure.kubeconfig"

echo "== a Kubernetes API server that lies"
# Each run: the API server on the attacker's machine, a director on z with a
# cluster of its own (VIP .78), its memory capped so that a director that
# holds what it is sent is killed - and seen - rather than the machine.
evil() { # mode
	local mode=$1 d=$tmp/x/kube-$1 kp
	install -d -m 0700 "$d"
	ip netns exec $P-x "$atk" evilkube ${addr[x]}:6443 "$mode" "$d" >"$d/out" 2>&1 &
	kp=$!
	within 10 grep -q READY "$d/out"
	install -d -m 0700 "$z/$mode"
	install -m 0600 "$tmp/d1/director.secret" "$tmp/d1/backend.secret" "$z/$mode/"
	systemd-run --quiet --scope -p MemoryMax=600M -p MemorySwapMax=0 \
		ip netns exec $P-z unshare --mount --propagation private \
		sh -c 'mount -t tmpfs -o mode=0755 tmpfs /run && exec "$@"' sh "$bin" director --vip $NET.78 --port $PORT --name z --cluster evil --bootstrap \
		--secret-file "$z/$mode/director.secret" --backend-secret-file "$z/$mode/backend.secret" --kubeconfig "$d/kubeconfig" >"$z/$mode.log" 2>&1 &
	pid[z]=$!
	sleep 25
	local zpid peak
	zpid=$(pgrep -f -- "--name z --cluster evil" | head -1)
	peak=$(awk '/^VmHWM/ { print int($2 / 1024) }' "/proc/$zpid/status" 2>/dev/null)
	if [ -n "$zpid" ]; then
		pass "evil API ($mode): the director is up after 25 s (peak ${peak:-?} MB)"
	else
		fail "evil API ($mode): the director is gone: $(tail -2 "$z/$mode.log" | tr '\n' ' ' | cut -c1-200)"
	fi
	check "evil API ($mode): the director still leads its own cluster" "grep -q 'OK control VIP taken' $z/$mode.log"
	check "evil API ($mode): the token never went anywhere but the API server" "! grep -q evil-token-not-to-be-seen $d/seen.log 2>/dev/null"
	check "evil API ($mode): what the API server said reached the log without control characters" "! grep -qP '[\x00-\x08\x0b-\x1f\x7f]' $z/$mode.log"
	pkill -TERM -f -- "--name z --cluster evil" 2>/dev/null
	kill "$kp" 2>/dev/null
	sleep 3
	pkill -9 -f -- "--name z --cluster evil" 2>/dev/null
}
for mode in status500 badjson redirect drip hugelist giantline; do
	evil $mode
done
healthy "the hostile files and API servers beside it"

if [ -n "$bbx" ]; then
	echo "== the command line, as root (test/blackbox), on a machine where nothing of theLB runs"
	out=$(ip netns exec $P-z unshare --mount --propagation private \
		sh -c 'mount -t tmpfs -o mode=0755 tmpfs /run && cd / && exec env THELB_BIN="$1" "$2" -test.timeout 30m -test.v' sh "$bin" "$bbx" 2>&1)
	runs=$(grep -o '[0-9]* runs of thelb, as root' <<<"$out")
	check "every command, flag and value, as root: ${runs:-?}" "grep -q '^PASS$' <<<\"\$out\""
	grep -E '^\s+blackbox_test.go:[0-9]+:' <<<"$out" | head -20 | while IFS= read -r l; do info "$l"; done
fi

echo "== the cluster, after it all"
d2_leads() { test "$(leader_of d2)" = d2; }
check "d2 takes over when d1 stops" "kill -TERM ${pid[d1]} && within 30 d2_leads"
check "the client is served through d2" "within 30 served"
check "no panic in any log" "! grep -l 'panic' $tmp/*.log $z/*.log 2>/dev/null"

if [ "$failures" -gt 0 ]; then
	echo "e2e-security: $failures check(s) failed"
	exit 1
fi
echo "e2e-security: all passed"
