#!/bin/sh
# Cross-compile static weaver binaries for the OpenWrt package architectures.
#
#   scripts/build.sh                 every architecture below
#   scripts/build.sh x86_64 ...      only the named ones
#
# Output: dist/weaver-<openwrt-arch> and dist/VERSION (read by build-ipk.sh, so
# the binaries and the packages always carry the same version string).
# VERSION in the environment overrides the one derived from git.
set -eu

ROOT=$(cd "$(dirname "$0")/.." && pwd)
DIST="$ROOT/dist"

# OpenWrt package arch -> GOARCH and its variant setting. The arch names are
# the ones `opkg print-architecture` prints on the router; the GL-MT6000
# (Flint 2) is aarch64_cortex-a53.
#
# arm_cortex-a7 is OpenWrt's FPU-less Cortex-A7 profile (its own golang
# package builds it as GOARM=5). GOARM=7,softfloat keeps the ARMv7
# instructions without emitting VFP ones, so it still runs where the kernel
# has no VFP support.
ALL_ARCHES="aarch64_cortex-a53 aarch64_generic arm_cortex-a7_neon-vfpv4 arm_cortex-a7 mipsel_24kc mips_24kc x86_64"

goenv() {
	case "$1" in
	aarch64_cortex-a53|aarch64_generic) echo "GOARCH=arm64" ;;
	arm_cortex-a7_neon-vfpv4) echo "GOARCH=arm GOARM=7" ;;
	arm_cortex-a7) echo "GOARCH=arm GOARM=7,softfloat" ;;
	mipsel_24kc) echo "GOARCH=mipsle GOMIPS=softfloat" ;;
	mips_24kc) echo "GOARCH=mips GOMIPS=softfloat" ;;
	x86_64) echo "GOARCH=amd64" ;;
	*) return 1 ;;
	esac
}

# Version: a tag gives "0.2.0" (or "0.2.0-3-gabc1234[-dirty]" past it); with no
# tag at all it is "0.1.0-dev" plus the commit, so two dev builds can still be
# told apart. opkg versions must start with a digit, so a leading "v" goes.
version() {
	if [ -n "${VERSION:-}" ]; then
		echo "$VERSION"
		return
	fi
	d=$(git -C "$ROOT" describe --tags --always --dirty 2>/dev/null) || d=""
	case "$d" in
	"") echo "0.1.0-dev" ;;
	v[0-9]*) echo "${d#v}" ;;
	[0-9]*.*) echo "$d" ;;
	*) echo "0.1.0-dev+$d" ;;
	esac
}

VER=$(version)
ARCHES=${*:-$ALL_ARCHES}

for a in $ARCHES; do
	goenv "$a" >/dev/null || {
		echo "build.sh: unknown architecture '$a' (known: $ALL_ARCHES)" >&2
		exit 2
	}
done

mkdir -p "$DIST"
printf '%s\n' "$VER" >"$DIST/VERSION"

for a in $ARCHES; do
	out="$DIST/weaver-$a"
	echo "build: $a ($(goenv "$a")) -> dist/weaver-$a"
	# shellcheck disable=SC2046 # word splitting of KEY=VALUE pairs is intended
	(cd "$ROOT" && env CGO_ENABLED=0 GOOS=linux $(goenv "$a") \
		go build -trimpath -ldflags "-s -w -X main.version=$VER" -o "$out" ./cmd/weaver)
done

echo "build: version $VER"
