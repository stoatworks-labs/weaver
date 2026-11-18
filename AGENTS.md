# AGENTS.md — weaver

Invariants and traps for anyone (human or agent) changing this repo. `docs/DESIGN.md` is
the protocol and behaviour; this is what it cost to learn.

## The shape

- **One binary, both ends.** `weaver run` is a client or a server by config. Only the
  client probes; the server takes RTT and path state from the client's REPORTs.
- **`internal/bond` does no I/O** and takes the time as an argument everywhere, so the
  scheduler, estimators and reorder buffer are unit-tested without a network. Keep it
  that way: no `time.Now()`, no goroutines, no locks (the engine holds the session lock).
- **The wire format is the protocol.** Any change to a header, frame or report layout
  bumps `proto.Version`. There is no compatibility with older peers yet, and the hello
  refuses a mismatch.
- **One loop writes every path's socket.** A write that waits stalls every path, so
  sockets write with `MSG_DONTWAIT` and drop on a full buffer (`send_errors`).

## Traps

- **Nonce counters belong to the session, per path ID, not to the `Path`.** A path
  removed and re-added (a WAN unplugged, a hotplug reload) keeps its ID by name; if its
  counter restarts, the peer's replay window for that ID drops everything on it, and the
  path stays down, rebinding every 10 s. `TestPathRemovedAndReAdded`.
- **Never act on an unconfirmed init.** Anyone can replay one. A new instance ID resets
  the session, and path addresses are learned, only once data arrives under the new
  keypair. `TestReplayedInitFromOldInstanceIsHarmless`.
- **Don't schedule on probe RTT.** `SRTT − MinRTT` crosses both directions, comes from
  five probes a second, and is smoothed over about a second. Driven by it, the slow
  path got 0–19 of its 20 Mbit/s and a single download never beat the fast link alone.
  The queue signal is the one-way `queueMin` from DATA timestamps (`bond.OWD`).
- **Loss alone is not a full path.** Treating 2 % loss as congestion walked a lossy
  link's rate estimate to the floor. Full means a standing one-way queue, or loss over
  10 % (a policer).
- **The server's RTT is the client's.** `RTT.Seed` from REPORTs; its min is the lowest
  reported SRTT, not a raw sample.
- **Delivered bytes are UDP payloads.** Add the 28 bytes of IPv4 and UDP per packet
  before comparing with what the scheduler assigned (`bond.PacketOverhead` includes
  them).
- **Logs go to stdout.** procd files a service's stderr as `daemon.err` whatever the
  line's level. Fatal start errors stay on stderr.

## Test-rig traps (test/netns/e2e.sh)

- **netem's `rate` and `limit` count packets still in its delay line against the queue.**
  A stream of small ACKs then overflows a 40 ms link it barely uses. It read as 10–23 %
  loss on an idle uplink and collapsed the rate estimate. Shape with netem for delay
  only, and a `tbf` child for rate and a byte-sized queue.
- **`ping -w N` ends with its last RTT of pings in flight** and counts them as lost.
  That looked like 5–7 pings lost at failover; it was zero. Use `-c` with `-W`.
- **`x=$(cmd)` under `set -e` exits the script when `cmd` fails** (as `path_state` does
  once auto paths has dropped a path). `x=$(cmd || true)`.
- **Clients shaped on their own egress interface put the link's queue in the socket's
  memory.** That is what exposed the blocking-write stall above; real routers with a
  local bottleneck (USB modems) do the same.

## Testing

- `go test -race ./...` runs everything on a Mac or Linux. The engine tests run two
  engines over an in-process link emulator, about 30 s.
- `sudo test/netns/e2e.sh path/to/weaver-linux` needs root on Linux with netem, tbf,
  iperf3 and python3, so use a throwaway VM. Last run: download 51.2 Mbit/s (wan1 alone
  40, both 60), upload 26.6 (20 and 30), four streams 53, 0 of 350 pings lost when wan1
  dies mid-download. Steady state reaches about 58 and 29.
- `scripts/build.sh && scripts/build-ipk.sh` build every OpenWrt arch and the ipks, no
  SDK needed. `docs/OPENWRT.md` lists what was tested in OpenWrt 21.02 x86_64 containers.
- **Nothing has run on a GL.iNet router or any non-x86 OpenWrt device yet.** Say so in
  anything you write about it until it has.
