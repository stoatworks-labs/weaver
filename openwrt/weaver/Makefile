# weaver for the OpenWrt SDK / buildroot. See docs/OPENWRT.md.
#
# Needs the packages feed for lang/golang, with a Go new enough for go.mod
# (go 1.26): OpenWrt 21.02's own feed is far older, so update
# feeds/packages/lang/golang from packages master first. scripts/build-ipk.sh
# makes the same packages without an SDK.
#
# To build from a local checkout instead of the tag:
#   make package/weaver/compile USE_SOURCE_DIR=/path/to/weaver

include $(TOPDIR)/rules.mk

PKG_NAME:=weaver
PKG_VERSION:=0.1.0
PKG_RELEASE:=1

PKG_SOURCE_PROTO:=git
PKG_SOURCE_URL:=https://github.com/stoatworks-labs/weaver.git
PKG_SOURCE_VERSION:=v$(PKG_VERSION)
PKG_MIRROR_HASH:=skip

PKG_LICENSE:=MIT
PKG_LICENSE_FILES:=LICENSE
PKG_MAINTAINER:=Stoatworks Labs

PKG_BUILD_DEPENDS:=golang/host
PKG_BUILD_PARALLEL:=1
# 21.02 spelling, then 22.03 and later.
PKG_USE_MIPS16:=0
PKG_BUILD_FLAGS:=no-mips16

GO_PKG:=github.com/stoatworks-labs/weaver
GO_PKG_BUILD_PKG:=$(GO_PKG)/cmd/weaver
GO_PKG_LDFLAGS_X:=main.version=$(PKG_VERSION)

include $(INCLUDE_DIR)/package.mk
include $(TOPDIR)/feeds/packages/lang/golang/golang-package.mk

define Package/weaver
  SECTION:=net
  CATEGORY:=Network
  SUBMENU:=VPN
  TITLE:=Multi-WAN bonding tunnel
  URL:=https://github.com/stoatworks-labs/weaver
  DEPENDS:=$(GO_ARCH_DEPENDS) +kmod-tun
endef

define Package/weaver/description
  Bonds every WAN a router has (Ethernet, hotel Wi-Fi, USB tethering, a
  cellular modem) into one encrypted UDP tunnel, per packet, with sub-second
  failover. The same binary is the client on a travel router or the server on
  a router at home or a VPS.
endef

define Package/weaver/conffiles
/etc/config/weaver
endef

# Becomes prerm-pkg, which default_prerm runs on removal: close the
# Allow-Weaver rule a server opened.
define Package/weaver/prerm
#!/bin/sh
[ -n "$${IPKG_INSTROOT}" ] || /etc/init.d/weaver closefw
exit 0
endef

define Package/weaver/install
	$(call GoPackage/Package/Install/Bin,$(PKG_INSTALL_DIR))

	$(INSTALL_DIR) $(1)/usr/bin
	$(INSTALL_BIN) $(PKG_INSTALL_DIR)/usr/bin/weaver $(1)/usr/bin/
	$(INSTALL_DIR) $(1)/etc/config
	$(INSTALL_CONF) ./files/weaver.config $(1)/etc/config/weaver
	$(INSTALL_DIR) $(1)/etc/init.d
	$(INSTALL_BIN) ./files/weaver.init $(1)/etc/init.d/weaver
	$(INSTALL_DIR) $(1)/etc/uci-defaults
	$(INSTALL_BIN) ./files/weaver.defaults $(1)/etc/uci-defaults/90-weaver
	$(INSTALL_DIR) $(1)/etc/hotplug.d/iface
	$(INSTALL_DATA) ./files/weaver.hotplug $(1)/etc/hotplug.d/iface/90-weaver
endef

$(eval $(call GoBinPackage,weaver))
$(eval $(call BuildPackage,weaver))
