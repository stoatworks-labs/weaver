package engine

import (
	"net/netip"
	"sync"
	"time"

	"github.com/stoatworks-labs/weaver/internal/bond"
	"github.com/stoatworks-labs/weaver/internal/proto"
	"github.com/stoatworks-labs/weaver/internal/replay"
)

// keypair is one completed handshake: a receive index and two ciphers.
type keypair struct {
	session       *Session
	local, remote uint32
	send, recv    cipher
	created       time.Time
	// peerInstance is the far end's instance ID from this handshake. The
	// server acts on a change only once data arrives under the keypair, so a
	// replayed init (which can never produce data) can't reset a session.
	peerInstance uint32

	mu      sync.Mutex
	windows [256]*replay.Window
}

type cipher interface {
	Encrypt(out []byte, n uint64, ad, plaintext []byte) []byte
	Decrypt(out []byte, n uint64, ad, ciphertext []byte) ([]byte, error)
}

// accept runs the per-path replay check after a packet has authenticated.
func (k *keypair) accept(pathID uint8, counter uint64) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	w := k.windows[pathID]
	if w == nil {
		w = &replay.Window{}
		k.windows[pathID] = w
	}
	return w.Accept(counter)
}

// meter counts bytes in one-second windows.
type meter struct {
	start time.Time
	cur   uint64
	last  uint64 // bytes in the last full second
}

func (m *meter) add(now time.Time, n int) {
	m.roll(now)
	m.cur += uint64(n)
}

func (m *meter) roll(now time.Time) {
	switch d := now.Sub(m.start); {
	case m.start.IsZero():
		m.start = now
	case d >= 2*time.Second:
		m.start, m.cur, m.last = now, 0, 0
	case d >= time.Second:
		m.start, m.last, m.cur = m.start.Add(time.Second), m.cur, 0
	}
}

func (m *meter) kbps(now time.Time) float64 {
	m.roll(now)
	return float64(m.last) * 8 / 1000
}

type probeRec struct {
	id   uint32
	sent time.Time
}

// Path is one WAN path within a session. On the client it owns a socket; on
// the server it is an address learned from the client's packets.
type Path struct {
	ID       uint8
	Name     string
	Device   string
	Priority bond.Priority
	Metered  bool
	UpKbps   int
	DownKbps int

	conn   PacketConn
	remote netip.AddrPort

	state      uint8 // proto.StateDown, StateSuspect, StateUp
	stateSince time.Time
	everUp     bool
	acks       int
	lastRx     time.Time
	rtt        bond.RTT
	loss       float64

	// client probing
	probeID    uint32
	probes     []probeRec // outstanding, oldest first
	lastProbe  time.Time
	needRebind bool
	rebinding  bool
	lastRebind time.Time

	// server: what the client says about this path
	peerState      uint8
	peerStateKnown bool

	// what this end sent on the path (the nonce counter is the session's)
	txPackets uint64
	txBytes   uint64
	txMeter   meter

	// what this end received on the path
	rxPackets uint64
	rxBytes   uint64
	rxHighest uint64
	rxMeter   meter
	owd       bond.OWD // queueing delay of DATA frames arriving here

	// the far end's last report about what it received from us here
	lastRep   proto.PathReport
	lastRepAt uint64
	haveRep   bool

	lane *bond.Lane
}

func newPath(id uint8, name, device string, prio bond.Priority) *Path {
	return &Path{
		ID:       id,
		Name:     name,
		Device:   device,
		Priority: prio,
		state:    proto.StateDown,
		lane:     bond.NewLane(id, prio, 0),
	}
}

func (p *Path) setState(st uint8, now time.Time) {
	if p.state == st {
		return
	}
	p.state = st
	p.stateSince = now
	if st == proto.StateUp {
		p.everUp = true
	} else {
		p.acks = 0
	}
}

func (p *Path) onRx(now time.Time, counter uint64, size int) {
	p.lastRx = now
	p.rxPackets++
	p.rxBytes += uint64(size)
	if counter > p.rxHighest {
		p.rxHighest = counter
	}
	p.rxMeter.add(now, size)
}

func (p *Path) onTx(counters *[256]uint64, size int, now time.Time) uint64 {
	c := counters[p.ID]
	counters[p.ID]++
	p.txPackets++
	p.txBytes += uint64(size)
	p.txMeter.add(now, size)
	return proto.Nonce(p.ID, c)
}

func (p *Path) resetRx() {
	p.rxPackets, p.rxBytes, p.rxHighest = 0, 0, 0
	p.owd.Reset()
	p.haveRep = false
	p.loss = 0
}

func (p *Path) recordProbe(now time.Time) uint32 {
	p.probeID++
	p.probes = append(p.probes, probeRec{id: p.probeID, sent: now})
	if len(p.probes) > 64 {
		p.probes = p.probes[len(p.probes)-64:]
	}
	p.lastProbe = now
	return p.probeID
}

// takeProbe removes an answered probe and every older one (a later probe
// answered means the older ones were lost), returning when it was sent.
func (p *Path) takeProbe(id uint32) (time.Time, bool) {
	for i, r := range p.probes {
		if r.id == id {
			p.probes = append(p.probes[:0], p.probes[i+1:]...)
			return r.sent, true
		}
	}
	return time.Time{}, false
}

// oldestOutstanding is when the oldest unanswered probe went out.
func (p *Path) oldestOutstanding(now time.Time) time.Time {
	// Forget probes older than 5 s: they are lost, and the down timer covers
	// a silent path.
	cut := 0
	for cut < len(p.probes) && now.Sub(p.probes[cut].sent) > 5*time.Second {
		cut++
	}
	if cut > 0 {
		p.probes = append(p.probes[:0], p.probes[cut:]...)
	}
	if len(p.probes) == 0 {
		return time.Time{}
	}
	return p.probes[0].sent
}

func (p *Path) srtt() time.Duration {
	if p.rtt.Valid() {
		return p.rtt.SRTT
	}
	return 100 * time.Millisecond
}

func stateName(s uint8) string {
	switch s {
	case proto.StateUp:
		return "up"
	case proto.StateSuspect:
		return "suspect"
	}
	return "down"
}
