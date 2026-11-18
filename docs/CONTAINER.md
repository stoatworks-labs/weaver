# weaver server in a container

The usual server is a small VPS. Clients' bonded traffic comes out of it to the
internet. The image is built from this repo's `Dockerfile`:

- Alpine 3.22 with `iptables` and `iproute2`;
- the static `weaver` binary;
- an entrypoint that turns on forwarding and NAT for the tunnel, then runs
  `weaver run` in the foreground.

No image is published to a registry yet, so build it on the VPS or next to it.

## What the host needs

- Linux with the `tun` module. `/dev/net/tun` must exist; it does on most VPS
  kernels.
- Docker, or another runtime that can grant `NET_ADMIN` and pass a device through.
- udp/9328 open in the provider's firewall or security group. Docker publishes the
  port itself, but a cloud firewall in front of the VPS still has to let it through.

## Build

```sh
git clone https://github.com/stoatworks-labs/weaver.git && cd weaver
docker build -t weaver .
```

For an arm64 VPS built on another machine:

```sh
docker buildx build --platform linux/arm64 -t weaver --load .
```

The Go stage cross-compiles natively, so only the small runtime stage runs under
emulation. To carry the image across, `docker save weaver | ssh vps docker load` works.

## Run

```sh
docker volume create weaver-data
docker run -d --name weaver --restart unless-stopped \
  --cap-add NET_ADMIN --device /dev/net/tun \
  --sysctl net.ipv4.ip_forward=1 \
  -p 9328:9328/udp \
  -v weaver-data:/var/lib/weaver \
  -e WEAVER_ROLE=server \
  -e WEAVER_ADDRESS=10.77.0.1/24 \
  weaver
```

Or with compose. Copy and edit `container/compose.example.yml`, then:

```sh
docker compose -f container/compose.example.yml up -d --build
```

The private key is generated into the volume on first start:
`/var/lib/weaver/private.key`, mode 0600. Keep the volume. A new key means telling
every client the new public key.

`--privileged` and `--network host` are not needed. On the default bridge network,
`weaver0`, the NAT rule and the forwarding setting all live in the container's own
network namespace, and go away with it.

## The server's public key

Clients need it as `server_public_key`. It's in the log on every start:

```sh
docker logs weaver 2>&1 | grep public_key
```

```text
weaver: generated a private key in /var/lib/weaver/private.key; public key SkN1N2UJ...EGE=
time=... level=INFO msg="server listening" port=9328 public_key="SkN1N2UJ...EGE=" peers=0
```

Or ask the running daemon:

```sh
docker exec weaver weaver status
```

## Adding clients

Each client router has its own key pair. On the client, `uci get weaver.main.private_key
| weaver pubkey` prints its public key (see [OPENWRT.md](OPENWRT.md)). Give each client
a tunnel address in the server's subnet and list them in `WEAVER_PEERS`, one per line or
separated by `;`:

```text
name,public key,tunnel address[,allowed ip...]
```

```sh
docker rm -f weaver
docker run -d --name weaver --restart unless-stopped \
  --cap-add NET_ADMIN --device /dev/net/tun \
  --sysctl net.ipv4.ip_forward=1 \
  -p 9328:9328/udp \
  -v weaver-data:/var/lib/weaver \
  -e WEAVER_ROLE=server \
  -e WEAVER_ADDRESS=10.77.0.1/24 \
  -e WEAVER_PEERS='flint,<flint public key>,10.77.0.2;beryl,<beryl public key>,10.77.0.3' \
  weaver
```

With compose, edit `WEAVER_PEERS` and run `docker compose ... up -d`, which recreates the
container. The key stays in the volume either way.

Some rules about peers:

- An environment config can't be reloaded in place. weaver ignores SIGHUP when its
  config comes from the environment, so adding a peer means recreating the container.
  Connected clients reconnect within a few seconds.
- The tunnel address is a bare IPv4 address, without a prefix.
- `allowed ip` entries are only needed to route to a subnet behind a client, such as
  its LAN, when that client doesn't masquerade. GL.iNet and OpenWrt clients set up by
  the weaver package do masquerade.
- An invalid key in `WEAVER_PEERS` stops weaver from starting. The container then
  restarts in a loop until the key is fixed, and `docker logs` says which peer is
  wrong. Start the server once without peers to read its own key first.

## JSON config instead of environment

For options the environment doesn't cover (timings, `rekey_interval_s` and so on),
mount a config file at `/etc/weaver/weaver.json`. The format is in
[DESIGN.md](DESIGN.md#configuration). The entrypoint prefers this file whenever it
exists:

```sh
install -m 0600 /dev/null /srv/weaver/weaver.json   # then write the config into it
docker run -d --name weaver --restart unless-stopped \
  --cap-add NET_ADMIN --device /dev/net/tun --sysctl net.ipv4.ip_forward=1 \
  -p 9328:9328/udp -v /srv/weaver:/etc/weaver:ro \
  weaver
```

The file holds the private key, so keep it 0600. After editing it, run
`docker kill -s HUP weaver`:

- mode and timing changes apply in place;
- a change that needs a restart makes weaver exit, and the restart policy starts it
  again with the new file. That includes adding or removing peers.

## NAT and forwarding

The entrypoint, before starting weaver:

- sets `net.ipv4.ip_forward=1` if it can. `--sysctl` already does that; without it the
  entrypoint warns, because clients would reach the server but not the internet;
- masquerades the tunnel subnet out of the container's default-route interface. The
  subnet is `WEAVER_ADDRESS`'s, or `tun.address` from the JSON file; set
  `WEAVER_MASQ_SUBNET` to use another;
- clamps TCP MSS on forwarded SYNs to the path MTU, since the tunnel's MTU (1380) is
  smaller than the internet's;
- uses `iptables`, which is the nftables backend on Alpine, and falls back to
  `iptables-legacy`.

Both rules are checked with `iptables -C` first, so a restarted container doesn't add
them twice. On the default bridge network, Docker then NATs the container to the host's
address as usual.

`--network host` works too, but then `weaver0`, the masquerade rule and the MSS rule are
added to the host itself. The two iptables rules stay there after the container stops.

## Checking on it

```sh
docker exec weaver weaver status          # sessions and paths, as a table
docker exec weaver weaver status --json   # the same, for scripts
docker logs -f weaver
```

Path state changes are logged at `info`; set `WEAVER_LOG_LEVEL=debug` to see each
handshake too.

## Running a client in a container

The image can be a client too: `WEAVER_ROLE=client`, `WEAVER_SERVER=host:port` and
`WEAVER_SERVER_PUBLIC_KEY`. A client bonds the host's WANs, though, so it needs
`--network host` to see them. It also adds policy routes to the host. That setup hasn't
been tested. The OpenWrt package is the supported client.

## What has been tested

On Docker 29 (amd64):

- The image built from this Dockerfile, run as above.
- The entrypoint's forwarding, masquerade and MSS clamping.
- Key generation, with the public key in the log.
- `weaver status` through `docker exec`.
- A client in an OpenWrt 21.02 container bonding two paths to it.

The CI workflow builds an arm64 image as well. That workflow hasn't run yet, and no
arm64 image has been run.
