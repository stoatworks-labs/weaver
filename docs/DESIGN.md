# weaver design

weaver bonds several WAN links into one tunnel, the way Peplink SpeedFusion and Speedify
do: per-packet, over UDP, with a sequence number on every packet so the far end can put
them back in order. Every link is measured all the time, and traffic moves off a failing
link in under a second.

The same binary is both ends. The **client** has the WANs; it binds one UDP socket to
each WAN device and sends to one server endpoint. The **server** has one UDP socket and
learns each client path from where its authenticated packets come from, so NAT rebinding
and roaming need nothing special. A GL.iNet router can be either end, and so can a VPS
container.

```
 LAN ── client router ──┬── wan (Ethernet) ────┐
        weaver0 (TUN)   ├── wwan (hotel Wi-Fi) ├── internet ── server ── weaver0 ── NAT ── internet
                        └── modem (LTE) ───────┘                (VPS or a GL.iNet at home)
```

## Why a userspace tunnel and not MPTCP

MPTCP (OpenMPTCProuter) only bonds TCP, needs kernel 5.6+ with MPTCP on both ends and a
transparent proxy for everything else. GL.iNet's firmware runs vendor kernels (5.4 on
the Flint 2) and can't be reflashed without losing GL's UI. A TUN device carries every
protocol, including the UDP that calls, games and QUIC use, so weaver can duplicate a
video call over two links while a download is split across three. It runs on any kernel
with `kmod-tun`.

## Layers

| Layer | Package | Job |
| --- | --- | --- |
| Wire | `internal/proto` | Outer packet headers and inner frames |
| Crypto | `internal/handshake`, `internal/replay` | Noise IK handshake, AEAD per packet, replay windows |
| Bond | `internal/bond` | RTT and queue estimators, scheduler, classifier, reorder and dedup buffer |
| Engine | `internal/engine` | Sessions, keypairs, probes, reports, the send and receive loops |
| Platform | `internal/platform` | TUN, addresses, routes, policy rules, per-device sockets, all via netlink and socket options (Linux) |
| Control | `internal/control` | Status over a Unix socket, for the CLI and LuCI |

`internal/bond` does no I/O and takes the time as an argument everywhere, so the
scheduler and reorder logic are unit-tested without a network. `internal/engine` talks
to the platform through an interface, and its tests run two engines over an in-process
link emulator (delay, rate, queue, loss, link down). `test/netns/e2e.sh` runs the real
binary on a real kernel: three network namespaces, netem links, policy routing.

## Wire format

Big-endian throughout. See `internal/proto` for the authoritative encoder.

Outer packets:

```
HandshakeInit  type=1 | pathID | 0 0 | senderIndex(4) | Noise IK msg 1
HandshakeResp  type=2 | pathID | 0 0 | senderIndex(4) | receiverIndex(4) | Noise IK msg 2
Data           type=3 | pathID | 0 0 | receiverIndex(4) | nonce(8) | AEAD(frame)
```

The AEAD is ChaCha20-Poly1305 with the 16-byte data header as associated data. The nonce
is `pathID<<56 | counter`, and the receiver rejects a header whose path ID disagrees with
its nonce. Each path ID has its own counter for the life of the session (a path that is
removed and re-added carries on where it stopped) and its own 2048-packet replay window
per keypair, so a slow link can't push a fast link's packets out of the window. The send
key and the receive key differ (Noise gives one per direction), so the two ends' nonces
never collide.

### Handshake

The handshake is `Noise_IK_25519_ChaChaPoly_BLAKE2s`, the pattern WireGuard uses. The
client knows the server's static key; the server knows each client's. Both payloads are a
hello: `version(1) | instance(4)`, where the instance is a random number each process
picks at start. The client sends the init on every path at once; the server answers each
copy with the same response (it caches responses by the init's hash for 10 s), and the
client takes the first.

The server never acts on an init alone, because anyone can replay one:

- A new keypair waits as *pending*. The server sends with it, and drops the previous
  one, only once data arrives under it, which proves the client holds the keys.
- A changed instance ID (the client restarted, so its sequence numbers and counters
  start again) resets the session's reorder buffer and paths at that same moment, not
  when the init arrives.
- A path's address is learned only from authenticated data, never from an init.

A replayed init therefore costs the server one DH and a pending keypair that expires
after 10 s. (A cookie to drop junk before the DH is not done yet.)

The client re-handshakes every `rekey_interval_s` (default 300 s), and once every path
has been silent for 3 s, since the server may have lost the session. The previous keypair keeps decrypting for 30 s,
so packets in flight on a slow path survive a rekey.

### Inner frames (the AEAD plaintext)

```
DATA      0x01 | flags | seq(4) | sentAt(4, µs) | IP packet
PROBE     0x02 | probeID(4) | sentAt(8, ns)
PROBEACK  0x03 | probeID(4) | sentAt(8) echoed | holdTime(4, µs)
REPORT    0x04 | at(8, ns) | n | n × path report
KEEPALIVE 0x05
PATHINFO  0x06 | n | n × (pathID | priority | flags | upKbps(4) | downKbps(4) | nameLen | name)
```

- `flags` bit 0 on DATA is *immediate*: the receiver delivers the packet without waiting
  for earlier sequence numbers (it is still deduplicated).
- `sentAt` on DATA is the sender's monotonic clock in microseconds, wrapping. The
  receiver uses it to measure each path's queueing delay (below).
- A path report is
  `pathID | state | rxPackets(4) | rxBytes(8) | highestCounter(8) | srtt(4, µs) | rttvar(4, µs) | queue(4, µs) | queueMin(4, µs)`.
  The counters are cumulative, so a lost report costs nothing: the next one covers the
  gap.
- PATHINFO is the client telling the server its paths' names, priorities, metered flags
  (bit 0) and rate caps. It is sent every 5 s, and at once when the paths change.

## Measuring paths

Only the client probes. It sends a PROBE on every path each `probe_interval_ms` (200 ms)
while traffic flows, and every `idle_probe_interval_ms` (1 s) when idle. Metered and
backup paths idle at 5 s. The server answers each probe on the path it arrived on, so
the round-trip time is that path's. The server takes RTT and path state from the
client's REPORTs instead of probing itself, which halves the overhead on a metered
modem.

Both ends send a REPORT every 100 ms while traffic flows (1 s idle). A REPORT describes
what this end received on each path. From two consecutive reports the sender gets, for
each path:

- **loss** = `1 − Δrx / ΔhighestCounter`, smoothed;
- **delivery rate** = `ΔrxBytes / Δat` plus 28 bytes of IPv4 and UDP header per packet,
  on the receiver's own clock, so jitter in the report's trip doesn't count;
- **queueing delay**, from the DATA timestamps. The two ends' clocks are unrelated, but
  both are monotonic, so `arrival − sentAt` moves only with the delay a frame met. Its
  lowest value over the last 30 s is the path with empty queues; anything above that is
  queue. The receiver reports the smoothed value at the time of the report (`queue`)
  and the lowest value in the interval since the last one (`queueMin`, or 0 if fewer
  than eight frames arrived).

`queueMin` is the signal that matters. A queue that never emptied during an interval
means the path was busy for all of it, so what it delivered is its capacity. Jitter on a
radio link doesn't trip it, because among a hundred frames some get through without
waiting. This is one-way and per packet. RTT probes, by comparison, cross both directions,
are few, and are smoothed over about a second.

## Path states and failover

A path is **up**, **suspect** or **down** (the client decides; the server follows the
client's reports):

- **suspect**: a probe has been outstanding longer than `srtt + 4·rttvar + 50 ms`. The
  scheduler stops putting new packets on it at once. If no path at all is up, suspect
  paths still carry traffic, which beats dropping everything.
- **down**: nothing received on the path for `max(path_timeout_ms, 3.5 × probe interval)`.
  While traffic flows that is 1 s; while idle, 3.5 s. After 10 s down, the path's socket
  is rebuilt, and again every 10 s. That picks up a new DHCP lease or a new hotel portal
  session.
- **up** again after one probe answer for a path that has never been up, three for one
  that has.

On the server, a path the client hasn't reported yet is usable while packets arrive on
it, and any path silent for 18 s is down.

Real-time flows are on two paths already, so a link dying under them loses nothing. The
netns test sees no ping lost (20 ms apart) when a link drops mid-download. Bulk packets
in flight on the dead link are lost once and TCP retransmits them.

When every path is down for 5 s, a client with `fallback_direct` removes its tunnel
routes so the LAN reaches the internet directly, and puts them back when the tunnel
returns.

## Scheduling

Each path has a priority:

- **primary**: always used.
- **secondary**: used for overflow. The scheduler adds 50 ms to its delivery estimate,
  so it only takes packets when the primaries are queued that far ahead.
- **backup**: carries nothing while a primary or secondary is up. It is still probed
  (every 5 s idle) so failover to it is instant.

Modes, set per tunnel:

- **adaptive** (default): each packet is classified (below). Real-time and control
  packets are duplicated on the two paths with the earliest delivery estimate. Bulk is
  bonded.
- **bond**: every packet goes to the single path that would deliver it soonest.
- **redundant**: every packet goes on every usable path (SpeedFusion's Hot Failover).
- **failover**: everything on the best path by priority then RTT, until it fails. The
  scheduler switches back to a higher-priority path once that path has been up for 5 s.

**Earliest delivery.** Each path keeps a virtual queue: `busyUntil` advances by
`(size + overhead) / rate` for every packet put on it, and each REPORT raises it to at
least `now + queue`, the queue the far end measured. A packet's delivery estimate on a
path is `max(now, busyUntil) + (size + overhead)/rate + minRTT/2 + penalty`, and the
packet goes to the path with the lowest estimate. `overhead` is 70 bytes: outer IPv4 and
UDP, the data header, the DATA frame header and the tag.

With a near fast path and a far slow one, the fast path takes everything until its queue
is as deep as the difference in their distances. Then both carry traffic, in proportion
to their rates, and packets from both arrive in step. That keeps the reorder buffer
nearly empty and the inner TCP's RTT steady.

**Rate.** Each path has a rate estimate per direction. It starts at the configured
`up_kbps`/`down_kbps` cap, or 10 Mbit/s without one. On every REPORT:

- **Full**: `queueMin` over 5 ms, or smoothed loss over 10 % (a policer drops without
  queueing). If the sender was feeding the path (it sent at least 90 % of `rate`, or at
  least 80 % of what was delivered), what was delivered is the capacity:
  `rate = 0.25 × rate + 0.75 × delivered`.
- **Room**: not full, and the sender kept the path at 90 % of `rate` or more:
  `rate = max(rate, delivered) × 1.1`, or × 1.03 for 2 s after the path was last full,
  so it doesn't keep overshooting.
- Otherwise the sender was app-limited and the rate is left alone.

Loss under 10 % on its own isn't treated as a full path, since radio links lose packets
at any rate. The rate is clamped to 100 kbit/s and to the configured cap.

## Sockets

One loop reads the TUN and writes every path's socket, so a write must never wait.
Path sockets write with `MSG_DONTWAIT`. A full socket buffer (the link's own queue has
backed up into it) drops that packet, as a full queue would, and counts it in
`send_errors` instead of stalling the other paths. Buffers are 2 MB to send and 4 MB to
receive (`SO_*BUFFORCE` as root), so packets wait in the link's queue, where the
scheduler can see them, rather than in the socket.

## Classification (adaptive mode)

| Class | Matches | Handling |
| --- | --- | --- |
| real-time | DSCP EF, CS4, AF41, CS5, CS6, CS7; ICMP and ICMPv6; UDP port 53, 123, 3478–3479, 5060–5061, 19302–19309; any UDP flow under `realtime_kbps` (2000) | duplicated, *immediate* |
| control | TCP SYN, FIN or RST; TCP pure ACKs | duplicated, in order |
| bulk | everything else | bonded, in order |

The flow table is a fixed 4096-entry hash of the 5-tuple with a one-second byte count.
It is approximate on purpose: a collision only means a flow gets duplicated for a
second.

## Reordering

The receiver keeps an 8192-slot ring keyed by sequence number. An in-order packet is
delivered at once and releases anything buffered after it. A packet past a gap is held
until the gap fills or a hold timer expires. On expiry the receiver skips the gap.
Duplicates and very late packets are recognised from the same ring and dropped. An
*immediate* packet skips the hold but still marks its slot.

The hold is `reorder_timeout_ms` when that is set. Otherwise it adapts, as the larger
of:

- `clamp((max srtt − min srtt)/2 + 2 × max rttvar + 5 ms, 5 ms, 200 ms)` across the
  usable paths. A path with no RTT yet counts as 100 ms, so the hold errs long.
- A learned hold. A packet that arrives after its gap was skipped is still delivered,
  and the receiver records how long after. The hold then grows to cover 1.25 × that
  plus 5 ms, up to 500 ms, and decays back over 20 s once late packets stop.

## Routing

**Client.** It creates `weaver0` with the tunnel address. With `default_route` it adds
`0.0.0.0/1` and `128.0.0.0/1` via `weaver0`. The WAN default routes stay where they are,
so turning weaver off restores everything. Each path's socket is bound to its device
(`SO_BINDTODEVICE`) and marked `fwmark_base + pathID`. With `policy_routing` (default)
weaver copies the device's default route, from whichever table holds it, into table
`table_base + pathID`, and adds `fwmark → table` at `rule_priority`. That keeps a path
working when the main table holds only the active WAN's default route, as GL.iNet's
multi-WAN failover leaves it. Auto paths are found the same way: any device that is up,
isn't a tunnel or excluded, and has an IPv4 default route in any table. weaver's own
routes carry protocol 87, so discovery never mistakes its copies for WAN routes.

**Server.** It creates `weaver0` with its address and a route for each peer's
`allowed_ips` via `weaver0`. Inner packets from a peer are accepted only from the peer's
address or its allowed IPs. NAT to the internet is the host's job: the container's
entrypoint adds the masquerade rule, and on OpenWrt the firewall zone does it.

## Configuration

The daemon reads one JSON file (`weaver run -c weaver.json`), or the `WEAVER_*`
environment when there is no `-c` (below). OpenWrt's init script writes the JSON from
UCI. All durations are milliseconds unless the name says otherwise.

```json
{
  "role": "client",
  "private_key": "base64 Curve25519",
  "listen_port": 9328,
  "server": "vps.example.net:9328",
  "server_public_key": "base64",
  "tun": { "name": "weaver0", "mtu": 1380, "address": "10.77.0.2/24" },
  "mode": "adaptive",
  "auto_paths": true,
  "exclude_devices": ["br-lan"],
  "paths": [
    { "name": "wan", "device": "eth1", "priority": "primary", "up_kbps": 0, "down_kbps": 0, "metered": false }
  ],
  "peers": [
    { "name": "flint", "public_key": "base64", "address": "10.77.0.2", "allowed_ips": ["192.168.8.0/24"] }
  ],
  "routing": {
    "default_route": true,
    "subnets": [],
    "fallback_direct": true,
    "policy_routing": true,
    "fwmark_base": 1464139776,
    "table_base": 5745,
    "rule_priority": 5745
  },
  "probe_interval_ms": 200,
  "idle_probe_interval_ms": 1000,
  "path_timeout_ms": 1000,
  "reorder_timeout_ms": 0,
  "realtime_kbps": 2000,
  "rekey_interval_s": 300,
  "control_socket": "/var/run/weaver.sock",
  "log_level": "info"
}
```

- `fwmark_base` is `0x57450000`.
- Client fields (`server`, `server_public_key`, `paths`, `auto_paths`,
  `exclude_devices`, `routing`) are ignored by a server, and `listen_port` and `peers`
  by a client.
- A peer's `address` is a bare IP inside the server's tunnel subnet, with no prefix.
- A client with no `paths` and `auto_paths` off is refused. (The OpenWrt init script
  waits for an interface instead of starting it like that.)
- Unknown fields are an error, so a typo doesn't silently fall back to a default.
- `log_level` is `error`, `warn`, `info`, `debug` or `trace`. `trace` logs every rate
  update.

SIGHUP re-reads the file and applies path, mode and timing changes without dropping the
session. A change to the key, role, `listen_port`, `server`, `server_public_key`, `tun`,
`control_socket`, `peers` or `routing` makes the daemon exit with status 75 for its
supervisor to start it again. So adding a peer restarts a server and drops every
session for a moment; clients reconnect within a second or two.

### Environment (container)

When `-c` is not given and `WEAVER_ROLE` is set, the daemon builds its config from the
environment itself, and ignores SIGHUP (recreate the container to change it):

| Variable | Meaning |
| --- | --- |
| `WEAVER_ROLE` | `server` or `client` |
| `WEAVER_PRIVATE_KEY` | base64 key, or |
| `WEAVER_PRIVATE_KEY_FILE` | path to one; generated (0600) if missing, public key logged even if the rest of the environment is wrong |
| `WEAVER_LISTEN_PORT` | default 9328 |
| `WEAVER_ADDRESS` | tunnel address, default `10.77.0.1/24` for a server |
| `WEAVER_PEERS` | `name,pubkey,address[,allowed_ip…]` entries separated by `;` or newlines |
| `WEAVER_MODE`, `WEAVER_MTU`, `WEAVER_LOG_LEVEL` | as in the JSON |
| `WEAVER_SERVER`, `WEAVER_SERVER_PUBLIC_KEY` | client only |

The log goes to stdout (procd files a service's stderr as `daemon.err`). Errors that
stop the daemon from starting go to stderr.

## Status

`GET /status` on the control socket (HTTP over a Unix socket) returns:

```json
{
  "version": "0.1.0",
  "role": "client",
  "mode": "adaptive",
  "uptime_s": 1234,
  "public_key": "base64",
  "tun": { "name": "weaver0", "address": "10.77.0.2/24", "mtu": 1380 },
  "send_errors": 0,
  "sessions": [
    {
      "peer": "server",
      "public_key": "base64",
      "state": "up",
      "endpoint": "203.0.113.5:9328",
      "established_s": 1200,
      "last_handshake_s": 34,
      "tx_bytes": 0, "rx_bytes": 0,
      "reorder": { "held": 0, "hold_ms": 32.5, "timeouts": 0, "duplicates": 0, "late": 0 },
      "paths": [
        {
          "id": 0, "name": "wan", "device": "eth1", "priority": "primary", "metered": false,
          "state": "up",
          "remote": "203.0.113.5:9328",
          "rtt_ms": 23.4, "rttvar_ms": 2.1, "min_rtt_ms": 20.1,
          "loss_pct": 0.5,
          "tx_kbps": 0, "rx_kbps": 0,
          "rate_kbps": 0, "queue_ms": 0,
          "tx_bytes": 0, "rx_bytes": 0, "tx_packets": 0, "rx_packets": 0,
          "last_rx_ms": 50
        }
      ]
    }
  ]
}
```

- `state` is `up`, `handshaking` or `down` for a session; `up`, `suspect` or `down` for
  a path.
- `rate_kbps` is the scheduler's current send-rate estimate for the path, and
  `queue_ms` the queueing delay the far end last measured on what this end sends on it.
- `tx_kbps` and `rx_kbps` are what actually crossed the path over the last second.
- `last_rx_ms` is -1 when nothing has arrived yet.
- `sessions` is empty, never null, when there are none.
- `metered` is left out when false.

`weaver status` prints this as a table; `weaver status --json` prints it raw.

## OpenWrt integration

- `/etc/config/weaver` holds a `weaver 'main'` section, `path` sections and `peer`
  sections (see `openwrt/weaver/files/weaver.config` and `docs/OPENWRT.md`).
- `/etc/init.d/weaver` (procd) resolves each `path`'s netifd `interface` to its L3
  device, writes `/var/run/weaver/weaver.json` (0600), and runs `weaver run -c`.
  - It also writes a restart key: a digest of the fields that need a restart. procd
    restarts the daemon when the key changes; otherwise `reload` sends SIGHUP.
  - Respawns are unlimited, so the daemon's exit for a restart never counts as a crash.
- `/etc/hotplug.d/iface/90-weaver` reloads on any ifup/ifdown, so a hotel network or a
  modem that comes up later joins the bond.
- `uci-defaults` generates the private key and adds the `weaver` netifd interface
  (`proto none`, `device weaver0`). It also adds a firewall zone with masquerading and
  MSS clamping, with forwarding from `lan` and to `wan`, and a disabled `Allow-Weaver`
  rule. The init script enables that rule while the router is a server.
- `luci-app-weaver` has a status page (paths, RTT, loss, rates, live) and a settings
  page. It reads status through an rpcd plugin that runs `weaver status --json`.

## Not done yet

- Sender-side pacing and queue management per path. Bulk is not dropped or held at the
  sender, so a path's own queue (its modem, its ISP) still decides tail drops. Real-time
  packets survive a full queue only because they are on two paths.
- Per-path IPv6 outer transport, IPv6 inner addressing.
- FEC (Reed-Solomon over groups), as an alternative to plain duplication on lossy links.
- A handshake MAC and cookie so the server can drop junk without doing DH.
- Server-assigned client addresses.
- Multiple server endpoints for the client (server failover).
