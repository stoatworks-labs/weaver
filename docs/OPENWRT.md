# weaver on OpenWrt and GL.iNet

Two packages:

- `weaver_<version>_<arch>.ipk`: the daemon, its procd init script, the UCI config
  (`/etc/config/weaver`), the first-run setup and a hotplug hook. Depends on `kmod-tun`.
- `luci-app-weaver_<version>_all.ipk`: a LuCI status page and settings page under
  **Services > Weaver**. Optional; everything works over SSH and `uci` too.

They target OpenWrt 21.02 and later, which covers GL.iNet firmware 4.x. Nothing here has
run on GL.iNet hardware yet: what was tested, and how, is under
[What has been tested](#what-has-been-tested).

## Building the packages

No OpenWrt SDK is needed. The binary is static Go, so one build serves every target
with the same CPU architecture.

```sh
scripts/build.sh                      # dist/weaver-<arch> for every supported arch
scripts/build-ipk.sh                  # dist/*.ipk for every binary built
scripts/build.sh aarch64_cortex-a53   # or just the ones you need
scripts/build-ipk.sh aarch64_cortex-a53
```

The version comes from `git describe`. Set `VERSION=` to override it, or
`SOURCE_DATE_EPOCH=` to pin the timestamps inside the packages.

| OpenWrt arch | Go build | Examples |
| --- | --- | --- |
| `aarch64_cortex-a53` | arm64 | GL.iNet Flint 2 (MT6000), Beryl AX (MT3000), MT7981/MT7986 boards |
| `aarch64_generic` | arm64 | armsr/armvirt 64-bit (VMs) |
| `arm_cortex-a7_neon-vfpv4` | arm, GOARM=7 | IPQ40xx boards |
| `arm_cortex-a7` | arm, GOARM=7 with soft float | MT7629 and other FPU-less Cortex-A7 targets |
| `mipsel_24kc` | mipsle, soft float | MT7621, MT7628 boards |
| `mips_24kc` | mips, soft float | ath79 boards |
| `x86_64` | amd64 | PCs, VMs |

Check the router's arch rather than guessing from the model: on the router, the last
line of `opkg print-architecture` is the one to build for.

An arch that isn't in the table but shares a Go target with one that is (for example
`aarch64_cortex-a72` on a Raspberry Pi 4) takes the same binary under its own name:

```sh
cp dist/weaver-aarch64_generic dist/weaver-aarch64_cortex-a72
scripts/build-ipk.sh aarch64_cortex-a72
```

To build inside an OpenWrt buildroot or SDK instead, copy `openwrt/weaver` and
`openwrt/luci-app-weaver` into a feed. `go.mod` asks for Go 1.26, far newer than the Go
in 21.02's packages feed, so replace `feeds/packages/lang/golang` with the one from
packages `master` first. `make package/weaver/compile USE_SOURCE_DIR=$PWD` builds from a
checkout instead of the release tag.

## Installing on a GL.iNet router

GL.iNet routers run OpenWrt with SSH enabled. The root password is the one you set for
the admin panel, and the router is `192.168.8.1` unless you changed it.

```sh
# On your computer. -O: the router's SSH server has no SFTP, which scp uses by default.
scp -O dist/weaver_*_aarch64_cortex-a53.ipk dist/luci-app-weaver_*_all.ipk \
    root@192.168.8.1:/tmp/

ssh root@192.168.8.1
opkg print-architecture     # confirm the arch matches the file you copied
opkg update                 # weaver needs kmod-tun from the feed
opkg install /tmp/weaver_*.ipk /tmp/luci-app-weaver_*.ipk
```

On install, the first-run script (`/etc/uci-defaults/90-weaver`, removed once it has
run):

- generates a private key into `weaver.main.private_key`;
- adds a netifd interface `weaver` (`proto none`, device `weaver0`);
- adds a firewall zone `weaver` (accept, masquerade, MSS clamping) with forwardings
  `lan → weaver` and `weaver → wan`;
- adds a firewall rule `Allow-Weaver` (udp/9328 from wan), disabled. The init script
  turns it on while the router is a server and off otherwise.

weaver ships disabled (`weaver.main.enabled=0`), so nothing changes in the routing until
you configure it.

### LuCI on GL.iNet firmware 4.x

GL's admin panel is its own interface; LuCI sits alongside it.

- Firmware 4.6 and earlier: in the admin panel, **SYSTEM > Advanced Settings**, then
  the link to LuCI. On models that don't ship LuCI, the same page offers to install it.
- Firmware 4.7 and later: LuCI is served on port 8080, at `http://192.168.8.1:8080/`.

Log in as `root` with the admin password. weaver's pages are under **Services > Weaver**.
If they don't appear straight after installing `luci-app-weaver`, log out and in again.

## Client: the travel router

The client bonds its WANs into a tunnel to one server. It needs the server's address
and public key, and the server needs the client's public key. Show this router's key:

```sh
uci get weaver.main.private_key | weaver pubkey
```

Then:

```sh
uci set weaver.main.role='client'
uci set weaver.main.server='vps.example.net:9328'
uci set weaver.main.server_public_key='<the server public key>'
uci set weaver.main.address='10.77.0.2/24'    # this client's address in the tunnel
uci set weaver.main.enabled='1'
uci commit weaver
/etc/init.d/weaver reload
```

Always finish with `/etc/init.d/weaver reload` (LuCI's Save & Apply does the same).
`reload_config` doesn't reliably reach weaver the first time it runs after boot.

With `auto_paths` on (the default), every interface that is up and has an IPv4 default
route, in any routing table, becomes a primary path: `wan`, `wwan` (Wi-Fi repeater),
`tethering`, a modem interface. These are left out:

- the LAN bridge;
- loopback;
- weaver itself;
- VPN interfaces (WireGuard, OpenVPN, ZeroTier, Tailscale, GRE and similar).

Add `path` sections only to change what auto-detection picks:

```sh
# Tethering only for overflow, and probed less while idle (it's metered).
uci add weaver path
uci set weaver.@path[-1].interface='tethering'
uci set weaver.@path[-1].priority='secondary'
uci set weaver.@path[-1].metered='1'

# The modem only when nothing else is up.
uci add weaver path
uci set weaver.@path[-1].interface='modem_1_1_2'
uci set weaver.@path[-1].priority='backup'

# Never bond over the repeater link.
uci add weaver path
uci set weaver.@path[-1].interface='wwan'
uci set weaver.@path[-1].priority='disabled'

uci commit weaver
/etc/init.d/weaver reload
```

`interface` is the netifd interface name: **Network > Interfaces** in LuCI, or
`ubus list network.interface.*`. It isn't the device name. With `auto_paths` off, only
`path` sections are used. The daemon then waits until at least one of them is up.

A WAN coming up later joins the bond, and one going down leaves it: this happens
through the hotplug hook, without restarting the daemon or dropping the session. Examples
are a hotel network or a modem that registers late.

Routing options:

| Option | Default | |
| --- | --- | --- |
| `default_route` | `1` | Route all LAN traffic through the tunnel (`0.0.0.0/1` and `128.0.0.0/1` via `weaver0`) |
| `list subnet` | none | Route only these subnets through the tunnel, or these as well |
| `fallback_direct` | `1` | When every path has been down for 5 s, remove the tunnel routes so the LAN reaches the internet directly; restore them when the tunnel is back |
| `mode` | `adaptive` | `adaptive`, `bond`, `redundant` or `failover` (see the README) |
| `list exclude_device` | none | Devices auto-detection must never use, in addition to the LAN bridge |

## Server: a router at home

A server can be a VPS container ([CONTAINER.md](CONTAINER.md)) or an OpenWrt router
with a public address, or a port forward to one:

```sh
uci set weaver.main.role='server'
uci set weaver.main.listen_port='9328'
uci set weaver.main.address='10.77.0.1/24'

# One peer section per client router.
uci add weaver peer
uci set weaver.@peer[-1].name='flint'
uci set weaver.@peer[-1].public_key='<the client public key>'
uci set weaver.@peer[-1].address='10.77.0.2'   # the client's tunnel address, no prefix

uci set weaver.main.enabled='1'
uci commit weaver
/etc/init.d/weaver reload
```

The peer's `address` has to be a bare IPv4 address inside the server's tunnel subnet.
The init script skips a peer without a public key or address, and logs a warning.

In LuCI, the **Peers** table's editor has a **Generate key pair** button for a new
client. It fills in the public key and shows the private key once, with the `uci`
commands to paste on the client router. The private key is not stored on the server.

Server-side routing: `listen_port` is opened from wan by the `Allow-Weaver` rule, and
the `weaver → wan` forwarding lets clients reach the internet through the server.
Behind another NAT (an ISP router), forward udp/9328 to this router as well.

## Firewall notes

- The `weaver` zone masquerades. Traffic a client sends into the tunnel leaves with the
  client's tunnel address (`10.77.0.2`), so the server needs no route back to the
  client's LAN, and peers need no `allowed_ip`.
- To reach a client's LAN from the server side:
  - on the client: `uci set firewall.weaver.masq='0'`, plus a forwarding
    `weaver → lan`;
  - on the server: add the client's LAN as the peer's `list allowed_ip`, e.g.
    `192.168.8.0/24`.
- `Allow-Weaver` is managed by the init script: opened while enabled with role server,
  closed otherwise. Removing the package closes it too. Don't edit it by hand; change
  `listen_port` instead.

## GL.iNet multi-WAN, and why weaver copes with it

GL firmware 4.x has its own multi-WAN (**NETWORK > Multi-WAN**), with a **Failover** and
a **Load Balance** mode. It watches each WAN with pings and moves the router's default
route to the best one. In failover mode the main routing table holds only the active
WAN's default route.

weaver's paths don't depend on that main-table default route:

- **Binding.** Each path's socket is bound to its device (`SO_BINDTODEVICE`) and its
  packets carry their own fwmark (`0x57450000 + path id`).
- **Routing.** For each path, weaver finds the device's default route in whichever
  routing table has it and copies it into a table of its own (`5745 + path id`).
  An `ip rule` at priority 5745 sends that path's fwmark to that table, so a standby
  WAN still carries its path while GL's failover points the main table elsewhere.
- **Precedence.** The tunnel's `0.0.0.0/1` and `128.0.0.0/1` routes are more specific
  than any default route, so LAN traffic goes into the tunnel whichever WAN GL has made
  the default. The WAN default routes stay in place, so when weaver stops or
  `fallback_direct` kicks in, GL's own failover is what's left.

Recommended: leave GL's multi-WAN in **Failover** (its default). weaver does the
bonding. GL's failover still decides how the router's own traffic and the
fallback-direct case go out.

Load Balance mode marks connections and routes them by its own policy rules. That
interacts with weaver's marks and rules in ways that have not been tested. If paths
stay down under Load Balance, switch GL back to Failover.

Don't run GL's own VPN client (WireGuard or OpenVPN) at the same time. Its policy
routing and kill switch compete with weaver for the LAN's default route.

Diagnostics:

```sh
weaver status                     # paths, RTT, loss, rates (or the LuCI status page)
logread -e weaver                 # daemon and init script messages
ip rule                           # expect "fwmark 0x5745000N lookup 574(5+N)" per path
ip route show table all | grep -E 'weaver0|table 574'
cat /var/run/weaver/weaver.json   # the config the daemon was given (contains the key)
/etc/init.d/weaver genconf        # regenerate it without starting anything
```

The daemon writes its log to stdout, which procd files at `daemon.info` whatever the
line's own level is. Read the `level=` field for that. Only a failure to start lands as
`daemon.err`.

## Removing

```sh
opkg remove luci-app-weaver weaver
```

That stops weaver, removes its routes and rules, and closes `Allow-Weaver`. These stay
behind, so that a reinstall keeps the same key and firewall setup:

- `/etc/config/weaver`, holding the private key;
- the `weaver` network interface;
- the `weaver` firewall zone, its two forwardings and the disabled rule.

To remove those too:

```sh
rm -f /etc/config/weaver /etc/config/weaver-opkg
uci -q delete network.weaver
uci -q delete firewall.weaver
for s in $(uci -q -X show firewall | sed -n "s/^firewall\.\([^.]*\)\.name='Allow-Weaver'$/\1/p"); do
	uci delete "firewall.$s"
done
for s in $(uci -q -X show firewall | sed -n 's/^firewall\.\([^.]*\)=forwarding$/\1/p'); do
	case "$(uci -q get firewall.$s.src)-$(uci -q get firewall.$s.dest)" in
	lan-weaver | weaver-wan) uci delete "firewall.$s" ;;
	esac
done
uci commit network; uci commit firewall
/etc/init.d/network reload; /etc/init.d/firewall reload
```

## What has been tested

All of this ran in an OpenWrt 21.02.7 x86_64 rootfs container, with procd as PID 1 and
netifd, fw3, rpcd, uhttpd and LuCI running:

- Installing both packages with opkg, which pulled in `kmod-tun`.
- The first-run setup, including running it a second time.
- Generating the config for client and server, with auto paths and with `path`
  sections.
- The firewall rule following the role.
- A tunnel to a server in a container built from this repo's Dockerfile.
- Reload behaviour: a mode change applied by SIGHUP; an MTU change restarting the
  daemon.
- A second WAN joining and leaving through the hotplug hook.
- Both LuCI pages.
- Removal.

Not tested:

- Any GL.iNet hardware or firmware: its multi-WAN, a real modem or tethering
  interface.
- Any non-x86 package on a real router.
