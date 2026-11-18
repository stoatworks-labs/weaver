#!/bin/sh
# End-to-end test of weaver on a real Linux kernel, using network namespaces.
#
#   ns client ── wan1 (20/40 Mbit/s, 10 ms) ──┐
#             ── wan2 (10/20 Mbit/s, 40 ms) ──┴── ns isp ── ns server (weaver server,
#                                                            203.0.113.10 = "the internet")
#
# wan1's default route is in the main table; wan2's is only in table 102, the
# way GL.iNet's multi-WAN leaves a standby WAN. weaver must find both (auto
# paths) and use both (policy routing).
#
# Usage (root): test/netns/e2e.sh path/to/weaver-linux-binary
# Needs iproute2 with netem, iperf3, python3.
#
# Environment: MODE (adaptive), BUF_MS (100: each link's queue, in ms at its
# rate; real links range from ~20 to 500+), LOG (info), KEEP (keep the work dir).
set -eu

BIN=$(readlink -f "${1:?usage: e2e.sh <weaver binary>}")
WORK=$(mktemp -d)
FAILS=0
cd "$WORK"

say() { printf '\n== %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*"; FAILS=$((FAILS + 1)); }
pass() { printf 'PASS: %s\n' "$*"; }

cleanup() {
	set +e
	for p in "$WORK"/*.pid; do [ -f "$p" ] && kill "$(cat "$p")" 2>/dev/null; done
	sleep 0.5
	for n in client isp server; do ip netns del "w-$n" 2>/dev/null; done
	[ "${KEEP:-}" ] || rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

C="ip netns exec w-client"
I="ip netns exec w-isp"
S="ip netns exec w-server"

say "topology"
for n in client isp server; do ip netns del "w-$n" 2>/dev/null || true; ip netns add "w-$n"; done
for n in client isp server; do ip netns exec "w-$n" ip link set lo up; done

ip link add wan1 netns w-client type veth peer name isp-w1 netns w-isp
ip link add wan2 netns w-client type veth peer name isp-w2 netns w-isp
ip link add uplink netns w-server type veth peer name isp-up netns w-isp

$C ip addr add 172.16.1.2/24 dev wan1
$C ip addr add 172.16.2.2/24 dev wan2
$C ip link set wan1 up
$C ip link set wan2 up
$C ip route add default via 172.16.1.1 dev wan1 metric 10
$C ip route add default via 172.16.2.1 dev wan2 table 102

$I ip addr add 172.16.1.1/24 dev isp-w1
$I ip addr add 172.16.2.1/24 dev isp-w2
$I ip addr add 198.51.100.1/24 dev isp-up
for d in isp-w1 isp-w2 isp-up; do $I ip link set "$d" up; done
$I sysctl -qw net.ipv4.ip_forward=1

$S ip addr add 198.51.100.2/24 dev uplink
$S ip link set uplink up
$S ip route add default via 198.51.100.1
$S ip link add inet type dummy
$S ip addr add 203.0.113.10/32 dev inet
$S ip link set inet up
$S sysctl -qw net.ipv4.ip_forward=1

# Link shapes: client egress is the uplink, isp egress the downlink. netem
# adds the distance (and any loss); a tbf under it is the bottleneck, with a
# queue of BUF_MS at the link's rate. (netem's own rate and limit would count
# packets still crossing its delay line against the queue, so a stream of
# small ACKs overflows a link it barely uses. Real links do not do that.)
shape() { # ns dev rate delay [loss]
	ip netns exec "$1" tc qdisc replace dev "$2" root handle 1: netem delay "$4" limit 100000 ${5:+loss "$5"}
	ip netns exec "$1" tc qdisc replace dev "$2" parent 1:1 handle 10: tbf rate "$3" burst 16k latency "${BUF_MS:-100}ms"
}
shape w-client wan1 20mbit 10ms
shape w-isp isp-w1 40mbit 10ms
shape w-client wan2 10mbit 40ms
shape w-isp isp-w2 20mbit 40ms

say "keys and config"
SK=$("$BIN" genkey); SP=$(echo "$SK" | "$BIN" pubkey)
CK=$("$BIN" genkey); CP=$(echo "$CK" | "$BIN" pubkey)
cat >server.json <<EOF
{"role":"server","private_key":"$SK","listen_port":9328,
 "tun":{"name":"weaver0","address":"10.77.0.1/24"},
 "peers":[{"name":"client","public_key":"$CP","address":"10.77.0.2"}],
 "control_socket":"$WORK/server.sock","log_level":"${LOG:-info}"}
EOF
cat >client.json <<EOF
{"role":"client","private_key":"$CK","server":"198.51.100.2:9328","server_public_key":"$SP",
 "tun":{"name":"weaver0","address":"10.77.0.2/24"},"mode":"${MODE:-adaptive}","auto_paths":true,
 "routing":{"default_route":true,"fallback_direct":true},
 "control_socket":"$WORK/client.sock","log_level":"${LOG:-info}"}
EOF

$S "$BIN" run -c server.json >server.log 2>&1 & echo $! >server.pid
$C "$BIN" run -c client.json >client.log 2>&1 & echo $! >client.pid

# status <side> <python expression over the status dict `s`>
status() {
	$C "$BIN" status --json --socket "$WORK/$1.sock" 2>/dev/null |
		python3 -c "import json,sys; s=json.load(sys.stdin); print($2)" 2>/dev/null
}
paths_up() { status client "sum(1 for p in s['sessions'][0]['paths'] if p['state']=='up')"; }
path_state() { status client "[p['state'] for p in s['sessions'][0]['paths'] if p['name']=='$1'][0]"; }

say "handshake and path discovery"
i=0
while [ "$(paths_up)" != "2" ] && [ $i -lt 100 ]; do sleep 0.1; i=$((i + 1)); done
if [ "$(paths_up)" = "2" ]; then pass "both WANs up after $((i * 100)) ms (wan2 only via table 102)"; else
	fail "paths up: $(paths_up)"; cat client.log; exit 1; fi
$C "$BIN" status --socket "$WORK/client.sock"

say "ping through the tunnel"
if $C ping -c 5 -i 0.2 -W 1 203.0.113.10 >ping.txt; then pass "$(tail -1 ping.txt)"; else fail "ping"; cat ping.txt; fi

iperf() { # extra args; prints Mbit/s received and the sender's retransmits
	$S iperf3 -s -1 -B 203.0.113.10 >/dev/null 2>&1 &
	sleep 0.3
	$C iperf3 -c 203.0.113.10 -t "${T:-8}" -J "$@" >iperf.json 2>&1 || true
	python3 - iperf.json <<'PY' || echo 0
import json, sys
j = json.load(open(sys.argv[1]))
if 'error' in j:
    print(0, '(iperf error: %s)' % j['error'], file=sys.stderr); print(0); sys.exit()
e = j['end']
print(round(e['sum_received']['bits_per_second'] / 1e6, 1), end='')
print('  retransmits %s' % e.get('sum_sent', {}).get('retransmits', '?'), file=sys.stderr)
PY
}

diag() {
	echo "-- server's view:"
	$S "$BIN" status --socket "$WORK/server.sock" 2>/dev/null | tail -n +4 || true
	echo "-- send errors (client, server): $(status client "s['send_errors']") $($S "$BIN" status --json --socket "$WORK/server.sock" 2>/dev/null | python3 -c "import json,sys; print(json.load(sys.stdin)['send_errors'])")"
	echo "-- reorder (client, server):"
	status client "s['sessions'][0]['reorder']"
	$S "$BIN" status --json --socket "$WORK/server.sock" 2>/dev/null | python3 -c "import json,sys; print(json.load(sys.stdin)['sessions'][0]['reorder'])" || true
	echo "-- UDP/TCP counters:"
	for n in client server; do printf '%s: ' "$n"; ip netns exec "w-$n" nstat -az 2>/dev/null |
		awk '/UdpRcvbufErrors|UdpSndbufErrors|UdpInErrors|TcpRetransSegs|TcpExtTCPSACKReorder|TcpExtTCPLostRetransmit/ {printf "%s=%s ", $1, $2}'; echo; done
	echo "-- qdisc drops:"
	for d in wan1 wan2; do printf 'client %s: ' "$d"; $C tc -s qdisc show dev "$d" | awk '/dropped/ {print $0}'; done
	for d in isp-w1 isp-w2; do printf 'isp %s: ' "$d"; $I tc -s qdisc show dev "$d" | awk '/dropped/ {print $0}'; done
	echo "-- softnet drops:"; awk '{print $2}' /proc/net/softnet_stat | tr '\n' ' '; echo
}

# watch <side> <file>: sample one end's per-path view every 0.5 s until killed
watch() {
	while :; do
		ip netns exec "w-$1" "$BIN" status --json --socket "$WORK/$1.sock" 2>/dev/null | python3 -c "
import json,sys
s=json.load(sys.stdin)['sessions'][0]
print(' | '.join('%s tx=%5.0f rate=%5.0f q=%5.1f rtt=%5.1f min=%5.1f %s' % (p['name'], p['tx_kbps']/1000, p['rate_kbps']/1000, p.get('queue_ms',0), p['rtt_ms'], p['min_rtt_ms'], p['state']) for p in s['paths']), ' hold=%.0f late=%d skip=%d' % (s['reorder']['hold_ms'], s['reorder']['late'], s['reorder']['timeouts']))" 2>/dev/null
		sleep 0.5
	done >"$2"
}

say "download, one TCP stream (wan1 alone: 40, both: 60 Mbit/s)"
watch server down.watch & WATCH=$!
DOWN=$(iperf -R)
kill $WATCH 2>/dev/null; wait $WATCH 2>/dev/null || true
echo "-- server per-path samples (Mbit/s, ms):"; cat down.watch
echo "download: $DOWN Mbit/s"
$C "$BIN" status --socket "$WORK/client.sock"
diag
python3 -c "import sys; sys.exit(0 if $DOWN > 44 else 1)" && pass "download $DOWN Mbit/s beats the best single link (40)" || fail "download $DOWN Mbit/s does not beat wan1 alone"

say "upload, one TCP stream (wan1 alone: 20, both: 30 Mbit/s)"
watch client up.watch & WATCH=$!
UP=$(iperf)
kill $WATCH 2>/dev/null; wait $WATCH 2>/dev/null || true
echo "-- client per-path samples (Mbit/s, ms):"; cat up.watch
echo "upload: $UP Mbit/s"
diag
python3 -c "import sys; sys.exit(0 if $UP > 22 else 1)" && pass "upload $UP Mbit/s beats the best single link (20)" || fail "upload $UP Mbit/s does not beat wan1 alone"

say "download, four streams"
DOWN4=$(iperf -R -P 4)
echo "download x4: $DOWN4 Mbit/s"

say "failover: wan1 dies under a 20 ms ping stream and a download"
PING_AT=$(date +%s.%N)
# -c and -W, not -w: a deadline ends ping with its last RTT of pings in flight,
# which would count as lost.
$C ping -i 0.02 -c 350 -W 2 203.0.113.10 >fping.txt 2>&1 &
PING=$!
T=8 iperf -R >fail-iperf.txt &
IP=$!
sleep 3
$C ip link set wan1 down
FAIL_AT=$(date +%s.%N)
wait $PING || true
wait $IP || true
SENT=$(sed -n 's/^\([0-9]*\) packets transmitted.*/\1/p' fping.txt)
RECV=$(sed -n 's/.* \([0-9]*\) received.*/\1/p' fping.txt)
LOST=$((SENT - RECV))
echo "ping: sent $SENT, received $RECV, lost $LOST (each 20 ms); download during failover: $(cat fail-iperf.txt) Mbit/s"
# Which pings were lost, against when wan1 went (seq 1 left at PING_AT).
LOSTSEQ=$(python3 - fping.txt "$SENT" <<'PY'
import re, sys
got = {int(m) for m in re.findall(r'icmp_seq=(\d+)', open(sys.argv[1]).read())}
print(' '.join(str(i) for i in range(1, int(sys.argv[2]) + 1) if i not in got) or 'none')
PY
)
FAIL_SEQ=$(python3 -c "print(int(($FAIL_AT - $PING_AT) / 0.02))")
echo "lost icmp_seq: $LOSTSEQ (wan1 went down near seq $FAIL_SEQ)"
[ "$LOST" -le 5 ] && pass "ICMP lost $LOST packets across the failover" || fail "ICMP lost $LOST packets across the failover"
# Auto paths drop a path whose device has no default route left, so "gone"
# counts as not up; a path still listed as up does not.
W1=$(path_state wan1 || true)
[ "$W1" != "up" ] && pass "wan1 is ${W1:-gone (auto paths dropped it)} after its link went down" || fail "wan1 still marked up"

say "recovery"
$C ip link set wan1 up
$C ip route add default via 172.16.1.1 dev wan1 metric 10 2>/dev/null || true
i=0
while [ "$(path_state wan1)" != "up" ] && [ $i -lt 150 ]; do sleep 0.1; i=$((i + 1)); done
[ "$(path_state wan1)" = "up" ] && pass "wan1 back up $((i * 100)) ms after its link returned" || fail "wan1 did not come back: $(path_state wan1)"

say "lossy link: 5% loss on wan2 both ways, adaptive mode duplicates pings"
shape w-client wan2 10mbit 40ms 5%
shape w-isp isp-w2 20mbit 40ms 5%
$C ip link set wan1 down
sleep 1.5
$C ping -c 100 -i 0.02 -W 1 203.0.113.10 >lossy.txt 2>&1 || true
echo "wan2 alone at 5% each way: $(grep 'packet loss' lossy.txt)"
$C ip link set wan1 up
$C ip route add default via 172.16.1.1 dev wan1 metric 10 2>/dev/null || true

say "fallback to direct internet when the server is gone"
kill "$(cat server.pid)"; rm server.pid
i=0
while $C ip route show | grep -q '0.0.0.0/1 dev weaver0' && [ $i -lt 150 ]; do sleep 0.1; i=$((i + 1)); done
if $C ip route show | grep -q '0.0.0.0/1 dev weaver0'; then fail "tunnel routes still present 15 s after the server died"; else
	pass "tunnel routes withdrawn $((i * 100)) ms after the server died"; fi

say "logs (last lines)"
tail -n 15 client.log
tail -n 8 server.log 2>/dev/null || true

say "result"
if [ $FAILS -eq 0 ]; then echo "ALL PASSED"; else echo "$FAILS FAILED"; exit 1; fi
