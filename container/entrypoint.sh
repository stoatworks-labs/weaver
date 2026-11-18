#!/bin/sh
# Container entrypoint: prepare forwarding and NAT for the tunnel, then exec
# weaver in the foreground. See docs/CONTAINER.md.
#
#   (no args)        weaver run, from /etc/weaver/weaver.json if it exists,
#                    otherwise from the WEAVER_* environment
#   -<flags>         passed on to weaver run
#   <subcommand>     any other weaver command, run as is (genkey, status, ...)
#
# WEAVER_MASQ_SUBNET overrides the subnet masqueraded out of the default-route
# interface (default: the tunnel address's subnet).
set -eu

CONF=/etc/weaver/weaver.json

log() {
	echo "weaver-entrypoint: $*" >&2
}

# A weaver subcommand (`docker run IMAGE genkey`, `docker exec ... status`).
if [ $# -gt 0 ] && [ "${1#-}" = "$1" ]; then
	exec weaver "$@"
fi

# --- IPv4 forwarding -----------------------------------------------------------

if [ "$(cat /proc/sys/net/ipv4/ip_forward 2>/dev/null)" != 1 ]; then
	if ! { echo 1 >/proc/sys/net/ipv4/ip_forward; } 2>/dev/null; then
		log "WARNING: net.ipv4.ip_forward is 0 and /proc/sys is read-only here;" \
			"clients will reach the server but not the internet." \
			"Start the container with --sysctl net.ipv4.ip_forward=1."
	fi
fi

[ -c /dev/net/tun ] ||
	log "WARNING: /dev/net/tun is missing; start the container with --device /dev/net/tun."

# --- NAT for the tunnel subnet ---------------------------------------------------

# The tunnel address: tun.address from the config file, or WEAVER_ADDRESS.
tun_address() {
	if [ -f "$CONF" ]; then
		tr -d '\n' <"$CONF" |
			sed -n 's/.*"tun"[[:space:]]*:[[:space:]]*{\([^}]*\)}.*/\1/p' |
			sed -n 's/.*"address"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p'
	else
		echo "${WEAVER_ADDRESS:-10.77.0.1/24}"
	fi
}

# iptables in Alpine 3.22 is the nft backend; fall back to legacy where the
# kernel has no nf_tables.
IPT=""
for t in iptables iptables-legacy; do
	if command -v "$t" >/dev/null 2>&1 && "$t" -t nat -S >/dev/null 2>&1; then
		IPT=$t
		break
	fi
done

subnet=${WEAVER_MASQ_SUBNET:-$(tun_address)}
wan=$(ip -4 route show default 2>/dev/null |
	awk '{ for (i = 1; i < NF; i++) if ($i == "dev") { print $(i + 1); exit } }')

if [ -z "$IPT" ]; then
	log "WARNING: iptables is unusable (no NET_ADMIN?); no NAT for the tunnel."
elif [ -z "$subnet" ]; then
	log "WARNING: no tunnel address found; set WEAVER_MASQ_SUBNET. No NAT added."
elif [ -z "$wan" ]; then
	log "WARNING: no IPv4 default route; no NAT added."
else
	# iptables masks host bits itself, so 10.77.0.1/24 matches 10.77.0.0/24,
	# and -C finds the rule from an earlier start of the same container.
	if "$IPT" -t nat -C POSTROUTING -s "$subnet" -o "$wan" -j MASQUERADE 2>/dev/null; then
		log "masquerade for $subnet out of $wan already present"
	elif "$IPT" -t nat -A POSTROUTING -s "$subnet" -o "$wan" -j MASQUERADE; then
		log "masquerading $subnet out of $wan ($IPT)"
	else
		log "WARNING: could not add the masquerade rule for $subnet"
	fi
	# MSS clamping for TCP forwarded through the tunnel's smaller MTU.
	if ! "$IPT" -t mangle -C FORWARD -p tcp --tcp-flags SYN,RST SYN \
		-j TCPMSS --clamp-mss-to-pmtu 2>/dev/null; then
		"$IPT" -t mangle -A FORWARD -p tcp --tcp-flags SYN,RST SYN \
			-j TCPMSS --clamp-mss-to-pmtu 2>/dev/null ||
			log "WARNING: could not add TCPMSS clamping"
	fi
fi

# --- run ---------------------------------------------------------------------------

if [ -f "$CONF" ]; then
	log "config: $CONF"
	exec weaver run -c "$CONF" "$@"
fi

export WEAVER_ROLE="${WEAVER_ROLE:-server}"
if [ -z "${WEAVER_PRIVATE_KEY:-}" ]; then
	export WEAVER_PRIVATE_KEY_FILE="${WEAVER_PRIVATE_KEY_FILE:-/var/lib/weaver/private.key}"
	mkdir -p "$(dirname "$WEAVER_PRIVATE_KEY_FILE")"
	chmod 0700 "$(dirname "$WEAVER_PRIVATE_KEY_FILE")" 2>/dev/null || true
fi
log "config: environment (role $WEAVER_ROLE)"
exec weaver run "$@"
