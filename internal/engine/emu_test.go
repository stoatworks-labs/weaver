package engine

import (
	"encoding/binary"
	"errors"
	"math/rand"
	"net"
	"net/netip"
	"sync"
	"time"
)

// This file is a small network emulator for engine tests: each client path is
// a real loopback UDP socket wrapped in a link with delay, loss, a rate limit
// and an off switch, in both directions.

type link struct {
	mu      sync.Mutex
	delay   time.Duration
	loss    float64
	rate    float64 // bytes/s, 0 = unlimited
	down    bool
	queue   time.Duration // tail-drop when the queue is longer than this
	busy    [2]time.Time
	dropped [2]int
}

func newLink(delay time.Duration, rate float64) *link {
	return &link{delay: delay, rate: rate, queue: 150 * time.Millisecond}
}

func (l *link) set(f func(l *link)) {
	l.mu.Lock()
	f(l)
	l.mu.Unlock()
}

// admit decides when a packet of n bytes arrives in direction dir, or drops it.
func (l *link) admit(dir, n int, now time.Time) (time.Time, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.down || (l.loss > 0 && rand.Float64() < l.loss) {
		l.dropped[dir]++
		return time.Time{}, false
	}
	start := now
	if l.busy[dir].After(now) {
		start = l.busy[dir]
	}
	if start.Sub(now) > l.queue {
		l.dropped[dir]++
		return time.Time{}, false
	}
	dep := start
	if l.rate > 0 {
		dep = start.Add(time.Duration(float64(n) / l.rate * float64(time.Second)))
	}
	l.busy[dir] = dep
	return dep.Add(l.delay), true
}

type delayed struct {
	at   time.Time
	b    []byte
	addr netip.AddrPort
}

// pipe delivers items in order at their times.
type pipe struct {
	ch   chan delayed
	done chan struct{}
}

func newPipe(deliver func(delayed)) *pipe {
	p := &pipe{ch: make(chan delayed, 65536), done: make(chan struct{})}
	go func() {
		for {
			select {
			case <-p.done:
				return
			case d := <-p.ch:
				if w := time.Until(d.at); w > 0 {
					t := time.NewTimer(w)
					select {
					case <-t.C:
					case <-p.done:
						t.Stop()
						return
					}
				}
				deliver(d)
			}
		}
	}()
	return p
}

type emuConn struct {
	udp    *net.UDPConn
	l      *link
	up, dn *pipe
	in     chan delayed
	closed chan struct{}
	once   sync.Once
}

func newEmuConn(l *link) (*emuConn, error) {
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return nil, err
	}
	c := &emuConn{udp: udp, l: l, in: make(chan delayed, 65536), closed: make(chan struct{})}
	c.up = newPipe(func(d delayed) { _, _ = c.udp.WriteToUDPAddrPort(d.b, d.addr) })
	c.dn = newPipe(func(d delayed) {
		select {
		case c.in <- d:
		default:
		}
	})
	go func() {
		buf := make([]byte, 65536)
		for {
			n, from, err := c.udp.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}
			at, ok := c.l.admit(1, n+28, time.Now())
			if !ok {
				continue
			}
			c.dn.ch <- delayed{at: at, b: append([]byte(nil), buf[:n]...), addr: from}
		}
	}()
	return c, nil
}

func (c *emuConn) WriteToUDPAddrPort(b []byte, addr netip.AddrPort) (int, error) {
	select {
	case <-c.closed:
		return 0, net.ErrClosed
	default:
	}
	at, ok := c.l.admit(0, len(b)+28, time.Now())
	if ok {
		c.up.ch <- delayed{at: at, b: append([]byte(nil), b...), addr: addr}
	}
	return len(b), nil
}

func (c *emuConn) ReadFromUDPAddrPort(b []byte) (int, netip.AddrPort, error) {
	select {
	case d := <-c.in:
		return copy(b, d.b), d.addr, nil
	case <-c.closed:
		return 0, netip.AddrPort{}, net.ErrClosed
	}
}

func (c *emuConn) Close() error {
	c.once.Do(func() {
		close(c.closed)
		close(c.up.done)
		close(c.dn.done)
		_ = c.udp.Close()
	})
	return nil
}

// testPlatform hands the engine emulated sockets.
type testPlatform struct {
	listen *net.UDPConn
	links  map[string]*link

	mu       sync.Mutex
	routesOn bool
	opened   map[string]int
}

func (p *testPlatform) ListenPath(spec PathSpec) (PacketConn, error) {
	l := p.links[spec.Device]
	if l == nil {
		return nil, errors.New("no such test link " + spec.Device)
	}
	p.mu.Lock()
	if p.opened == nil {
		p.opened = map[string]int{}
	}
	p.opened[spec.Device]++
	p.mu.Unlock()
	return newEmuConn(l)
}

func (p *testPlatform) Listen(int) (PacketConn, error) { return p.listen, nil }

func (p *testPlatform) DiscoverPaths([]string) ([]string, error) { return nil, nil }

func (p *testPlatform) PathRouting(PathSpec, bool) error { return nil }

func (p *testPlatform) SetRoutes(_ []netip.Prefix, on bool) error {
	p.mu.Lock()
	p.routesOn = on
	p.mu.Unlock()
	return nil
}

func (p *testPlatform) Resolve(host string) (netip.Addr, error) { return netip.ParseAddr(host) }

// fakeTUN is a TUN backed by channels.
type fakeTUN struct {
	in     chan []byte // packets "from the LAN"
	out    chan []byte // packets the engine wrote
	closed chan struct{}
	once   sync.Once
}

func newFakeTUN() *fakeTUN {
	return &fakeTUN{in: make(chan []byte, 65536), out: make(chan []byte, 65536), closed: make(chan struct{})}
}

func (t *fakeTUN) Read(b []byte) (int, error) {
	select {
	case p := <-t.in:
		return copy(b, p), nil
	case <-t.closed:
		return 0, net.ErrClosed
	}
}

func (t *fakeTUN) Write(b []byte) (int, error) {
	select {
	case t.out <- append([]byte(nil), b...):
	default:
	}
	return len(b), nil
}

func (t *fakeTUN) Close() error {
	t.once.Do(func() { close(t.closed) })
	return nil
}

// udpPacket builds an IPv4 UDP packet whose payload starts with a 32-bit
// sequence number, padded to size bytes.
func udpPacket(src, dst string, sport, dport uint16, seq uint32, size int) []byte {
	if size < 32 {
		size = 32
	}
	b := make([]byte, size)
	b[0] = 0x45
	binary.BigEndian.PutUint16(b[2:4], uint16(size))
	b[8] = 64
	b[9] = 17
	s, d := netip.MustParseAddr(src).As4(), netip.MustParseAddr(dst).As4()
	copy(b[12:16], s[:])
	copy(b[16:20], d[:])
	binary.BigEndian.PutUint16(b[20:22], sport)
	binary.BigEndian.PutUint16(b[22:24], dport)
	binary.BigEndian.PutUint16(b[24:26], uint16(size-20))
	binary.BigEndian.PutUint32(b[28:32], seq)
	return b
}

// tcpPacket builds an IPv4 TCP data segment (ACK set, payload) carrying seq.
func tcpPacket(src, dst string, seq uint32, size int) []byte {
	if size < 44 {
		size = 44
	}
	b := make([]byte, size)
	b[0] = 0x45
	binary.BigEndian.PutUint16(b[2:4], uint16(size))
	b[8] = 64
	b[9] = 6
	s, d := netip.MustParseAddr(src).As4(), netip.MustParseAddr(dst).As4()
	copy(b[12:16], s[:])
	copy(b[16:20], d[:])
	binary.BigEndian.PutUint16(b[20:22], 40000)
	binary.BigEndian.PutUint16(b[22:24], 443)
	b[32] = 5 << 4 // data offset
	b[33] = 0x10   // ACK
	binary.BigEndian.PutUint32(b[40:44], seq)
	return b
}

func payloadSeq(p []byte) uint32 {
	switch p[9] {
	case 17:
		return binary.BigEndian.Uint32(p[28:32])
	case 6:
		return binary.BigEndian.Uint32(p[40:44])
	}
	return 0
}
