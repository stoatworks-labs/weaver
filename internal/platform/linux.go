//go:build linux

// Package platform is weaver's Linux side: the TUN device, sockets pinned to
// a WAN device, the tunnel's addresses and routes, and per-path policy
// routing, all through netlink and socket options. There is nothing to shell
// out to, so it behaves the same on OpenWrt, in a container and on a desktop.
package platform

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"syscall"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/stoatworks-labs/weaver/internal/engine"
)

// routeProtocol tags every route weaver installs, so discovery can tell
// weaver's copies from the WAN routes they were copied from.
const routeProtocol = netlink.RouteProtocol(87)

// TUN is a Linux TUN device without packet information headers.
type TUN struct {
	f    *os.File
	name string
}

func (t *TUN) Read(b []byte) (int, error)  { return t.f.Read(b) }
func (t *TUN) Write(b []byte) (int, error) { return t.f.Write(b) }
func (t *TUN) Close() error                { return t.f.Close() }

// Name is the kernel's name for the device.
func (t *TUN) Name() string { return t.name }

// OpenTUN creates (or attaches to) a TUN device, sets its MTU and address and
// brings it up.
func OpenTUN(name string, mtu int, addr netip.Prefix) (*TUN, error) {
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("opening /dev/net/tun (is kmod-tun installed?): %w", err)
	}
	ifr, err := unix.NewIfreq(name)
	if err != nil {
		unix.Close(fd)
		return nil, err
	}
	ifr.SetUint16(unix.IFF_TUN | unix.IFF_NO_PI)
	if err := unix.IoctlIfreq(fd, unix.TUNSETIFF, ifr); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("creating %s: %w", name, err)
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		unix.Close(fd)
		return nil, err
	}
	t := &TUN{f: os.NewFile(uintptr(fd), "/dev/net/tun"), name: ifr.Name()}

	link, err := netlink.LinkByName(t.name)
	if err != nil {
		t.Close()
		return nil, err
	}
	if err := netlink.LinkSetMTU(link, mtu); err != nil {
		t.Close()
		return nil, fmt.Errorf("setting MTU: %w", err)
	}
	ipnet := &net.IPNet{IP: addr.Addr().AsSlice(), Mask: net.CIDRMask(addr.Bits(), addr.Addr().BitLen())}
	if err := netlink.AddrReplace(link, &netlink.Addr{IPNet: ipnet}); err != nil {
		t.Close()
		return nil, fmt.Errorf("setting address %s: %w", addr, err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		t.Close()
		return nil, fmt.Errorf("bringing %s up: %w", t.name, err)
	}
	return t, nil
}

// Linux implements engine.Platform.
type Linux struct {
	tun string
}

// New returns the Linux platform for a tunnel device.
func New(tunName string) *Linux { return &Linux{tun: tunName} }

var _ engine.Platform = (*Linux)(nil)

// ListenPath opens a UDP socket bound to a device and marked for its policy
// rule.
func (l *Linux) ListenPath(spec engine.PathSpec) (engine.PacketConn, error) {
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		err := c.Control(func(fd uintptr) {
			if serr = unix.BindToDevice(int(fd), spec.Device); serr != nil {
				serr = fmt.Errorf("binding to %s: %w", spec.Device, serr)
				return
			}
			if spec.Mark != 0 {
				if serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_MARK, int(spec.Mark)); serr != nil {
					serr = fmt.Errorf("SO_MARK: %w", serr)
					return
				}
			}
			tuneSocket(int(fd))
		})
		if err != nil {
			return err
		}
		return serr
	}}
	pc, err := lc.ListenPacket(context.Background(), "udp4", "0.0.0.0:0")
	if err != nil {
		return nil, err
	}
	return newUDPConn(pc.(*net.UDPConn))
}

// Listen opens the server socket.
func (l *Linux) Listen(port int) (engine.PacketConn, error) {
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		return c.Control(func(fd uintptr) { tuneSocket(int(fd)) })
	}}
	pc, err := lc.ListenPacket(context.Background(), "udp4", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		return nil, err
	}
	return newUDPConn(pc.(*net.UDPConn))
}

// Socket buffers: deep enough that packets wait in the path's own queue, not
// in the socket. Root may exceed the sysctl limits (the FORCE options);
// otherwise the kernel clamps to them.
const (
	sendBuffer = 2 << 20
	recvBuffer = 4 << 20
)

func tuneSocket(fd int) {
	if unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_SNDBUFFORCE, sendBuffer) != nil {
		_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_SNDBUF, sendBuffer)
	}
	if unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUFFORCE, recvBuffer) != nil {
		_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF, recvBuffer)
	}
}

// udpConn never waits to write. One loop feeds every path, so a socket
// whose buffer is full (its link's queue backed up into it) must drop the
// packet, as a full queue would, rather than stall the other paths.
type udpConn struct {
	*net.UDPConn
	raw syscall.RawConn
}

func newUDPConn(c *net.UDPConn) (*udpConn, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		c.Close()
		return nil, err
	}
	return &udpConn{UDPConn: c, raw: raw}, nil
}

func (c *udpConn) WriteToUDPAddrPort(b []byte, to netip.AddrPort) (int, error) {
	a := to.Addr().Unmap()
	if !a.Is4() {
		return 0, fmt.Errorf("%v is not IPv4", to)
	}
	sa := unix.SockaddrInet4{Port: int(to.Port()), Addr: a.As4()}
	var serr error
	err := c.raw.Write(func(fd uintptr) bool {
		serr = unix.Sendto(int(fd), b, unix.MSG_DONTWAIT, &sa)
		return true
	})
	if err == nil {
		err = serr
	}
	if err != nil {
		return 0, err
	}
	return len(b), nil
}

// Resolve looks up an IPv4 address.
func (l *Linux) Resolve(host string) (netip.Addr, error) {
	if a, err := netip.ParseAddr(host); err == nil {
		return a.Unmap(), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip4", host)
	if err != nil {
		return netip.Addr{}, err
	}
	if len(addrs) == 0 {
		return netip.Addr{}, fmt.Errorf("%s has no IPv4 address", host)
	}
	return addrs[0].Unmap(), nil
}

func isDefault(r netlink.Route) bool {
	if r.Dst == nil {
		return true
	}
	ones, _ := r.Dst.Mask.Size()
	return ones == 0
}

// tunnelish are link types that are themselves tunnels: never a WAN path.
var tunnelish = map[string]bool{
	"tuntap": true, "wireguard": true, "ipip": true, "gre": true, "gretap": true,
	"sit": true, "vti": true, "ip6tnl": true, "ip6gre": true, "vxlan": true, "dummy": true,
}

// DiscoverPaths lists devices with an IPv4 default route in any table.
func (l *Linux) DiscoverPaths(exclude []string) ([]string, error) {
	skip := map[string]bool{"lo": true, l.tun: true}
	for _, e := range exclude {
		skip[e] = true
	}
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: unix.RT_TABLE_UNSPEC}, netlink.RT_FILTER_TABLE)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, r := range routes {
		if !isDefault(r) || r.Protocol == routeProtocol || r.Type != unix.RTN_UNICAST {
			continue
		}
		idx := r.LinkIndex
		if idx == 0 && len(r.MultiPath) > 0 {
			idx = r.MultiPath[0].LinkIndex
		}
		link, err := netlink.LinkByIndex(idx)
		if err != nil {
			continue
		}
		a := link.Attrs()
		if skip[a.Name] || seen[a.Name] || tunnelish[link.Type()] || a.Flags&net.FlagUp == 0 {
			continue
		}
		seen[a.Name] = true
		out = append(out, a.Name)
	}
	return out, nil
}

// defaultVia finds a device's IPv4 default route, preferring the main table,
// skipping weaver's own copies.
func defaultVia(link netlink.Link) (*netlink.Route, error) {
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4,
		&netlink.Route{Table: unix.RT_TABLE_UNSPEC, LinkIndex: link.Attrs().Index},
		netlink.RT_FILTER_TABLE|netlink.RT_FILTER_OIF)
	if err != nil {
		return nil, err
	}
	var best *netlink.Route
	for i := range routes {
		r := &routes[i]
		if !isDefault(*r) || r.Protocol == routeProtocol || r.Type != unix.RTN_UNICAST {
			continue
		}
		if best == nil || r.Table == unix.RT_TABLE_MAIN {
			best = r
		}
	}
	if best == nil {
		return nil, fmt.Errorf("no IPv4 default route on %s", link.Attrs().Name)
	}
	return best, nil
}

// PathRouting copies the device's default route into the path's table and
// points the path's fwmark at it, so the path keeps working when the main
// table only routes through some other WAN.
func (l *Linux) PathRouting(spec engine.PathSpec, on bool) error {
	rule := netlink.NewRule()
	rule.Family = netlink.FAMILY_V4
	rule.Mark = spec.Mark
	mask := uint32(0xffffffff)
	rule.Mask = &mask
	rule.Table = spec.Table
	rule.Priority = spec.Rule

	if !on {
		_ = netlink.RuleDel(rule)
		routes, _ := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: spec.Table}, netlink.RT_FILTER_TABLE)
		for i := range routes {
			_ = netlink.RouteDel(&routes[i])
		}
		return nil
	}

	link, err := netlink.LinkByName(spec.Device)
	if err != nil {
		return err
	}
	src, err := defaultVia(link)
	if err != nil {
		return err
	}
	r := &netlink.Route{
		LinkIndex: link.Attrs().Index,
		Dst:       &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)},
		Gw:        src.Gw,
		Table:     spec.Table,
		Protocol:  routeProtocol,
	}
	if src.Gw != nil {
		r.Flags = int(netlink.FLAG_ONLINK)
	} else {
		r.Scope = netlink.SCOPE_LINK
	}
	if err := netlink.RouteReplace(r); err != nil {
		return fmt.Errorf("table %d default via %s: %w", spec.Table, spec.Device, err)
	}

	rules, err := netlink.RuleList(netlink.FAMILY_V4)
	if err != nil {
		return err
	}
	for _, have := range rules {
		if have.Mark == spec.Mark && have.Table == spec.Table && have.Priority == spec.Rule {
			return nil
		}
	}
	if err := netlink.RuleAdd(rule); err != nil && !errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("rule fwmark %#x lookup %d: %w", spec.Mark, spec.Table, err)
	}
	return nil
}

// SetRoutes adds or removes routes to prefixes through the tunnel device.
func (l *Linux) SetRoutes(prefixes []netip.Prefix, on bool) error {
	link, err := netlink.LinkByName(l.tun)
	if err != nil {
		return err
	}
	var errs []error
	for _, p := range prefixes {
		r := &netlink.Route{
			LinkIndex: link.Attrs().Index,
			Dst:       &net.IPNet{IP: p.Addr().AsSlice(), Mask: net.CIDRMask(p.Bits(), p.Addr().BitLen())},
			Scope:     netlink.SCOPE_LINK,
			Protocol:  routeProtocol,
		}
		if on {
			err = netlink.RouteReplace(r)
		} else {
			err = netlink.RouteDel(r)
			if errors.Is(err, unix.ESRCH) {
				err = nil
			}
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p, err))
		}
	}
	return errors.Join(errs...)
}
