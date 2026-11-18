package engine

import (
	"context"
	"log/slog"
	"math"
	"net/netip"
	"sort"
	"sync"
	"time"

	"github.com/stoatworks-labs/weaver/internal/bond"
	"github.com/stoatworks-labs/weaver/internal/handshake"
	"github.com/stoatworks-labs/weaver/internal/proto"
)

const (
	stateDown        = "down"
	stateHandshaking = "handshaking"
	stateUp          = "up"

	handshakeRetry   = time.Second
	previousKeepFor  = 30 * time.Second
	pendingExpiry    = 10 * time.Second
	activeWindow     = 2 * time.Second
	reportActive     = 100 * time.Millisecond
	reportIdle       = time.Second
	pathInfoInterval = 5 * time.Second
	rehandshakeDown  = 3 * time.Second
	rebindAfter      = 10 * time.Second
	serverSilentDown = 18 * time.Second
	minHold          = 5 * time.Millisecond
	maxHold          = 200 * time.Millisecond // for the RTT-derived hold
	maxLearnedHold   = 500 * time.Millisecond // for one learned from late packets
	holdDecay        = 20 * time.Second       // learned hold's time constant
)

// outPkt is a frame waiting to be sealed and sent once the session lock is
// released. Its nonce was reserved under the lock.
type outPkt struct {
	conn   PacketConn
	to     netip.AddrPort
	kp     *keypair
	pathID uint8
	nonce  uint64
	frame  []byte
}

// rawPkt is an unencrypted datagram (a handshake message).
type rawPkt struct {
	conn PacketConn
	to   netip.AddrPort
	b    []byte
}

// Session is the tunnel between this end and one other: the client's single
// session with its server, or one of a server's peers.
type Session struct {
	e       *Engine
	name    string
	peerKey handshake.Key
	allowed []netip.Prefix // server: inner source addresses this peer may use

	mu sync.Mutex

	state         string
	established   time.Time
	lastHandshake time.Time
	downSince     time.Time

	current, previous, pending *keypair
	previousUntil              time.Time

	// client handshake in flight
	initiator  *handshake.Initiator
	initIndex  uint32
	initSentAt time.Time

	peerInstance uint32

	paths      map[uint8]*Path
	sched      bond.Scheduler
	reorder    bond.Reorder
	classifier *bond.Classifier
	txSeq      uint32
	// txCounters are the nonce counters by path ID. They outlive Path
	// objects: the peer's replay window for an ID outlives them too.
	txCounters [256]uint64
	tm         timing // the engine's timing, copied under s.mu on reload

	holdBoost      time.Duration // learned from packets that arrived after the hold
	lastBoostDecay time.Time

	lastActive     time.Time
	lastReport     time.Time
	lastPathInfo   time.Time
	pathInfoDirty  bool
	txBytes        uint64
	rxBytes        uint64
	droppedSpoofed uint64

	outs      []outPkt
	raws      []rawPkt
	delivered [][]byte
}

func newSession(e *Engine, name string, key handshake.Key) *Session {
	s := &Session{
		e:          e,
		name:       name,
		peerKey:    key,
		state:      stateDown,
		paths:      map[uint8]*Path{},
		classifier: bond.NewClassifier(e.tm.realtimeKbps),
	}
	s.tm = e.tm
	s.sched.Mode = e.tm.mode
	s.reorder.Deliver = func(b []byte) { s.delivered = append(s.delivered, b) }
	s.reorder.Drop = e.putBuf
	if e.server {
		s.state = stateDown
	} else {
		s.state = stateHandshaking
	}
	return s
}

// pathList returns the paths in ID order. Caller holds s.mu.
func (s *Session) pathList() []*Path {
	out := make([]*Path, 0, len(s.paths))
	for _, p := range s.paths {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (s *Session) addPath(p *Path) {
	s.paths[p.ID] = p
	s.sched.AddLane(p.lane)
	s.pathInfoDirty = true
}

func (s *Session) removePath(id uint8) {
	delete(s.paths, id)
	s.sched.RemoveLane(id)
	s.pathInfoDirty = true
}

// serverPath returns the path with an ID, creating it the first time the
// client uses it. Caller holds s.mu.
func (s *Session) serverPath(id uint8, now time.Time) *Path {
	p := s.paths[id]
	if p == nil {
		p = newPath(id, "path"+itoa(int(id)), "", bond.Primary)
		s.addPath(p)
	}
	p.conn = s.e.listen
	return p
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [4]byte
	n := len(b)
	for i > 0 {
		n--
		b[n] = byte('0' + i%10)
		i /= 10
	}
	return string(b[n:])
}

// resetForNewPeerInstance forgets sequence and path state when the far end
// has restarted. Caller holds s.mu.
func (s *Session) resetForNewPeerInstance() {
	s.reorder.Reset()
	if s.e.server {
		for id := range s.paths {
			s.removePath(id)
		}
		return
	}
	for _, p := range s.paths {
		p.resetRx()
	}
}

// sendKeypair is the keypair to send under. Caller holds s.mu.
func (s *Session) sendKeypair() *keypair { return s.current }

// queue reserves a nonce for a control frame on a path. Caller holds s.mu.
func (s *Session) queue(p *Path, frame []byte, now time.Time) {
	kp := s.sendKeypair()
	if kp == nil || p.conn == nil || !p.remote.IsValid() {
		return
	}
	nonce := p.onTx(&s.txCounters, len(frame)+proto.DataHeaderLen+proto.AEADOverhead, now)
	s.outs = append(s.outs, outPkt{conn: p.conn, to: p.remote, kp: kp, pathID: p.ID, nonce: nonce, frame: frame})
}

// takeOut hands queued work to the caller to do outside the lock.
func (s *Session) takeOut() ([]outPkt, []rawPkt, [][]byte) {
	o, r, d := s.outs, s.raws, s.delivered
	s.outs, s.raws, s.delivered = nil, nil, nil
	return o, r, d
}

// usable reports whether a path may carry traffic now. Caller holds s.mu.
func (s *Session) usable(p *Path, now time.Time) bool {
	if p.conn == nil || !p.remote.IsValid() {
		return false
	}
	if !s.e.server {
		return p.state == proto.StateUp
	}
	if now.Sub(p.lastRx) > serverSilentDown {
		return false
	}
	if p.peerStateKnown {
		return p.peerState == proto.StateUp
	}
	return now.Sub(p.lastRx) < time.Second
}

// degraded reports whether a path that isn't usable is merely suspect: worth
// using when nothing better is up. Caller holds s.mu.
func (s *Session) degraded(p *Path, now time.Time) bool {
	if p.conn == nil || !p.remote.IsValid() {
		return false
	}
	if !s.e.server {
		return p.state == proto.StateSuspect
	}
	return p.peerStateKnown && p.peerState == proto.StateSuspect && now.Sub(p.lastRx) < serverSilentDown
}

// prepareData classifies and schedules one packet from the TUN. frame is the
// packet with DataFrameHeaderLen bytes of headroom in front.
func (s *Session) prepareData(frame, pkt []byte, now time.Time, out []outPkt) []outPkt {
	s.mu.Lock()
	defer s.mu.Unlock()
	kp := s.sendKeypair()
	if kp == nil {
		return out
	}
	var (
		dup       bool
		immediate bool
	)
	mode := s.sched.Mode
	switch mode {
	case bond.Adaptive:
		switch s.classifier.Classify(pkt, now) {
		case bond.Realtime:
			dup, immediate = true, true
		case bond.Control:
			dup = true
		}
	case bond.Redundant:
		// Every copy arrives in order on its own path; the receiver takes
		// whichever is first.
	}
	var ids [8]uint8
	picked := s.sched.Pick(now, len(pkt), dup, ids[:0])
	if len(picked) == 0 {
		return out
	}
	var flags uint8
	if immediate {
		flags |= proto.FlagImmediate
	}
	proto.PutDataFrameHeader(frame, flags, s.txSeq, s.e.micros(now))
	s.txSeq++
	s.lastActive = now
	s.txBytes += uint64(len(pkt))
	wire := len(frame) + proto.DataHeaderLen + proto.AEADOverhead
	for _, id := range picked {
		p := s.paths[id]
		if p == nil || p.conn == nil || !p.remote.IsValid() {
			continue
		}
		nonce := p.onTx(&s.txCounters, wire, now)
		out = append(out, outPkt{conn: p.conn, to: p.remote, kp: kp, pathID: id, nonce: nonce, frame: frame})
	}
	return out
}

// onData handles one authenticated data packet.
func (s *Session) onData(kp *keypair, pid uint8, counter uint64, size int, from netip.AddrPort, frame []byte, scratch []byte) {
	now := time.Now()
	e := s.e
	s.mu.Lock()
	if e.server && kp == s.pending {
		s.promotePending(now)
	}
	p := s.paths[pid]
	if p == nil {
		if !e.server {
			s.mu.Unlock()
			return
		}
		p = s.serverPath(pid, now)
	}
	if e.server && p.remote != from {
		if p.remote.IsValid() {
			e.log.Debug("path moved", "peer", s.name, "path", p.Name, "from", p.remote, "to", from)
		}
		p.remote = from
	}
	p.onRx(now, counter, size)

	switch frame[0] {
	case proto.FrameData:
		flags, seq, sentUs, pkt, err := proto.ParseDataFrame(frame)
		if err != nil || len(pkt) == 0 {
			break
		}
		p.owd.Sample(sentUs, e.micros(now), now)
		if e.server && !s.allowedSource(pkt) {
			s.droppedSpoofed++
			break
		}
		buf := e.getBuf(len(pkt))
		copy(buf, pkt)
		s.lastActive = now
		s.rxBytes += uint64(len(pkt))
		s.reorder.Push(seq, buf, flags&proto.FlagImmediate != 0, now, s.holdTime())
	case proto.FrameProbe:
		if pr, err := proto.ParseProbe(frame); err == nil {
			s.queue(p, proto.AppendProbeAck(nil, pr), now)
		}
	case proto.FrameProbeAck:
		if pr, err := proto.ParseProbe(frame); err == nil && !e.server {
			s.onProbeAck(p, pr, now)
		}
	case proto.FrameReport:
		if r, err := proto.ParseReport(frame); err == nil {
			s.onReport(r, now)
		}
	case proto.FramePathInfo:
		if infos, err := proto.ParsePathInfo(frame); err == nil && e.server {
			s.onPathInfo(infos, now)
		}
	case proto.FrameKeepalive:
	}
	outs, raws, delivered := s.takeOut()
	s.mu.Unlock()
	e.flush(outs, raws, delivered, scratch)
}

func (s *Session) allowedSource(pkt []byte) bool {
	src, ok := srcAddr(pkt)
	if !ok {
		return false
	}
	for _, pfx := range s.allowed {
		if pfx.Contains(src) {
			return true
		}
	}
	return false
}

// promotePending makes the server's pending keypair current, now that the
// client has used it. Caller holds s.mu.
func (s *Session) promotePending(now time.Time) {
	if inst := s.pending.peerInstance; inst != s.peerInstance {
		// Confirmed: the client really restarted. Its sequence numbers and
		// counters start again, so forget the old ones.
		if s.peerInstance != 0 {
			s.e.log.Info("peer restarted", "peer", s.name)
		}
		s.resetForNewPeerInstance()
		s.peerInstance = inst
	}
	if s.previous != nil {
		s.e.dropIndex(s.previous.local)
	}
	s.previous, s.previousUntil = s.current, now.Add(previousKeepFor)
	s.current, s.pending = s.pending, nil
	s.lastHandshake = now
	if s.state != stateUp {
		s.state = stateUp
		s.established = now
		s.e.log.Info("peer connected", "peer", s.name)
	}
}

// startHandshake sends a fresh handshake init. Caller holds s.mu (client).
func (s *Session) startHandshake(now time.Time) {
	e := s.e
	if s.initIndex != 0 {
		e.dropIndex(s.initIndex)
	}
	idx := e.reserveIndex()
	payload := proto.AppendHello(nil, proto.Hello{Version: proto.Version, Instance: e.instance})
	ini, msg, err := handshake.Initiate(e.priv, s.peerKey, payload)
	if err != nil {
		e.log.Error("starting handshake", "err", err)
		e.dropIndex(idx)
		return
	}
	s.initiator, s.initIndex, s.initSentAt = ini, idx, now

	// While nothing is up, try every path; otherwise the two best.
	var targets []*Path
	for _, p := range s.pathList() {
		if p.conn != nil && p.remote.IsValid() {
			targets = append(targets, p)
		}
	}
	if s.state == stateUp {
		var up []*Path
		for _, p := range targets {
			if s.usable(p, now) {
				up = append(up, p)
			}
		}
		sort.Slice(up, func(i, j int) bool { return up[i].srtt() < up[j].srtt() })
		if len(up) > 2 {
			up = up[:2]
		}
		if len(up) > 0 {
			targets = up
		}
	}
	for _, p := range targets {
		b := make([]byte, proto.InitHeaderLen, proto.InitHeaderLen+len(msg))
		proto.PutInitHeader(b, p.ID, idx)
		b = append(b, msg...)
		s.raws = append(s.raws, rawPkt{conn: p.conn, to: p.remote, b: b})
	}
	if len(targets) > 0 {
		e.log.Debug("handshake sent", "paths", len(targets))
	}
}

// onResp completes the client's handshake.
func (s *Session) onResp(b []byte, from netip.AddrPort, scratch []byte) {
	pid, sender, receiver, msg, err := proto.ParseRespHeader(b)
	if err != nil {
		return
	}
	e := s.e
	now := time.Now()
	s.mu.Lock()
	if s.initiator == nil || receiver != s.initIndex {
		s.mu.Unlock()
		return
	}
	send, recv, payload, err := s.initiator.Finish(msg)
	s.initiator = nil
	if err != nil {
		e.log.Debug("handshake response rejected", "err", err)
		s.mu.Unlock()
		return
	}
	hello, err := proto.ParseHello(payload)
	if err != nil || hello.Version != proto.Version {
		e.log.Warn("server speaks a different protocol version")
		s.mu.Unlock()
		return
	}
	kp := &keypair{session: s, local: s.initIndex, remote: sender, send: send, recv: recv, created: now}
	e.setIndex(s.initIndex, kp)
	s.initIndex = 0
	if hello.Instance != s.peerInstance {
		if s.peerInstance != 0 {
			e.log.Info("server restarted")
		}
		s.resetForNewPeerInstance()
		s.peerInstance = hello.Instance
	}
	if s.previous != nil {
		e.dropIndex(s.previous.local)
	}
	s.previous, s.previousUntil = s.current, now.Add(previousKeepFor)
	s.current = kp
	s.lastHandshake = now
	if p := s.paths[pid]; p != nil {
		p.lastRx = now
		if p.state != proto.StateUp {
			p.setState(proto.StateUp, now)
		}
	}
	if s.state != stateUp {
		s.state = stateUp
		s.established = now
		s.downSince = time.Time{}
		e.log.Info("connected", "server", from)
	}
	// Probe every path now: it confirms the keypair to the server and
	// measures the paths straight away.
	for _, p := range s.paths {
		p.lastProbe = time.Time{}
	}
	s.pathInfoDirty = true
	s.lastReport = time.Time{}
	s.tickLocked(now)
	outs, raws, delivered := s.takeOut()
	s.mu.Unlock()
	e.flush(outs, raws, delivered, scratch)
}

func (s *Session) onProbeAck(p *Path, pr proto.Probe, now time.Time) {
	sent, ok := p.takeProbe(pr.ID)
	if !ok {
		return
	}
	sample := now.Sub(sent) - time.Duration(pr.HoldUs)*time.Microsecond
	if !p.rtt.Valid() {
		// The server sizes its reorder hold from these; tell it now.
		s.lastReport = time.Time{}
	}
	p.rtt.Update(sample, now)
	p.acks++
	s.clientPathState(p, now, s.tm)
}

// onReport applies the far end's account of what it received from us.
func (s *Session) onReport(r proto.Report, now time.Time) {
	for _, pr := range r.Paths {
		p := s.paths[pr.PathID]
		if p == nil {
			continue
		}
		if s.e.server {
			p.peerState, p.peerStateKnown = pr.State, true
			if pr.SRTTUs > 0 {
				p.rtt.Seed(time.Duration(pr.SRTTUs)*time.Microsecond, time.Duration(pr.RTTVarUs)*time.Microsecond, now)
			}
		}
		if p.haveRep && r.At <= p.lastRepAt {
			continue // a duplicate (reports go on two paths) or an old one
		}
		if p.haveRep && pr.HighestCounter < p.lastRep.HighestCounter {
			p.haveRep = false // the far end started over
		}
		if p.haveRep {
			dt := time.Duration(r.At - p.lastRepAt)
			dHigh := pr.HighestCounter - p.lastRep.HighestCounter
			dRx := pr.RxPackets - p.lastRep.RxPackets
			// The far end counts UDP payloads; the link also carries the
			// outer IPv4 and UDP headers.
			dBytes := pr.RxBytes - p.lastRep.RxBytes + uint64(uint32(dRx))*28
			if dHigh > 0 {
				loss := 1 - float64(dRx)/float64(dHigh)
				if loss < 0 {
					loss = 0
				}
				if loss > 1 {
					loss = 1
				}
				w := 0.3
				if dHigh < 10 {
					w = 0.3 * float64(dHigh) / 10
				}
				p.loss = (1-w)*p.loss + w*loss
			}
			if dt > 0 {
				f := bond.Feedback{
					Delivered: float64(dBytes) / dt.Seconds(),
					Loss:      p.loss,
					Queue:     time.Duration(pr.QueueUs) * time.Microsecond,
					QueueMin:  time.Duration(pr.QueueMinUs) * time.Microsecond,
					SRTT:      p.rtt.SRTT,
					MinRTT:    p.rtt.Min(now),
				}
				p.lane.OnFeedback(now, f)
				if s.e.log.Enabled(context.Background(), levelTrace) {
					s.e.log.Log(context.Background(), levelTrace, "feedback", "path", p.Name, "dt_ms", ms(dt),
						"delivered_kbps", f.Delivered*8/1000, "loss", f.Loss, "queue_ms", ms(f.Queue), "queue_min_ms", ms(f.QueueMin), "srtt_ms", ms(f.SRTT),
						"min_ms", ms(f.MinRTT), "rate_kbps", p.lane.Rate*8/1000)
				}
			}
		}
		p.lastRep, p.lastRepAt, p.haveRep = pr, r.At, true
	}
}

// onPathInfo is the server learning the client's paths.
func (s *Session) onPathInfo(infos []proto.PathInfo, now time.Time) {
	seen := map[uint8]bool{}
	for _, in := range infos {
		seen[in.PathID] = true
		p := s.serverPath(in.PathID, now)
		p.Name = in.Name
		p.Priority = bond.Priority(in.Priority)
		p.Metered = in.Metered
		p.UpKbps, p.DownKbps = int(in.UpKbps), int(in.DownKbps)
		p.lane.Priority = p.Priority
		p.lane.Cap = kbpsToBytes(p.DownKbps)
	}
	for id := range s.paths {
		if !seen[id] {
			s.removePath(id)
		}
	}
}

// holdTime is how long the receiver waits for a gap. Caller holds s.mu.
func (s *Session) holdTime() time.Duration {
	if f := s.tm.reorderFixed; f > 0 {
		return f
	}
	var lo, hi, maxVar time.Duration
	n := 0
	for _, p := range s.paths {
		if !p.lane.Usable {
			continue
		}
		r := p.srtt() // an unmeasured path counts as slow, so the hold errs long
		if n == 0 || r < lo {
			lo = r
		}
		if r > hi {
			hi = r
		}
		if p.rtt.RTTVar > maxVar {
			maxVar = p.rtt.RTTVar
		}
		n++
	}
	h := (hi-lo)/2 + 2*maxVar + minHold
	if h < minHold {
		h = minHold
	}
	if h > maxHold {
		h = maxHold
	}
	if s.holdBoost > h {
		h = s.holdBoost
	}
	return h
}

// learnHold grows the hold to cover packets that arrived after it, and lets
// it shrink back slowly when they stop. Caller holds s.mu.
func (s *Session) learnHold(now time.Time) {
	if !s.lastBoostDecay.IsZero() && s.holdBoost > 0 {
		dt := now.Sub(s.lastBoostDecay)
		s.holdBoost -= time.Duration(float64(s.holdBoost) * dt.Seconds() / holdDecay.Seconds())
	}
	s.lastBoostDecay = now
	if late := s.reorder.LateBy(); late > 0 {
		want := late*5/4 + minHold
		if want > maxLearnedHold {
			want = maxLearnedHold
		}
		if want > s.holdBoost {
			s.holdBoost = want
			s.e.log.Debug("reorder hold grown", "peer", s.name, "late_ms", ms(late), "hold_ms", ms(want))
		}
	}
}

// probeInterval is how often the client probes a path right now.
func (s *Session) probeInterval(p *Path, now time.Time, tm timing, primariesUsable bool) time.Duration {
	active := now.Sub(s.lastActive) < activeWindow
	inUse := active && (p.Priority != bond.Backup || !primariesUsable)
	switch {
	case inUse:
		return tm.probe
	case p.Metered || p.Priority == bond.Backup:
		return tm.slowProbe
	}
	return tm.idleProbe
}

// clientPathState moves a path between up, suspect and down. Caller holds s.mu.
func (s *Session) clientPathState(p *Path, now time.Time, tm timing) {
	interval := s.probeInterval(p, now, tm, s.primariesUsable())
	downAfter := tm.pathTimeout
	if d := interval * 7 / 2; d > downAfter {
		downAfter = d
	}
	silent := p.lastRx.IsZero() || now.Sub(p.lastRx) > downAfter
	late := false
	if oldest := p.oldestOutstanding(now); !oldest.IsZero() {
		thr := time.Second
		if p.rtt.Valid() {
			thr = p.rtt.SRTT + 4*p.rtt.RTTVar + 50*time.Millisecond
		}
		late = now.Sub(oldest) > thr
	}
	prev := p.state
	switch p.state {
	case proto.StateUp:
		if silent {
			p.setState(proto.StateDown, now)
		} else if late {
			p.setState(proto.StateSuspect, now)
		}
	case proto.StateSuspect:
		if silent {
			p.setState(proto.StateDown, now)
		} else if !late {
			p.setState(proto.StateUp, now)
		}
	default:
		need := 1
		if p.everUp {
			need = 3
		}
		if !silent && !late && p.acks >= need {
			p.setState(proto.StateUp, now)
		}
	}
	if p.state != prev {
		s.e.log.Info("path "+stateName(p.state), "path", p.Name, "device", p.Device,
			"rtt_ms", ms(p.rtt.SRTT), "was", stateName(prev))
	}
}

func (s *Session) primariesUsable() bool {
	for _, p := range s.paths {
		if p.state == proto.StateUp && (p.Priority == bond.Primary || p.Priority == bond.Secondary) {
			return true
		}
	}
	return false
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func durUs(d time.Duration) uint32 {
	us := d.Microseconds()
	switch {
	case us < 0:
		return 0
	case us > math.MaxUint32:
		return math.MaxUint32
	}
	return uint32(us)
}

// tick runs timers: handshakes, probes, path states, reports, reordering.
func (s *Session) tick(now time.Time, scratch []byte) {
	s.mu.Lock()
	s.tickLocked(now)
	outs, raws, delivered := s.takeOut()
	s.mu.Unlock()
	s.e.flush(outs, raws, delivered, scratch)
}

func (s *Session) tickLocked(now time.Time) {
	e := s.e
	tm := s.tm

	if !e.server {
		s.tickClientHandshake(now, tm)
	} else {
		if s.pending != nil && now.Sub(s.pending.created) > pendingExpiry {
			e.dropIndex(s.pending.local)
			s.pending = nil
		}
	}
	if s.previous != nil && now.After(s.previousUntil) {
		e.dropIndex(s.previous.local)
		s.previous = nil
	}

	// Paths: state, probes, lanes.
	anyUsable := false
	primaries := s.primariesUsable()
	for _, p := range s.paths {
		if !e.server {
			s.clientPathState(p, now, tm)
			if s.current != nil && now.Sub(p.lastProbe) >= s.probeInterval(p, now, tm, primaries) {
				id := p.recordProbe(now)
				s.queue(p, proto.AppendProbe(nil, proto.Probe{ID: id, SentAt: e.mono(now)}), now)
			}
			if p.state == proto.StateDown && now.Sub(p.stateSince) > rebindAfter && now.Sub(p.lastRebind) > rebindAfter {
				p.needRebind = true
			}
		}
		u := s.usable(p, now)
		if u && !p.lane.Usable {
			p.lane.UsableSince = now
		}
		p.lane.Usable = u
		p.lane.Degraded = !u && s.degraded(p, now)
		p.lane.SRTT = p.srtt()
		if p.rtt.Valid() {
			p.lane.MinRTT = p.rtt.Min(now)
		}
		p.lane.Priority = p.Priority
		if u || p.lane.Degraded {
			anyUsable = true
		}
	}

	// Session state.
	switch {
	case s.current == nil:
		if !e.server {
			s.state = stateHandshaking
		}
	case anyUsable:
		if s.state != stateUp {
			s.state = stateUp
			s.downSince = time.Time{}
			if s.established.IsZero() {
				s.established = now
			}
		}
	default:
		if s.state == stateUp {
			s.state = stateDown
			s.downSince = now
			e.log.Warn("all paths down", "peer", s.name)
		}
	}

	if s.current != nil {
		// Reports.
		interval := reportIdle
		if now.Sub(s.lastActive) < activeWindow {
			interval = reportActive
		}
		if now.Sub(s.lastReport) >= interval {
			s.lastReport = now
			s.sendToBest(s.buildReport(now), now)
		}
		// Path info, client to server.
		if !e.server && (s.pathInfoDirty || now.Sub(s.lastPathInfo) >= pathInfoInterval) {
			s.pathInfoDirty = false
			s.lastPathInfo = now
			s.sendToBest(s.buildPathInfo(), now)
		}
	}

	s.learnHold(now)
	hold := s.holdTime()
	if d := s.reorder.Deadline(); !d.IsZero() && !now.Before(d) {
		s.reorder.Tick(now, hold)
	}
}

func (s *Session) tickClientHandshake(now time.Time, tm timing) {
	if s.initiator != nil && now.Sub(s.initSentAt) < handshakeRetry {
		return
	}
	due := false
	switch {
	case s.current == nil:
		due = true
	case now.Sub(s.current.created) >= tm.rekey:
		due = true
	case s.state == stateDown && now.Sub(s.downSince) >= rehandshakeDown:
		// Every path is silent: the server may have lost our session.
		due = true
	}
	if due {
		s.startHandshake(now)
	}
}

// sendToBest queues a control frame on the two best usable paths, or on
// every path when none is usable. Caller holds s.mu.
func (s *Session) sendToBest(frame []byte, now time.Time) {
	var up []*Path
	for _, p := range s.paths {
		if s.usable(p, now) {
			up = append(up, p)
		}
	}
	if len(up) == 0 {
		for _, p := range s.paths {
			if p.conn != nil && p.remote.IsValid() {
				s.queue(p, frame, now)
			}
		}
		return
	}
	sort.Slice(up, func(i, j int) bool { return up[i].srtt() < up[j].srtt() })
	if len(up) > 2 {
		up = up[:2]
	}
	for _, p := range up {
		s.queue(p, frame, now)
	}
}

func (s *Session) buildReport(now time.Time) []byte {
	r := proto.Report{At: s.e.mono(now)}
	for _, p := range s.pathList() {
		pr := proto.PathReport{
			PathID:         p.ID,
			RxPackets:      uint32(p.rxPackets),
			RxBytes:        p.rxBytes,
			HighestCounter: p.rxHighest,
		}
		q, qmin := p.owd.Report()
		pr.QueueUs, pr.QueueMinUs = durUs(q), durUs(qmin)
		if s.e.server {
			pr.State = proto.StateDown
			if s.usable(p, now) {
				pr.State = proto.StateUp
			}
		} else {
			pr.State = p.state
			if p.rtt.Valid() {
				pr.SRTTUs = uint32(p.rtt.SRTT.Microseconds())
				pr.RTTVarUs = uint32(p.rtt.RTTVar.Microseconds())
			}
		}
		r.Paths = append(r.Paths, pr)
	}
	return proto.AppendReport(nil, r)
}

func (s *Session) buildPathInfo() []byte {
	var infos []proto.PathInfo
	for _, p := range s.pathList() {
		infos = append(infos, proto.PathInfo{
			PathID:   p.ID,
			Priority: uint8(p.Priority),
			Metered:  p.Metered,
			UpKbps:   uint32(p.UpKbps),
			DownKbps: uint32(p.DownKbps),
			Name:     p.Name,
		})
	}
	return proto.AppendPathInfo(nil, infos)
}

// levelTrace is below debug: per-report rate feedback.
const levelTrace = slog.LevelDebug - 4
