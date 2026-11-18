#!/bin/sh
# Build OpenWrt .ipk packages without the OpenWrt SDK.
#
#   scripts/build.sh && scripts/build-ipk.sh          every dist/weaver-<arch>
#   scripts/build-ipk.sh x86_64 aarch64_cortex-a53    only those
#
# Output in dist/:
#   weaver_<ver>_<arch>.ipk       one per binary built by build.sh
#   luci-app-weaver_<ver>_all.ipk
#
# The layout is what OpenWrt 21.02's scripts/ipkg-build writes: an outer
# gzipped tar (not an ar archive) holding ./debian-binary, ./data.tar.gz and
# ./control.tar.gz in that order, GNU tar format, every entry root:root with a
# ./ prefix, one fixed mtime, gzip without a name or timestamp. Control fields,
# conffiles, postinst and prerm follow include/package-ipkg.mk: the generic
# postinst/prerm call default_postinst/default_prerm from /lib/functions.sh,
# and package-specific steps go in postinst-pkg, which default_postinst runs.
#
# Works with GNU tar (Linux, CI) and bsdtar (macOS). TAR=gtar to pick one.
set -eu
umask 022

ROOT=$(cd "$(dirname "$0")/.." && pwd)
DIST="$ROOT/dist"
OWRT="$ROOT/openwrt"
TAR=${TAR:-tar}

die() {
	echo "build-ipk.sh: $*" >&2
	exit 1
}

[ -f "$DIST/VERSION" ] || die "no dist/VERSION: run scripts/build.sh first"
VER=$(cat "$DIST/VERSION")

if [ $# -gt 0 ]; then
	ARCHES="$*"
else
	ARCHES=""
	for f in "$DIST"/weaver-*; do
		[ -f "$f" ] || continue
		ARCHES="$ARCHES ${f##*/weaver-}"
	done
	[ -n "$ARCHES" ] || die "no dist/weaver-<arch> binaries: run scripts/build.sh first"
fi

# One timestamp for every entry: SOURCE_DATE_EPOCH, else the last commit.
EPOCH=${SOURCE_DATE_EPOCH:-$(git -C "$ROOT" log -1 --format=%ct 2>/dev/null || date +%s)}
STAMP=$(date -u -d "@$EPOCH" +%Y%m%d%H%M.%S 2>/dev/null ||
	date -u -r "$EPOCH" +%Y%m%d%H%M.%S 2>/dev/null) ||
	die "cannot format SOURCE_DATE_EPOCH $EPOCH"

case "$("$TAR" --version 2>/dev/null)" in
*"GNU tar"*) TAR_KIND=gnu ;;
*bsdtar*) TAR_KIND=bsd ;;
*) die "$TAR is neither GNU tar nor bsdtar" ;;
esac

WORK=$(mktemp -d "${TMPDIR:-/tmp}/weaver-ipk.XXXXXX")
trap 'rm -rf "$WORK"' EXIT INT TERM

# tar_c LIST: an uncompressed tar of the names in LIST (relative to the
# current directory, no recursion) on stdout, every entry root:root.
MAC_FLAG=""
[ "$(uname -s)" = Darwin ] && MAC_FLAG="--no-mac-metadata"
tar_c() {
	if [ "$TAR_KIND" = gnu ]; then
		"$TAR" --format=gnu --no-recursion --owner=root:0 --group=root:0 \
			-cf - -T "$1"
	else
		# COPYFILE_DISABLE and --no-mac-metadata keep macOS from adding
		# ._AppleDouble entries for extended attributes.
		# shellcheck disable=SC2086 # MAC_FLAG is empty or one flag
		COPYFILE_DISABLE=1 "$TAR" --format=gnutar -n --uid 0 --gid 0 \
			--uname root --gname root --no-xattrs --no-acls --no-fflags $MAC_FLAG \
			-cf - -T "$1"
	fi
}

# mktgz DIR OUT: gzipped tar of everything under DIR, entries "./...",
# sorted, root:root, mtime = EPOCH.
mktgz() {
	(
		cd "$1"
		find . -exec env TZ=UTC0 touch -h -t "$STAMP" {} +
		find . | LC_ALL=C sort >"$WORK/list"
		tar_c "$WORK/list"
	) | gzip -n >"$2"
}

# mkipk STAGE OUT: STAGE holds data/ and CONTROL/ (control without
# Installed-Size, which ipkg-build fills in with the size of data.tar.gz).
mkipk() {
	stage=$1 out=$2
	pkg="$stage/pkg"
	mkdir -p "$pkg"
	find "$stage/data" "$stage/CONTROL" -type d -exec chmod 0755 {} +
	mktgz "$stage/data" "$pkg/data.tar.gz"
	size=$(wc -c <"$pkg/data.tar.gz" | tr -d ' ')
	sed -i.bak "s/^Installed-Size: .*/Installed-Size: $size/" "$stage/CONTROL/control"
	rm -f "$stage/CONTROL/control.bak"
	chmod 0644 "$stage/CONTROL/control"
	mktgz "$stage/CONTROL" "$pkg/control.tar.gz"
	echo "2.0" >"$pkg/debian-binary"
	(
		cd "$pkg"
		for f in debian-binary data.tar.gz control.tar.gz; do
			env TZ=UTC0 touch -t "$STAMP" "$f"
		done
		printf './debian-binary\n./data.tar.gz\n./control.tar.gz\n' >"$WORK/list"
		tar_c "$WORK/list"
	) | gzip -n >"$out"
	echo "ipk: ${out#"$ROOT"/} ($(wc -c <"$out" | tr -d ' ') bytes)"
}

# inst MODE SRC DEST
inst() {
	mkdir -p "$(dirname "$3")"
	cp "$2" "$3"
	chmod "$1" "$3"
}

# The two maintainer scripts package-ipkg.mk writes for every package.
generic_scripts() {
	cat >"$1/postinst" <<'EOF'
#!/bin/sh
[ "${IPKG_NO_SCRIPT}" = "1" ] && exit 0
[ -s ${IPKG_INSTROOT}/lib/functions.sh ] || exit 0
. ${IPKG_INSTROOT}/lib/functions.sh
default_postinst $0 $@
EOF
	cat >"$1/prerm" <<'EOF'
#!/bin/sh
[ -s ${IPKG_INSTROOT}/lib/functions.sh ] || exit 0
. ${IPKG_INSTROOT}/lib/functions.sh
default_prerm $0 $@
EOF
	chmod 0755 "$1/postinst" "$1/prerm"
}

# --- weaver, one per architecture ------------------------------------------

F="$OWRT/weaver/files"
for arch in $ARCHES; do
	bin="$DIST/weaver-$arch"
	[ -f "$bin" ] || die "missing $bin (scripts/build.sh $arch)"
	s="$WORK/weaver-$arch"
	d="$s/data"
	c="$s/CONTROL"
	mkdir -p "$d" "$c"

	inst 0755 "$bin" "$d/usr/bin/weaver"
	# 0600: the private key lives in this file.
	inst 0600 "$F/weaver.config" "$d/etc/config/weaver"
	inst 0755 "$F/weaver.init" "$d/etc/init.d/weaver"
	inst 0755 "$F/weaver.defaults" "$d/etc/uci-defaults/90-weaver"
	inst 0644 "$F/weaver.hotplug" "$d/etc/hotplug.d/iface/90-weaver"

	cat >"$c/control" <<EOF
Package: weaver
Version: $VER
Depends: libc, kmod-tun
Source: openwrt/weaver
SourceName: weaver
License: MIT
LicenseFiles: LICENSE
Section: net
SourceDateEpoch: $EPOCH
Maintainer: Stoatworks Labs
Architecture: $arch
Installed-Size: 0
Description:  Bonds every WAN a router has (Ethernet, hotel Wi-Fi, USB tethering, a
 cellular modem) into one encrypted UDP tunnel, per packet, with sub-second
 failover. The same binary is the client on a travel router or the server on a
 router at home or a VPS.
EOF
	echo /etc/config/weaver >"$c/conffiles"
	chmod 0644 "$c/conffiles"
	generic_scripts "$c"
	# default_prerm runs this before it stops and disables the service. opkg
	# runs prerm only on removal, not on upgrade.
	cat >"$c/prerm-pkg" <<'EOF'
[ -n "${IPKG_INSTROOT}" ] || /etc/init.d/weaver closefw
exit 0
EOF
	chmod 0755 "$c/prerm-pkg"

	mkipk "$s" "$DIST/weaver_${VER}_${arch}.ipk"
done

# --- luci-app-weaver ---------------------------------------------------------

L="$OWRT/luci-app-weaver"
s="$WORK/luci-app-weaver"
d="$s/data"
c="$s/CONTROL"
mkdir -p "$d" "$c"

for js in "$L"/htdocs/luci-static/resources/view/weaver/*.js; do
	inst 0644 "$js" "$d/www/luci-static/resources/view/weaver/${js##*/}"
done
inst 0644 "$L/root/usr/share/luci/menu.d/luci-app-weaver.json" "$d/usr/share/luci/menu.d/luci-app-weaver.json"
inst 0644 "$L/root/usr/share/rpcd/acl.d/luci-app-weaver.json" "$d/usr/share/rpcd/acl.d/luci-app-weaver.json"
inst 0755 "$L/root/usr/libexec/rpcd/luci.weaver" "$d/usr/libexec/rpcd/luci.weaver"

cat >"$c/control" <<EOF
Package: luci-app-weaver
Version: $VER
Depends: libc, luci-base, weaver
Source: openwrt/luci-app-weaver
SourceName: luci-app-weaver
License: MIT
LicenseFiles: LICENSE
Section: luci
SourceDateEpoch: $EPOCH
Maintainer: Stoatworks Labs
Architecture: all
Installed-Size: 0
Description:  LuCI status and settings pages for weaver, the multi-WAN bonding tunnel.
EOF
generic_scripts "$c"
# What luci.mk's default postinst does, so the new menu entry, ACL and rpcd
# plugin show up without a reboot: drop LuCI's caches (21.02 keeps one
# indexcache file, later releases one per hash) and HUP rpcd, which re-execs
# itself and rescans acl.d and libexec/rpcd. Skipped in an image build.
cat >"$c/postinst-pkg" <<'EOF'
[ -n "${IPKG_INSTROOT}" ] || {
	rm -f /tmp/luci-indexcache /tmp/luci-indexcache.*
	rm -rf /tmp/luci-modulecache/
	killall -HUP rpcd 2>/dev/null
	exit 0
}
EOF
cat >"$c/postrm" <<'EOF'
#!/bin/sh
[ -n "${IPKG_INSTROOT}" ] || {
	rm -f /tmp/luci-indexcache /tmp/luci-indexcache.*
	rm -rf /tmp/luci-modulecache/
	killall -HUP rpcd 2>/dev/null
}
exit 0
EOF
chmod 0755 "$c/postinst-pkg" "$c/postrm"

mkipk "$s" "$DIST/luci-app-weaver_${VER}_all.ipk"
