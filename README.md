# weaver — multi-WAN bonding with adaptive failover for OpenWrt

> **AI-assisted project.** This codebase was created with [Claude](https://claude.com/claude-code)
> (Anthropic), directed and reviewed by a human author. **Work in progress: nothing here
> has run on a router yet.** It has been tested in network namespaces on a Linux VM and in
> OpenWrt 21.02 x86_64 containers. See [What has been tested](#what-has-been-tested).

One Go binary, `weaver`, that bonds every WAN a router has (Ethernet, hotel Wi-Fi, USB
tethering, a cellular modem) into one encrypted tunnel to a remote endpoint, per packet,
the way SpeedFusion and Speedify do. It runs as either end: on a GL.iNet travel router
as an OpenWrt package, on a router at home, or on a VPS as a container.

```
 LAN ── client router ──┬── wan (Ethernet) ────┐
        weaver0 (TUN)   ├── wwan (hotel Wi-Fi) ├── internet ── server ── NAT ── internet
                        └── modem (LTE) ───────┘                (VPS or a GL.iNet at home)
```

- **Bonding.** Bulk traffic is split across the WANs by measured capacity and distance,
  so packets from a near fast link and a far slow one arrive in step. The far end puts
  them back in order.
- **Adaptive failover.** Calls, games, DNS, pings and TCP handshakes are duplicated on
  the two best links, so a link dying under them loses nothing. Everything else moves off
  a failed link within about a second.
- **Modes.** `adaptive` (the default), `bond`, `redundant` (every packet on every link)
  and `failover`. Per-link priorities: primary, secondary (overflow only), backup (only
  when nothing else is up) and disabled.
- **Works with GL.iNet's own multi-WAN.** Each link gets its own socket, fwmark and
  routing table, so a standby WAN still carries its share while GL's failover points
  the main table elsewhere.
- **Encryption.** Noise IK (the WireGuard pattern), ChaCha20-Poly1305 per packet, a
  replay window per link.

## Getting started

- **Server on a VPS:** [docs/CONTAINER.md](docs/CONTAINER.md). `docker build -t weaver .`,
  then one `docker run` with `NET_ADMIN` and `/dev/net/tun`.
- **Client (or server) on OpenWrt or GL.iNet:** [docs/OPENWRT.md](docs/OPENWRT.md).
  `scripts/build.sh && scripts/build-ipk.sh` makes the ipks for every common router
  architecture without an SDK. The Flint 2 (GL-MT6000) is `aarch64_cortex-a53`.
- **How it works:** [docs/DESIGN.md](docs/DESIGN.md) covers the wire format, handshake,
  path measurement, scheduler, reorder buffer, configuration and status JSON.

```sh
weaver genkey | tee private.key | weaver pubkey   # a key pair
weaver run -c weaver.json                          # run either end
weaver status                                      # paths, RTT, loss, rates
```

## What has been tested

- **Unit and emulator tests** (`go test -race ./...`): two engines over an in-process
  link emulator. They cover bonding by capacity, failover with no real-time loss, rekey
  under traffic, a path removed and re-added, a replayed handshake, fallback to direct
  internet, and redundant-mode dedup.
- **Real kernel** (`test/netns/e2e.sh`, three network namespaces on an arm64 Linux VM).
  The links are netem and tbf: wan1 20 Mbit/s up, 40 down, 10 ms; wan2 10/20 Mbit/s,
  40 ms, with its default route only in a side table the way GL's multi-WAN leaves a
  standby WAN. Last run:

  | Check | Result |
  | --- | --- |
  | Both WANs found and up | 200 ms |
  | One TCP download (wan1 alone: 40, both: 60 Mbit/s) | 51.2 Mbit/s average, about 58 steady |
  | One TCP upload (wan1 alone: 20, both: 30) | 26.6 average, about 29 steady |
  | Four-stream download | 53.2 |
  | wan1 unplugged mid-download, pings every 20 ms | 0 of 350 lost |
  | wan1 back | up again in 1.3–2.9 s |
  | Server killed | LAN falls back to direct internet in about 6 s |

- **OpenWrt 21.02.7 x86_64 containers** (procd, netifd, fw3, rpcd, LuCI):
  - install and removal;
  - first-run setup;
  - config generation for both roles;
  - the firewall rule;
  - SIGHUP versus restart reloads;
  - a WAN joining and leaving through hotplug;
  - both LuCI pages;
  - a tunnel to the container image.

Not tested yet:

- any GL.iNet hardware or firmware;
- any non-x86 binary on a real device;
- a real modem, tethering or hotel network;
- the arm64 container image;
- the OpenWrt SDK Makefiles.

## Layout

| Path | What |
| --- | --- |
| `cmd/weaver` | the CLI: `run`, `genkey`, `pubkey`, `status`, `version` |
| `internal/` | `proto`, `handshake`, `replay`, `bond`, `engine`, `platform`, `config`, `control` |
| `openwrt/` | the `weaver` and `luci-app-weaver` packages |
| `scripts/` | cross-compiling and SDK-free ipk building |
| `Dockerfile`, `container/` | the server image, its entrypoint and a compose example |
| `test/netns/` | the real-kernel end-to-end test |

## License

MIT. See [LICENSE](LICENSE).
