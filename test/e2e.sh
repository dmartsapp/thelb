#!/bin/bash
# End to end, as root, on Linux: the built binary bootstraps a director in a
# throw-away directory, and every command that exists so far is checked against
# it - the secret files, status as tables and JSON, admin secret show, one theLB
# per node, root only (with the kernel's own answer for who calls), and a clean
# stop. Nothing is sent on the network, and nothing is left behind.
#
#   sudo test/e2e.sh bin/thelb

set -u
bin="$(readlink -f "${1:?usage: test/e2e.sh <thelb binary>}")"
[ "$(id -u)" = 0 ] || { echo "e2e: run as root"; exit 1; }
[ -x "$bin" ] || { echo "e2e: $bin is not executable"; exit 1; }

failures=0
pass() { printf '  ok    %s\n' "$1"; }
fail() { printf '  FAIL  %s\n' "$1"; failures=$((failures + 1)); }
check() { if eval "$2"; then pass "$1"; else fail "$1"; fi; }

tmp="$(mktemp -d /tmp/thelb-e2e.XXXXXX)"
# Another user must be able to run the binary for the "root only" checks: a copy
# in a directory anyone may enter, though not list (the secret files are 0600).
chmod 0711 "$tmp"
install -d -m 0755 "$tmp/bin"
install -m 0755 "$bin" "$tmp/bin/thelb"
bin="$tmp/bin/thelb"
pid=""
cleanup() {
	[ -n "$pid" ] && kill "$pid" 2>/dev/null && wait "$pid" 2>/dev/null
	rm -rf "$tmp"
}
trap cleanup EXIT

if [ -e /run/thelb/thelb.sock ]; then
	echo "e2e: theLB is already running on this machine; stop it first"
	exit 1
fi

# A control VIP in the network of the interface the default route uses - an
# address nothing is plumbed on: this build sends and adds nothing.
read -r dev src < <(ip -4 route get 192.0.2.1 | awk '{for (i = 1; i < NF; i++) { if ($i == "dev") d = $(i+1); if ($i == "src") s = $(i+1) } } END { print d, s }')
prefix="$(ip -o -4 addr show dev "$dev" | awk -v s="$src" '{ split($4, a, "/"); if (a[1] == s) print $4 }' | head -n1)"
vip="$(python3 -c "import ipaddress, sys; n = ipaddress.ip_interface(sys.argv[1]).network; print([str(h) for h in n.hosts()][-2])" "$prefix")"
echo "e2e: $bin on $dev ($prefix), control VIP $vip"

start() {
	"$bin" director --bootstrap --vip "$vip" --port 7443 --cluster e2e --name e2e-1 \
		--secret-file "$tmp/director.secret" --backend-secret-file "$tmp/backend.secret" >"$tmp/log" 2>&1 &
	pid=$!
	for _ in $(seq 1 50); do
		grep -q "director ready" "$tmp/log" 2>/dev/null && return 0
		kill -0 "$pid" 2>/dev/null || break
		sleep 0.2
	done
	echo "e2e: the director did not get ready:"; cat "$tmp/log"
	return 1
}
stop() {
	kill -TERM "$pid"
	wait "$pid"
	code=$?
	pid=""
	return $code
}

echo "== bootstrap"
start || exit 1
for f in director backend; do
	check "the $f secret file is root's, mode 0600" "[ \"\$(stat -c '%U %a' $tmp/$f.secret)\" = 'root 600' ]"
	check "the $f secret file holds a secret" "grep -qE '^thelb1:[A-Za-z0-9_-]{43}$' $tmp/$f.secret"
done
check "/run/thelb is root's, mode 0700" "[ \"\$(stat -c '%U %a' /run/thelb)\" = 'root 700' ]"
check "the socket is root's, mode 0600" "[ \"\$(stat -c '%U %a' /run/thelb/thelb.sock)\" = 'root 600' ]"

echo "== status"
"$bin" status >"$tmp/status" 2>&1
check "status exits 0" "[ $? = 0 ]"
check "status shows the cluster" "grep -qE '^ e2e +\\| [0-9a-f]{8} \\|' $tmp/status"
check "status shows the CA and the director's certificate" "grep -q 'theLB e2e CA | ca' $tmp/status && grep -qE '^ e2e-1 +\\| director' $tmp/status"
check "status shows the empty tables" "[ \$(grep -c '(0 rows)' $tmp/status) -ge 6 ]"
"$bin" status --json >"$tmp/status.json"
check "status --json is the same cluster" "python3 -c 'import json,sys; s=json.load(open(sys.argv[1])); assert s[\"cluster\"][\"name\"]==\"e2e\" and len(s[\"certificates\"])==2 and s[\"pools\"]==[] and len(s[\"nodes\"][0][\"node_fingerprint\"])==16' $tmp/status.json"
check "status nodes shows one table" "[ \"\$($bin status nodes | grep -c 'rows\\?)')\" = 1 ]"

echo "== secrets"
for f in director backend; do
	check "admin secret show --role $f is the file's" "[ \"\$($bin admin secret show --role $f)\" = \"\$(cat $tmp/$f.secret)\" ]"
	check "the $f secret is not in the log" "! grep -qF \"\$(cut -d: -f2 $tmp/$f.secret)\" $tmp/log"
done

echo "== one theLB per node"
"$bin" director --bootstrap --vip "$vip" --port 7443 --secret-file "$tmp/x.secret" --backend-secret-file "$tmp/y.secret" >"$tmp/second" 2>&1
code=$?
check "a second director exits 1, already running" "[ $code = 1 ] && grep -q 'already running' $tmp/second"
check "the second director created no secret file" "[ ! -e $tmp/x.secret ] && [ ! -e $tmp/y.secret ]"

echo "== root only"
check "help runs for anyone" "runuser -u nobody -- $bin --help >/dev/null"
check "--version runs for anyone" "runuser -u nobody -- $bin --version | grep -q '^thelb '"
check "status refuses a user" "! runuser -u nobody -- $bin status 2>&1 | grep -q '^ e2e' && runuser -u nobody -- $bin status 2>&1 | grep -q 'runs only as root'"
check "the socket refuses a user's process" "! runuser -u nobody -- python3 -c 'import socket; s=socket.socket(socket.AF_UNIX); s.connect(\"/run/thelb/thelb.sock\")' 2>/dev/null"

echo "== stop"
stop
check "the director stops cleanly on SIGTERM" "[ $? = 0 ] && grep -q 'OK stopped' $tmp/log"
check "nothing is left in /run" "[ ! -e /run/thelb ]"

echo "== restart: the secret files are kept"
before="$(sha256sum "$tmp/director.secret" "$tmp/backend.secret")"
start || exit 1
check "the same secrets after a restart" "[ \"\$(sha256sum $tmp/director.secret $tmp/backend.secret)\" = \"$before\" ]"
check "no file created the second time" "! grep -q 'secret file created' $tmp/log"
stop

echo "== without --bootstrap"
"$bin" director --vip "$vip" --port 7443 --secret-file "$tmp/director.secret" --backend-secret-file "$tmp/backend.secret" >"$tmp/join" 2>&1
code=$?
check "a director without --bootstrap exits 1: joining comes later" "[ $code = 1 ] && grep -q 'joining comes with the cluster phase' $tmp/join"
check "and leaves nothing in /run" "[ ! -e /run/thelb ]"

echo
if [ "$failures" -gt 0 ]; then
	echo "e2e: $failures check(s) failed. The director's log:"
	cat "$tmp/log"
	exit 1
fi
echo "e2e: all checks passed"
