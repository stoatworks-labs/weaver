package engine

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"testing"
	"time"

	"github.com/stoatworks-labs/weaver/internal/config"
	"github.com/stoatworks-labs/weaver/internal/handshake"
	"github.com/stoatworks-labs/weaver/internal/proto"
)

type harness struct {
	t          *testing.T
	server     *Engine
	client     *Engine
	serverTun  *fakeTUN
	clientTun  *fakeTUN
	links      map[string]*link
	clientPlat *testPlatform
	cancel     context.CancelFunc
	done       chan struct{}
}

func testLogger() *slog.Logger {
	if os.Getenv("WEAVER_TEST_LOG") != "" {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug - 4}))
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func key(t *testing.T) handshake.Key {
	k, err := handshake.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// start brings up a server and a client joined by the named links.
func start(t *testing.T, mode string, links map[string]*link, tweak func(c *config.Config)) *harness {
	t.Helper()
	sk, ck := key(t), key(t)
	off := false
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	scfg := &config.Config{
		Role:       config.RoleServer,
		PrivateKey: sk.String(),
		Mode:       mode,
		Peers:      []config.Peer{{Name: "flint", PublicKey: handshake.PublicKey(ck).String(), Address: "10.77.0.2"}},
	}
	if err := scfg.Finish(); err != nil {
		t.Fatal(err)
	}
	ccfg := &config.Config{
		Role:            config.RoleClient,
		PrivateKey:      ck.String(),
		Server:          udp.LocalAddr().String(),
		ServerPublicKey: handshake.PublicKey(sk).String(),
		Mode:            mode,
		AutoPaths:       &off,
		Tun:             config.Tun{Address: "10.77.0.2/24"},
		Routing:         config.Routing{PolicyRouting: &off},
	}
	for name := range links {
		ccfg.Paths = append(ccfg.Paths, config.Path{Name: name, Device: name})
	}
	if tweak != nil {
		tweak(ccfg)
	}
	if err := ccfg.Finish(); err != nil {
		t.Fatal(err)
	}

	h := &harness{t: t, links: links, serverTun: newFakeTUN(), clientTun: newFakeTUN(), done: make(chan struct{})}
	h.server, err = New(scfg, h.serverTun, &testPlatform{listen: udp}, testLogger().With("side", "server"), "test")
	if err != nil {
		t.Fatal(err)
	}
	h.clientPlat = &testPlatform{links: links}
	h.client, err = New(ccfg, h.clientTun, h.clientPlat, testLogger().With("side", "client"), "test")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() { _ = h.server.Run(ctx) }()
	go func() { _ = h.client.Run(ctx); close(h.done) }()
	t.Cleanup(h.stop)
	return h
}

func (h *harness) stop() {
	h.cancel()
	select {
	case <-h.done:
	case <-time.After(5 * time.Second):
		h.t.Error("client did not stop")
	}
}

// waitPaths waits until the client sees every named path up.
func (h *harness) waitPaths(timeout time.Duration, names ...string) {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		st := h.client.Status()
		up := 0
		if len(st.Sessions) == 1 && st.Sessions[0].State == "up" {
			for _, p := range st.Sessions[0].Paths {
				for _, n := range names {
					if p.Name == n && p.State == "up" {
						up++
					}
				}
			}
		}
		if up == len(names) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatalf("paths %v not up after %v: %+v", names, timeout, h.client.Status().Sessions)
}

func (h *harness) clientPath(name string) PathStatus {
	for _, p := range h.client.Status().Sessions[0].Paths {
		if p.Name == name {
			return p
		}
	}
	h.t.Fatalf("no path %s", name)
	return PathStatus{}
}

// collect reads packets from a TUN until quiet for idle.
func collect(tun *fakeTUN, idle time.Duration) [][]byte {
	var out [][]byte
	for {
		select {
		case p := <-tun.out:
			out = append(out, p)
		case <-time.After(idle):
			return out
		}
	}
}

func TestTrafficBothWays(t *testing.T) {
	h := start(t, "adaptive", map[string]*link{
		"a": newLink(5*time.Millisecond, 0),
		"b": newLink(20*time.Millisecond, 0),
	}, nil)
	h.waitPaths(3*time.Second, "a", "b")

	const n = 200
	for i := 0; i < n; i++ {
		h.clientTun.in <- tcpPacket("10.77.0.2", "1.1.1.1", uint32(i), 600)
	}
	got := collect(h.serverTun, 300*time.Millisecond)
	if len(got) != n {
		t.Fatalf("server got %d packets, want %d", len(got), n)
	}
	for i, p := range got {
		if s := payloadSeq(p); s != uint32(i) {
			t.Fatalf("packet %d carries seq %d: out of order", i, s)
		}
	}

	// And back: the server routes to the peer by its tunnel address.
	for i := 0; i < n; i++ {
		h.serverTun.in <- tcpPacket("1.1.1.1", "10.77.0.2", uint32(i), 600)
	}
	back := collect(h.clientTun, 300*time.Millisecond)
	if len(back) != n {
		t.Fatalf("client got %d packets, want %d", len(back), n)
	}
	for i, p := range back {
		if s := payloadSeq(p); s != uint32(i) {
			t.Fatalf("return packet %d carries seq %d", i, s)
		}
	}
	if !h.clientPlat.routesOn {
		t.Error("tunnel routes not turned on once connected")
	}
}

func TestServerDropsSpoofedSource(t *testing.T) {
	h := start(t, "bond", map[string]*link{"a": newLink(time.Millisecond, 0)}, nil)
	h.waitPaths(3*time.Second, "a")
	h.clientTun.in <- udpPacket("10.9.9.9", "1.1.1.1", 4000, 4000, 1, 100)
	h.clientTun.in <- udpPacket("10.77.0.2", "1.1.1.1", 4000, 4000, 2, 100)
	got := collect(h.serverTun, 200*time.Millisecond)
	if len(got) != 1 || payloadSeq(got[0]) != 2 {
		t.Fatalf("server delivered %d packets; want only the one from the peer's address", len(got))
	}
}

func TestBondingUsesBothPathsByCapacity(t *testing.T) {
	// a: 800 kB/s, b: 300 kB/s. Offer ~1.4 MB/s of bulk for 3 s.
	h := start(t, "bond", map[string]*link{
		"a": newLink(10*time.Millisecond, 800e3),
		"b": newLink(25*time.Millisecond, 300e3),
	}, nil)
	h.waitPaths(3*time.Second, "a", "b")

	stop := time.Now().Add(3 * time.Second)
	seq := uint32(0)
	for time.Now().Before(stop) {
		for i := 0; i < 14; i++ { // 14 × 1000 B per ms ≈ 1.4 MB/s
			h.clientTun.in <- tcpPacket("10.77.0.2", "1.1.1.1", seq, 1000)
			seq++
		}
		time.Sleep(10 * time.Millisecond)
	}
	got := collect(h.serverTun, 500*time.Millisecond)
	a, b := h.clientPath("a"), h.clientPath("b")
	t.Logf("offered %d, delivered %d; a: tx %d B, rate %.0f kbit/s; b: tx %d B, rate %.0f kbit/s",
		seq, len(got), a.TxBytes, a.RateKbps, b.TxBytes, b.RateKbps)
	if b.TxBytes == 0 || a.TxBytes == 0 {
		t.Fatal("bonding left a path unused")
	}
	if a.TxBytes < b.TxBytes {
		t.Errorf("the faster path carried less (%d < %d)", a.TxBytes, b.TxBytes)
	}
	// Both links together can carry ~1.1 MB/s, one alone at most 0.8.
	delivered := float64(len(got)) * 1000 / 3
	if delivered < 0.85e6 {
		t.Errorf("delivered %.0f B/s; bonding should beat the faster link alone (800 kB/s)", delivered)
	}
}

func TestRealtimeSurvivesLinkLoss(t *testing.T) {
	h := start(t, "adaptive", map[string]*link{
		"a": newLink(5*time.Millisecond, 0),
		"b": newLink(30*time.Millisecond, 0),
	}, nil)
	h.waitPaths(3*time.Second, "a", "b")

	// A SIP-port UDP stream every 5 ms for 3 s; path a dies after 1 s.
	go func() {
		time.Sleep(time.Second)
		h.links["a"].set(func(l *link) { l.down = true })
	}()
	const n = 600
	for i := 0; i < n; i++ {
		h.clientTun.in <- udpPacket("10.77.0.2", "1.1.1.1", 5060, 5060, uint32(i), 160)
		time.Sleep(5 * time.Millisecond)
	}
	got := collect(h.serverTun, 300*time.Millisecond)
	seen := map[uint32]bool{}
	for _, p := range got {
		seen[payloadSeq(p)] = true
	}
	missing := 0
	for i := 0; i < n; i++ {
		if !seen[uint32(i)] {
			missing++
		}
	}
	t.Logf("delivered %d of %d (%d distinct), missing %d", len(got), n, len(seen), missing)
	if len(got) != len(seen) {
		t.Errorf("%d duplicates reached the TUN", len(got)-len(seen))
	}
	if missing > 0 {
		t.Errorf("a duplicated real-time stream lost %d packets when one link died", missing)
	}
	if st := h.clientPath("a").State; st == "up" {
		t.Errorf("path a still %s 2 s after its link died", st)
	}
}

func TestBulkFailoverIsQuick(t *testing.T) {
	h := start(t, "bond", map[string]*link{
		"a": newLink(5*time.Millisecond, 0),
		"b": newLink(20*time.Millisecond, 0),
	}, func(c *config.Config) { c.ProbeIntervalMs = 100 })
	h.waitPaths(3*time.Second, "a", "b")

	go func() {
		time.Sleep(time.Second)
		h.links["a"].set(func(l *link) { l.down = true })
	}()
	const n = 600
	for i := 0; i < n; i++ {
		h.clientTun.in <- tcpPacket("10.77.0.2", "1.1.1.1", uint32(i), 400)
		time.Sleep(5 * time.Millisecond)
	}
	got := collect(h.serverTun, 300*time.Millisecond)
	missing := n - len(got)
	t.Logf("bulk: delivered %d of %d, %d lost across the failover", len(got), n, missing)
	// At 5 ms spacing, losing more than ~80 packets would mean the dead path
	// kept being used for well over 400 ms.
	if missing > 80 {
		t.Errorf("lost %d packets: failover too slow", missing)
	}
}

func TestPathComesBack(t *testing.T) {
	h := start(t, "bond", map[string]*link{
		"a": newLink(5*time.Millisecond, 0),
		"b": newLink(10*time.Millisecond, 0),
	}, nil)
	h.waitPaths(3*time.Second, "a", "b")
	h.links["a"].set(func(l *link) { l.down = true })
	deadline := time.Now().Add(5 * time.Second)
	for h.clientPath("a").State != "down" {
		if time.Now().After(deadline) {
			t.Fatal("path a never went down")
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.links["a"].set(func(l *link) { l.down = false })
	h.waitPaths(6*time.Second, "a", "b")
}

func TestFallbackDirectWhenServerGone(t *testing.T) {
	h := start(t, "bond", map[string]*link{"a": newLink(time.Millisecond, 0)}, nil)
	h.waitPaths(3*time.Second, "a")
	h.links["a"].set(func(l *link) { l.down = true })
	deadline := time.Now().Add(12 * time.Second)
	for {
		h.clientPlat.mu.Lock()
		on := h.clientPlat.routesOn
		h.clientPlat.mu.Unlock()
		if !on {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("tunnel routes still on long after every path died")
		}
		time.Sleep(50 * time.Millisecond)
	}
	h.links["a"].set(func(l *link) { l.down = false })
	h.waitPaths(8*time.Second, "a")
	time.Sleep(50 * time.Millisecond)
	h.clientPlat.mu.Lock()
	defer h.clientPlat.mu.Unlock()
	if !h.clientPlat.routesOn {
		t.Error("tunnel routes not restored when the path came back")
	}
}

func TestRedundantDeliversOnce(t *testing.T) {
	h := start(t, "redundant", map[string]*link{
		"a": newLink(5*time.Millisecond, 0),
		"b": newLink(15*time.Millisecond, 0),
		"c": newLink(25*time.Millisecond, 0),
	}, nil)
	h.waitPaths(3*time.Second, "a", "b", "c")
	const n = 300
	for i := 0; i < n; i++ {
		h.clientTun.in <- tcpPacket("10.77.0.2", "1.1.1.1", uint32(i), 300)
	}
	got := collect(h.serverTun, 300*time.Millisecond)
	if len(got) != n {
		t.Fatalf("got %d packets, want exactly %d", len(got), n)
	}
	srv := h.server.Status().Sessions[0]
	if srv.Reorder.Duplicates < n {
		t.Errorf("server dropped %d duplicates, want at least %d (3 copies of each)", srv.Reorder.Duplicates, n)
	}
	fmt.Fprint(io.Discard, srv)
}

func TestRekeyKeepsTrafficFlowing(t *testing.T) {
	h := start(t, "bond", map[string]*link{
		"a": newLink(5*time.Millisecond, 0),
		"b": newLink(15*time.Millisecond, 0),
	}, nil)
	h.waitPaths(3*time.Second, "a", "b")
	// Force a rekey by moving the current keypair's birth back.
	h.client.client.mu.Lock()
	h.client.client.current.created = time.Now().Add(-time.Hour)
	h.client.client.mu.Unlock()
	const n = 400
	for i := 0; i < n; i++ {
		h.clientTun.in <- tcpPacket("10.77.0.2", "1.1.1.1", uint32(i), 300)
		time.Sleep(2 * time.Millisecond)
	}
	got := collect(h.serverTun, 300*time.Millisecond)
	if len(got) < n-2 {
		t.Errorf("delivered %d of %d across a rekey", len(got), n)
	}
	h.client.client.mu.Lock()
	age := time.Since(h.client.client.current.created)
	h.client.client.mu.Unlock()
	if age > time.Minute {
		t.Error("client never rekeyed")
	}
}

// A path that goes away and comes back (a WAN unplugged, a modem
// re-enumerating) must work again: its nonce counter must carry on, or the
// peer's replay window for that path ID rejects everything on it.
func TestPathRemovedAndReAdded(t *testing.T) {
	h := start(t, "bond", map[string]*link{
		"a": newLink(5*time.Millisecond, 0),
		"b": newLink(10*time.Millisecond, 0),
	}, nil)
	h.waitPaths(3*time.Second, "a", "b")
	for i := 0; i < 300; i++ { // advance both paths' counters
		h.clientTun.in <- tcpPacket("10.77.0.2", "1.1.1.1", uint32(i), 300)
	}
	collect(h.serverTun, 200*time.Millisecond)

	cfg, _ := h.client.config()
	only := *cfg
	only.Paths = []config.Path{{Name: "b", Device: "b"}}
	h.client.Reload(&only)
	time.Sleep(300 * time.Millisecond)
	h.client.Reload(cfg)
	h.waitPaths(3*time.Second, "a", "b")

	h.links["b"].set(func(l *link) { l.down = true })
	time.Sleep(1500 * time.Millisecond)
	for i := 0; i < 100; i++ {
		h.clientTun.in <- tcpPacket("10.77.0.2", "1.1.1.1", uint32(1000+i), 300)
	}
	if got := collect(h.serverTun, 300*time.Millisecond); len(got) < 95 {
		t.Fatalf("only %d of 100 packets crossed the re-added path", len(got))
	}
}

// A handshake init carrying another client instance, as an attacker could
// replay from an earlier run of the client, must not reset the session or
// move its paths: the server acts on a new instance only once data arrives
// under the new keypair, which a replay can never send.
func TestReplayedInitFromOldInstanceIsHarmless(t *testing.T) {
	h := start(t, "bond", map[string]*link{
		"a": newLink(5*time.Millisecond, 0),
	}, nil)
	h.waitPaths(3*time.Second, "a")

	s := h.server.sessions[0]
	var (
		inst   uint32
		pid    uint8
		remote netip.AddrPort
	)
	for deadline := time.Now().Add(2 * time.Second); !remote.IsValid(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the server never learned the path")
		}
		s.mu.Lock()
		inst = s.peerInstance
		for id, p := range s.paths {
			if p != nil && p.remote.IsValid() {
				pid, remote = uint8(id), p.remote
			}
		}
		s.mu.Unlock()
	}

	_, msg, err := handshake.Initiate(h.client.priv, h.server.pub,
		proto.AppendHello(nil, proto.Hello{Version: proto.Version, Instance: inst + 1}))
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, proto.InitHeaderLen, proto.InitHeaderLen+len(msg))
	proto.PutInitHeader(b, pid, 0x1234)
	b = append(b, msg...)
	h.server.onInit(b, netip.MustParseAddrPort("127.0.0.1:9"))

	for i := 0; i < 50; i++ {
		h.clientTun.in <- tcpPacket("10.77.0.2", "1.1.1.1", uint32(i), 300)
	}
	if got := collect(h.serverTun, 200*time.Millisecond); len(got) < 50 {
		t.Fatalf("only %d of 50 packets crossed after the replayed init", len(got))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.peerInstance != inst {
		t.Errorf("peer instance changed to %d by an unconfirmed init", s.peerInstance)
	}
	if p := s.paths[pid]; p == nil || p.remote != remote {
		t.Errorf("path %d moved by an unconfirmed init", pid)
	}
}
