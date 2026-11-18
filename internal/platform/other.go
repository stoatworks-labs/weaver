//go:build !linux

package platform

import (
	"errors"
	"net/netip"

	"github.com/stoatworks-labs/weaver/internal/engine"
)

// ErrUnsupported is returned off Linux: the data plane needs a Linux TUN,
// netlink and SO_BINDTODEVICE. Key generation and status still work.
var ErrUnsupported = errors.New("weaver's data plane runs on Linux only")

// TUN is unavailable off Linux.
type TUN struct{}

func (*TUN) Read([]byte) (int, error)  { return 0, ErrUnsupported }
func (*TUN) Write([]byte) (int, error) { return 0, ErrUnsupported }
func (*TUN) Close() error              { return nil }

// Name is empty off Linux.
func (*TUN) Name() string { return "" }

// OpenTUN fails off Linux.
func OpenTUN(string, int, netip.Prefix) (*TUN, error) { return nil, ErrUnsupported }

// Linux is a stub off Linux.
type Linux struct{}

// New returns the stub.
func New(string) *Linux { return &Linux{} }

func (*Linux) ListenPath(engine.PathSpec) (engine.PacketConn, error) { return nil, ErrUnsupported }
func (*Linux) Listen(int) (engine.PacketConn, error)                 { return nil, ErrUnsupported }
func (*Linux) DiscoverPaths([]string) ([]string, error)              { return nil, ErrUnsupported }
func (*Linux) PathRouting(engine.PathSpec, bool) error               { return ErrUnsupported }
func (*Linux) SetRoutes([]netip.Prefix, bool) error                  { return ErrUnsupported }
func (*Linux) Resolve(string) (netip.Addr, error)                    { return netip.Addr{}, ErrUnsupported }
