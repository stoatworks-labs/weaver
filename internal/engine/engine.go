// Package engine is weaver's data plane: it owns the TUN device and the UDP
// sockets, runs the handshakes, and moves packets between them through the
// bond layer. The same engine is the client (many paths to one server) and
// the server (one socket, many peers, each with many paths).
//
// Locking: Engine.mu guards the index and peer maps; each Session has its own
// mu. A goroutine may take Engine.mu while holding a Session.mu, never the
// other way round. Nothing does I/O while holding either.
package engine

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/stoatworks-labs/weaver/internal/bond"
	"github.com/stoatworks-labs/weaver/internal/config"
	"github.com/stoatworks-labs/weaver/internal/handshake"
	"github.com/stoatworks-labs/weaver/internal/proto"
)

const (
	tickInterval     = 5 * time.Millisecond
	discoverInterval = 5 * time.Second
	initCacheTTL     = 10 * time.Second
	fallbackAfter    = 5 * time.Second
)

// timing is the part of the config that a reload can change live.
type timing struct {
	mode         bond.Mode
	probe        time.Duration
	idleProbe    time.Duration
	slowProbe    time.Duration // metered and backup paths while idle
	pathTimeout  time.Duration
	reorderFixed time.Duration // 0 = adaptive
	rekey        time.Duration
	realtimeKbps int
}

func timingFrom(c *config.Config) timing {
	mode, _ := bond.ParseMode(c.Mode)
	return timing{
		mode:         mode,
		probe:        time.Duration(c.ProbeIntervalMs) * time.Millisecond,
		idleProbe:    time.Duration(c.IdleProbeIntervalMs) * time.Millisecond,
		slowProbe:    5 * time.Second,
		pathTimeout:  time.Duration(c.PathTimeoutMs) * time.Millisecond,
		reorderFixed: time.Duration(c.ReorderTimeoutMs) * time.Millisecond,
		rekey:        time.Duration(c.RekeyIntervalS) * time.Second,
		realtimeKbps: c.RealtimeKbps,
	}
}

type cachedResp struct {
	resp    []byte
	session *Session
	expires time.Time
}

type prefixRoute struct {
	prefix  netip.Prefix
	session *Session
}

// Engine is one running weaver.
type Engine struct {
	log     *slog.Logger
	plat    Platform
	tun     TUN
	version string

	priv, pub handshake.Key
	instance  uint32
	start     time.Time
	server    bool
	bufSize   int
	pool      sync.Pool

	sendErrors atomic.Uint64

	mu        sync.RWMutex
	cfg       *config.Config
	tm        timing
	byIndex   map[uint32]*keypair
	sessions  []*Session
	peerByKey map[handshake.Key]*Session
	routes    []prefixRoute
	initCache map[[32]byte]*cachedResp

	// server
	listen PacketConn

	// client
	client      *Session
	serverAddr  netip.AddrPort
	pathIDs     map[string]uint8
	routesOn    bool
	downSince   time.Time
	tunnelPfx   []netip.Prefix
	pathsMu     sync.Mutex // serialises syncPaths
	lastResolve time.Time

	wg sync.WaitGroup
}

// New builds an engine. It does no I/O until Run.
func New(cfg *config.Config, tun TUN, plat Platform, log *slog.Logger, version string) (*Engine, error) {
	priv, err := handshake.ParseKey(cfg.PrivateKey)
	if err != nil {
		return nil, err
	}
	var inst [4]byte
	if _, err := rand.Read(inst[:]); err != nil {
		return nil, err
	}
	e := &Engine{
		log:       log,
		plat:      plat,
		tun:       tun,
		version:   version,
		priv:      priv,
		pub:       handshake.PublicKey(priv),
		instance:  binary.BigEndian.Uint32(inst[:]) | 1,
		start:     time.Now(),
		server:    cfg.Role == config.RoleServer,
		bufSize:   cfg.Tun.MTU + 128,
		cfg:       cfg,
		tm:        timingFrom(cfg),
		byIndex:   map[uint32]*keypair{},
		peerByKey: map[handshake.Key]*Session{},
		initCache: map[[32]byte]*cachedResp{},
		pathIDs:   map[string]uint8{},
	}
	e.pool.New = func() any { return make([]byte, e.bufSize) }

	if e.server {
		for _, p := range cfg.Peers {
			key, _ := handshake.ParseKey(p.PublicKey)
			s := newSession(e, p.Name, key)
			addr, _ := netip.ParseAddr(p.Address)
			s.allowed = append(s.allowed, netip.PrefixFrom(addr, addr.BitLen()))
			for _, a := range p.AllowedIPs {
				pfx, _ := netip.ParsePrefix(a)
				s.allowed = append(s.allowed, pfx.Masked())
			}
			for _, pfx := range s.allowed {
				e.routes = append(e.routes, prefixRoute{prefix: pfx, session: s})
			}
			e.sessions = append(e.sessions, s)
			e.peerByKey[key] = s
		}
		// Longest prefix first, so the first match wins.
		sort.SliceStable(e.routes, func(i, j int) bool { return e.routes[i].prefix.Bits() > e.routes[j].prefix.Bits() })
	} else {
		key, _ := handshake.ParseKey(cfg.ServerPublicKey)
		e.client = newSession(e, "server", key)
		e.sessions = []*Session{e.client}
		if config.On(cfg.Routing.DefaultRoute) {
			e.tunnelPfx = []netip.Prefix{netip.MustParsePrefix("0.0.0.0/1"), netip.MustParsePrefix("128.0.0.0/1")}
		}
		for _, s := range cfg.Routing.Subnets {
			pfx, _ := netip.ParsePrefix(s)
			e.tunnelPfx = append(e.tunnelPfx, pfx.Masked())
		}
	}
	return e, nil
}

// PublicKey is this end's public key.
func (e *Engine) PublicKey() handshake.Key { return e.pub }

func (e *Engine) config() (*config.Config, timing) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.cfg, e.tm
}

func (e *Engine) timing() timing {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.tm
}

// mono is the engine's monotonic clock in nanoseconds, used in probes and
// reports.
func (e *Engine) mono(now time.Time) uint64 { return uint64(now.Sub(e.start)) }

// write sends one datagram. A failure (a full socket buffer, a WAN that just
// went away) loses the packet, which the path's loss figures then show; it is
// counted, not retried.
func (e *Engine) write(c PacketConn, b []byte, to netip.AddrPort) {
	if _, err := c.WriteToUDPAddrPort(b, to); err != nil {
		e.sendErrors.Add(1)
	}
}

// micros is the clock stamped on DATA frames: microseconds, wrapping.
func (e *Engine) micros(now time.Time) uint32 { return uint32(now.Sub(e.start).Microseconds()) }

func (e *Engine) getBuf(n int) []byte {
	b := e.pool.Get().([]byte)
	if n > cap(b) {
		return make([]byte, n)
	}
	return b[:n]
}

func (e *Engine) putBuf(b []byte) {
	if cap(b) == e.bufSize {
		e.pool.Put(b[:cap(b)]) //nolint:staticcheck // slices are fine here
	}
}

// newIndex picks an unused random receiver index. Caller holds e.mu.
func (e *Engine) newIndexLocked() uint32 {
	var b [4]byte
	for {
		_, _ = rand.Read(b[:])
		i := binary.BigEndian.Uint32(b[:])
		if _, used := e.byIndex[i]; !used && i != 0 {
			return i
		}
	}
}

func (e *Engine) addIndex(kp *keypair) {
	e.mu.Lock()
	kp.local = e.newIndexLocked()
	e.byIndex[kp.local] = kp
	e.mu.Unlock()
}

func (e *Engine) reserveIndex() uint32 {
	e.mu.Lock()
	defer e.mu.Unlock()
	i := e.newIndexLocked()
	e.byIndex[i] = nil // reserved for a handshake in flight
	return i
}

func (e *Engine) setIndex(i uint32, kp *keypair) {
	e.mu.Lock()
	e.byIndex[i] = kp
	e.mu.Unlock()
}

func (e *Engine) dropIndex(i uint32) {
	e.mu.Lock()
	delete(e.byIndex, i)
	e.mu.Unlock()
}

// Run starts the engine and blocks until ctx ends.
func (e *Engine) Run(ctx context.Context) error {
	cfg, _ := e.config()
	if e.server {
		conn, err := e.plat.Listen(cfg.ListenPort)
		if err != nil {
			return fmt.Errorf("listen on udp/%d: %w", cfg.ListenPort, err)
		}
		e.listen = conn
		var pfx []netip.Prefix
		tunPfx, _ := netip.ParsePrefix(cfg.Tun.Address)
		for _, r := range e.routes {
			if tunPfx.Masked().Contains(r.prefix.Addr()) && r.prefix.Bits() >= tunPfx.Bits() {
				continue // on-link through the tunnel address already
			}
			pfx = append(pfx, r.prefix)
		}
		if len(pfx) > 0 {
			if err := e.plat.SetRoutes(pfx, true); err != nil {
				e.log.Warn("adding peer routes", "err", err)
			}
			defer func() { _ = e.plat.SetRoutes(pfx, false) }()
		}
		e.log.Info("server listening", "port", cfg.ListenPort, "public_key", e.pub.String(), "peers", len(e.sessions))
		e.wg.Add(1)
		go func() { defer e.wg.Done(); e.readLoop(conn) }()
	} else {
		if err := e.resolveServer(); err != nil {
			e.log.Warn("resolving server; will retry", "server", cfg.Server, "err", err)
		}
		e.syncPaths()
		if !config.On(cfg.Routing.FallbackDirect) && len(e.tunnelPfx) > 0 {
			if err := e.plat.SetRoutes(e.tunnelPfx, true); err != nil {
				e.log.Warn("adding tunnel routes", "err", err)
			} else {
				e.routesOn = true
			}
		}
		e.log.Info("client starting", "server", cfg.Server, "public_key", e.pub.String())
	}

	e.wg.Add(1)
	go func() { defer e.wg.Done(); e.tunLoop(ctx) }()

	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()
	scratch := make([]byte, 0, e.bufSize+64)
	lastDiscover := time.Now()
	for {
		select {
		case <-ctx.Done():
			e.shutdown()
			return nil
		case now := <-ticker.C:
			e.mu.RLock()
			sessions := append([]*Session(nil), e.sessions...)
			e.mu.RUnlock()
			for _, s := range sessions {
				s.tick(now, scratch)
			}
			if !e.server {
				e.clientHousekeeping(now)
				if now.Sub(lastDiscover) >= discoverInterval {
					lastDiscover = now
					go e.syncPaths()
				}
			} else {
				e.pruneInitCache(now)
			}
		}
	}
}

func (e *Engine) shutdown() {
	if e.client != nil {
		cfg, _ := e.config()
		if e.routesOn {
			_ = e.plat.SetRoutes(e.tunnelPfx, false)
		}
		e.client.mu.Lock()
		paths := e.client.pathList()
		e.client.mu.Unlock()
		for _, p := range paths {
			if p.conn != nil {
				_ = p.conn.Close()
			}
			if config.On(cfg.Routing.PolicyRouting) {
				_ = e.plat.PathRouting(e.spec(p.ID, p.Name, p.Device), false)
			}
		}
	}
	if e.listen != nil {
		_ = e.listen.Close()
	}
	_ = e.tun.Close()
	e.wg.Wait()
}

// Reload applies a new config that config.NeedsRestart says can be applied live.
func (e *Engine) Reload(cfg *config.Config) {
	e.mu.Lock()
	e.cfg = cfg
	e.tm = timingFrom(cfg)
	sessions := append([]*Session(nil), e.sessions...)
	e.mu.Unlock()
	for _, s := range sessions {
		s.mu.Lock()
		s.tm = timingFrom(cfg)
		s.sched.Mode = s.tm.mode
		s.classifier.RealtimeBps = float64(cfg.RealtimeKbps) * 1000 / 8
		s.mu.Unlock()
	}
	if !e.server {
		e.syncPaths()
	}
	e.log.Info("reloaded", "mode", cfg.Mode)
}

func (e *Engine) resolveServer() error {
	cfg, _ := e.config()
	host, portStr, err := net.SplitHostPort(cfg.Server)
	if err != nil {
		return err
	}
	port, _ := strconv.Atoi(portStr)
	addr, err := e.plat.Resolve(host)
	if err != nil {
		return err
	}
	ap := netip.AddrPortFrom(addr, uint16(port))
	e.mu.Lock()
	changed := e.serverAddr != ap
	e.serverAddr = ap
	e.lastResolve = time.Now()
	e.mu.Unlock()
	if changed {
		e.log.Info("server address", "addr", ap)
		if s := e.client; s != nil {
			s.mu.Lock()
			for _, p := range s.paths {
				p.remote = ap
			}
			s.mu.Unlock()
		}
	}
	return nil
}

func (e *Engine) server4() netip.AddrPort {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.serverAddr
}

func (e *Engine) spec(id uint8, name, device string) PathSpec {
	cfg, _ := e.config()
	return PathSpec{
		ID:     id,
		Name:   name,
		Device: device,
		Mark:   cfg.Routing.FwmarkBase + uint32(id),
		Table:  cfg.Routing.TableBase + int(id),
		Rule:   cfg.Routing.RulePriority,
	}
}

type wantPath struct {
	name, device string
	prio         bond.Priority
	up, down     int
	metered      bool
}

// syncPaths makes the client's open paths match the config plus whatever
// auto discovery finds, opening and closing sockets as needed.
func (e *Engine) syncPaths() {
	e.pathsMu.Lock()
	defer e.pathsMu.Unlock()
	cfg, _ := e.config()
	s := e.client

	var want []wantPath
	explicitDev := map[string]bool{}
	for _, p := range cfg.Paths {
		prio, _ := bond.ParsePriority(p.Priority)
		explicitDev[p.Device] = true
		if prio == bond.Disabled {
			continue
		}
		want = append(want, wantPath{p.Name, p.Device, prio, p.UpKbps, p.DownKbps, p.Metered})
	}
	if config.On(cfg.AutoPaths) {
		exclude := append([]string{cfg.Tun.Name}, cfg.ExcludeDevices...)
		devs, err := e.plat.DiscoverPaths(exclude)
		if err != nil {
			e.log.Warn("discovering paths", "err", err)
		}
		for _, d := range devs {
			if explicitDev[d] {
				continue
			}
			want = append(want, wantPath{name: d, device: d, prio: bond.Primary})
		}
	}
	if !e.server4().IsValid() && time.Since(e.lastResolve) > 10*time.Second {
		if err := e.resolveServer(); err != nil {
			e.log.Debug("resolving server", "err", err)
		}
	}
	remote := e.server4()

	s.mu.Lock()
	existing := map[string]*Path{}
	for _, p := range s.paths {
		existing[p.Name] = p
	}
	s.mu.Unlock()

	wanted := map[string]bool{}
	for _, w := range want {
		wanted[w.name] = true
		p := existing[w.name]
		if p != nil && p.Device == w.device && !p.needRebind {
			s.mu.Lock()
			changed := p.Priority != w.prio || p.UpKbps != w.up || p.DownKbps != w.down || p.Metered != w.metered
			p.Priority, p.UpKbps, p.DownKbps, p.Metered = w.prio, w.up, w.down, w.metered
			p.lane.Priority = w.prio
			p.lane.Cap = kbpsToBytes(w.up)
			if changed {
				s.pathInfoDirty = true
			}
			s.mu.Unlock()
			if config.On(cfg.Routing.PolicyRouting) {
				if err := e.plat.PathRouting(e.spec(p.ID, p.Name, p.Device), true); err != nil {
					e.log.Debug("refreshing policy routing", "path", p.Name, "err", err)
				}
			}
			continue
		}
		if p != nil {
			e.closePath(p)
		}
		id, ok := e.pathID(w.name)
		if !ok {
			e.log.Warn("too many paths", "path", w.name)
			continue
		}
		spec := e.spec(id, w.name, w.device)
		if config.On(cfg.Routing.PolicyRouting) {
			if err := e.plat.PathRouting(spec, true); err != nil {
				e.log.Warn("policy routing", "path", w.name, "device", w.device, "err", err)
			}
		}
		conn, err := e.plat.ListenPath(spec)
		if err != nil {
			e.log.Warn("opening path", "path", w.name, "device", w.device, "err", err)
			continue
		}
		np := newPath(id, w.name, w.device, w.prio)
		np.UpKbps, np.DownKbps, np.Metered = w.up, w.down, w.metered
		np.lane.Cap = kbpsToBytes(w.up)
		np.conn, np.remote = conn, remote
		np.lastRebind = time.Now()
		s.mu.Lock()
		s.addPath(np)
		s.mu.Unlock()
		e.log.Info("path added", "path", w.name, "device", w.device, "id", id, "priority", w.prio)
		e.wg.Add(1)
		go func() { defer e.wg.Done(); e.readLoop(conn) }()
	}
	for name, p := range existing {
		if !wanted[name] {
			e.closePath(p)
			e.log.Info("path removed", "path", name, "device", p.Device)
		}
	}
}

func (e *Engine) closePath(p *Path) {
	cfg, _ := e.config()
	s := e.client
	s.mu.Lock()
	s.removePath(p.ID)
	s.mu.Unlock()
	if p.conn != nil {
		_ = p.conn.Close()
	}
	if config.On(cfg.Routing.PolicyRouting) {
		_ = e.plat.PathRouting(e.spec(p.ID, p.Name, p.Device), false)
	}
}

// pathID gives a stable ID per path name for the life of the process.
func (e *Engine) pathID(name string) (uint8, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if id, ok := e.pathIDs[name]; ok {
		return id, true
	}
	used := map[uint8]bool{}
	for _, id := range e.pathIDs {
		used[id] = true
	}
	for id := 0; id < 255; id++ {
		if !used[uint8(id)] {
			e.pathIDs[name] = uint8(id)
			return uint8(id), true
		}
	}
	return 0, false
}

// clientHousekeeping turns the tunnel routes on and off with the session
// (fallback_direct) and rebuilds sockets on paths that have been down a while.
func (e *Engine) clientHousekeeping(now time.Time) {
	cfg, _ := e.config()
	s := e.client
	s.mu.Lock()
	up := s.state == stateUp
	var rebind []*Path
	for _, p := range s.paths {
		if p.needRebind && !p.rebinding {
			p.rebinding = true
			rebind = append(rebind, p)
		}
	}
	s.mu.Unlock()

	for _, p := range rebind {
		go e.rebind(p)
	}

	if len(e.tunnelPfx) == 0 || !config.On(cfg.Routing.FallbackDirect) {
		return
	}
	want := e.routesOn
	if up {
		e.downSince = time.Time{}
		want = true
	} else {
		if e.downSince.IsZero() {
			e.downSince = now
		}
		if now.Sub(e.downSince) >= fallbackAfter {
			want = false
		}
	}
	if want != e.routesOn {
		if err := e.plat.SetRoutes(e.tunnelPfx, want); err != nil {
			e.log.Warn("tunnel routes", "on", want, "err", err)
			return
		}
		e.routesOn = want
		if want {
			e.log.Info("tunnel up; routing through it")
		} else {
			e.log.Warn("tunnel down; falling back to direct internet")
		}
	}
}

func (e *Engine) rebind(p *Path) {
	spec := e.spec(p.ID, p.Name, p.Device)
	conn, err := e.plat.ListenPath(spec)
	s := e.client
	s.mu.Lock()
	defer s.mu.Unlock()
	p.rebinding = false
	p.lastRebind = time.Now()
	p.needRebind = false
	if err != nil {
		e.log.Debug("rebinding path", "path", p.Name, "err", err)
		return
	}
	if s.paths[p.ID] != p {
		_ = conn.Close()
		return
	}
	old := p.conn
	p.conn = conn
	if old != nil {
		_ = old.Close()
	}
	e.log.Debug("rebound path socket", "path", p.Name)
	e.wg.Add(1)
	go func() { defer e.wg.Done(); e.readLoop(conn) }()
}

func (e *Engine) pruneInitCache(now time.Time) {
	e.mu.Lock()
	for k, c := range e.initCache {
		if now.After(c.expires) {
			delete(e.initCache, k)
		}
	}
	e.mu.Unlock()
}

// route finds the session an IP packet from the TUN belongs to.
func (e *Engine) route(pkt []byte) *Session {
	if !e.server {
		return e.client
	}
	dst, ok := dstAddr(pkt)
	if !ok {
		return nil
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	for _, r := range e.routes {
		if r.prefix.Contains(dst) {
			return r.session
		}
	}
	return nil
}

func dstAddr(pkt []byte) (netip.Addr, bool) {
	if len(pkt) < 20 {
		return netip.Addr{}, false
	}
	switch pkt[0] >> 4 {
	case 4:
		return netip.AddrFrom4([4]byte(pkt[16:20])), true
	case 6:
		if len(pkt) < 40 {
			return netip.Addr{}, false
		}
		return netip.AddrFrom16([16]byte(pkt[24:40])), true
	}
	return netip.Addr{}, false
}

func srcAddr(pkt []byte) (netip.Addr, bool) {
	if len(pkt) < 20 {
		return netip.Addr{}, false
	}
	switch pkt[0] >> 4 {
	case 4:
		return netip.AddrFrom4([4]byte(pkt[12:16])), true
	case 6:
		if len(pkt) < 40 {
			return netip.Addr{}, false
		}
		return netip.AddrFrom16([16]byte(pkt[8:24])), true
	}
	return netip.Addr{}, false
}

// tunLoop reads packets from the TUN and sends them into their session.
func (e *Engine) tunLoop(ctx context.Context) {
	buf := make([]byte, e.bufSize)
	scratch := make([]byte, 0, e.bufSize+64)
	var targets []outPkt
	head := proto.DataFrameHeaderLen
	for {
		n, err := e.tun.Read(buf[head:])
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			e.log.Warn("reading tun", "err", err)
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if n == 0 {
			continue
		}
		pkt := buf[head : head+n]
		s := e.route(pkt)
		if s == nil {
			continue
		}
		targets = s.prepareData(buf[:head+n], pkt, time.Now(), targets[:0])
		for i := range targets {
			scratch = e.seal(scratch, &targets[i])
			e.write(targets[i].conn, scratch, targets[i].to)
		}
	}
}

// readLoop reads one UDP socket until it is closed.
func (e *Engine) readLoop(conn PacketConn) {
	buf := make([]byte, e.bufSize+64)
	plain := make([]byte, 0, e.bufSize+64)
	scratch := make([]byte, 0, e.bufSize+64)
	for {
		n, from, err := conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			// ICMP-driven errors (port unreachable) are transient.
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if n < 1 {
			continue
		}
		e.handlePacket(conn, buf[:n], from.Addr().Unmap(), from, plain, scratch)
	}
}

func (e *Engine) handlePacket(conn PacketConn, b []byte, _ netip.Addr, from netip.AddrPort, plain, scratch []byte) {
	from = netip.AddrPortFrom(from.Addr().Unmap(), from.Port())
	switch b[0] {
	case proto.TypeData:
		h, err := proto.ParseDataHeader(b)
		if err != nil {
			return
		}
		e.mu.RLock()
		kp := e.byIndex[h.Receiver]
		e.mu.RUnlock()
		if kp == nil {
			return
		}
		pt, err := kp.recv.Decrypt(plain[:0], h.Nonce, b[:proto.DataHeaderLen], b[proto.DataHeaderLen:])
		if err != nil || len(pt) == 0 {
			return
		}
		_, ctr := proto.SplitNonce(h.Nonce)
		if !kp.accept(h.PathID, ctr) {
			return
		}
		kp.session.onData(kp, h.PathID, ctr, len(b), from, pt, scratch)
	case proto.TypeHandshakeInit:
		if e.server {
			e.onInit(b, from)
		}
	case proto.TypeHandshakeResp:
		if !e.server {
			e.client.onResp(b, from, scratch)
		}
	}
}

// onInit answers a client's handshake. The same init sent over several paths
// gets the same answer from a cache, so the client and server agree on one
// keypair; each copy still teaches the server that path's address.
func (e *Engine) onInit(b []byte, from netip.AddrPort) {
	pid, sender, msg, err := proto.ParseInitHeader(b)
	if err != nil {
		return
	}
	h := sha256.Sum256(msg)
	now := time.Now()

	e.mu.RLock()
	c := e.initCache[h]
	e.mu.RUnlock()
	if c != nil && now.Before(c.expires) {
		// The same init on another path (the client sends it on several):
		// the same answer, on this one. Paths learn their address only from
		// authenticated data, never from an init, which anyone can replay.
		resp := append([]byte(nil), c.resp...)
		resp[1] = pid
		e.write(e.listen, resp, from)
		return
	}

	var s *Session
	res, err := handshake.Respond(e.priv, msg,
		func(k handshake.Key) bool {
			e.mu.RLock()
			s = e.peerByKey[k]
			e.mu.RUnlock()
			return s != nil
		},
		func(handshake.Key, []byte) []byte {
			return proto.AppendHello(nil, proto.Hello{Version: proto.Version, Instance: e.instance})
		})
	if err != nil {
		e.log.Debug("handshake rejected", "from", from, "err", err)
		return
	}
	hello, err := proto.ParseHello(res.Payload)
	if err != nil || hello.Version != proto.Version {
		e.log.Debug("handshake: bad hello", "from", from)
		return
	}

	s.mu.Lock()
	kp := &keypair{session: s, remote: sender, send: res.Send, recv: res.Recv, created: now, peerInstance: hello.Instance}
	e.addIndex(kp)
	if s.pending != nil {
		e.dropIndex(s.pending.local)
	}
	s.pending = kp
	resp := make([]byte, proto.RespHeaderLen, proto.RespHeaderLen+len(res.Message))
	proto.PutRespHeader(resp, pid, kp.local, sender)
	resp = append(resp, res.Message...)
	s.mu.Unlock()

	e.mu.Lock()
	e.initCache[h] = &cachedResp{resp: resp, session: s, expires: now.Add(initCacheTTL)}
	e.mu.Unlock()
	e.log.Debug("handshake answered", "peer", s.name, "from", from, "path", pid)
	e.write(e.listen, resp, from)
}

// seal encrypts one outgoing frame into dst and returns the datagram.
func (e *Engine) seal(dst []byte, o *outPkt) []byte {
	dst = dst[:proto.DataHeaderLen]
	proto.PutDataHeader(dst, o.pathID, o.kp.remote, o.nonce)
	return o.kp.send.Encrypt(dst, o.nonce, dst[:proto.DataHeaderLen], o.frame)
}

// flush sends queued datagrams and writes delivered packets to the TUN.
func (e *Engine) flush(outs []outPkt, raws []rawPkt, delivered [][]byte, scratch []byte) {
	for i := range outs {
		if outs[i].conn == nil || !outs[i].to.IsValid() {
			continue
		}
		scratch = e.seal(scratch, &outs[i])
		e.write(outs[i].conn, scratch, outs[i].to)
	}
	for _, r := range raws {
		if r.conn == nil || !r.to.IsValid() {
			continue
		}
		e.write(r.conn, r.b, r.to)
	}
	for _, p := range delivered {
		_, _ = e.tun.Write(p)
		e.putBuf(p)
	}
}

func kbpsToBytes(k int) float64 { return float64(k) * 1000 / 8 }
