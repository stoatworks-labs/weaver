# luci-app-weaver for the OpenWrt SDK / buildroot (needs the luci feed).
# scripts/build-ipk.sh makes the same package without an SDK.

include $(TOPDIR)/rules.mk

LUCI_TITLE:=LuCI support for weaver
LUCI_DESCRIPTION:=Status and settings pages for weaver, the multi-WAN bonding tunnel.
LUCI_DEPENDS:=+luci-base +weaver
LUCI_PKGARCH:=all

PKG_LICENSE:=MIT
PKG_MAINTAINER:=Stoatworks Labs

include $(TOPDIR)/feeds/luci/luci.mk

# call BuildPackage - OpenWrt buildroot signature
