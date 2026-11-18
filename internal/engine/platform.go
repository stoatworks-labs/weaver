package engine

import (
	"net/netip"
)

// TUN is the tunnel device: one IP packet per Read and Write.
type TUN interface {
	Read(b []byte) (int, error)
	Write(b []byte) (int, error)
	Close() error
}

// PacketConn is a UDP socket. *net.UDPConn satisfies it.
type PacketConn interface {
	ReadFromUDPAddrPort(b []byte) (int, netip.AddrPort, error)
	WriteToUDPAddrPort(b []byte, addr netip.AddrPort) (int, error)
	Close() error
}

// PathSpec is what the platform needs to open and route one client path.
type PathSpec struct {
	ID     uint8
	Name   string
	Device string
	Mark   uint32 // SO_MARK for the socket and the policy rule
	Table  int    // policy routing table
	Rule   int    // policy rule priority
}

// Platform is everything the engine needs from the host. The Linux one binds
// sockets to devices and drives netlink; tests use one on loopback.
type Platform interface {
	// ListenPath opens a UDP socket whose packets leave through spec.Device.
	ListenPath(spec PathSpec) (PacketConn, error)
	// Listen opens the server's UDP socket.
	Listen(port int) (PacketConn, error)
	// DiscoverPaths lists devices that have an IPv4 default route in any
	// table, minus the tunnel and anything in exclude.
	DiscoverPaths(exclude []string) ([]string, error)
	// PathRouting installs (on) or removes policy routing for a path. It is
	// called again periodically and must be idempotent.
	PathRouting(spec PathSpec, on bool) error
	// SetRoutes adds (on) or removes routes to prefixes via the tunnel.
	SetRoutes(prefixes []netip.Prefix, on bool) error
	// Resolve looks up an IPv4 address for a host name or literal.
	Resolve(host string) (netip.Addr, error)
}
