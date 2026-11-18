package bond

import (
	"encoding/binary"
	"testing"
	"time"
)

var t0 = time.Unix(1_800_000_000, 0)

type sink struct {
	got  []uint32
	drop int
}

func (s *sink) reorder() *Reorder {
	return &Reorder{
		Deliver: func(b []byte) { s.got = append(s.got, binary.BigEndian.Uint32(b)) },
		Drop:    func([]byte) { s.drop++ },
	}
}

func pkt(seq uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, seq)
	return b
}

func eq(a, b []uint32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestReorderPutsBackInOrder(t *testing.T) {
	var s sink
	r := s.reorder()
	hold := 20 * time.Millisecond
	for _, q := range []uint32{100, 102, 103, 101, 104} {
		r.Push(q, pkt(q), false, t0, hold)
	}
	if want := []uint32{100, 101, 102, 103, 104}; !eq(s.got, want) {
		t.Fatalf("got %v, want %v", s.got, want)
	}
	if r.Stats.Held != 0 || !r.Deadline().IsZero() {
		t.Fatalf("still holding %d", r.Stats.Held)
	}
}

func TestReorderSkipsGapAfterHold(t *testing.T) {
	var s sink
	r := s.reorder()
	hold := 20 * time.Millisecond
	r.Push(1, pkt(1), false, t0, hold)
	r.Push(3, pkt(3), false, t0, hold)
	r.Push(4, pkt(4), false, t0.Add(5*time.Millisecond), hold)
	r.Tick(t0.Add(19*time.Millisecond), hold)
	if !eq(s.got, []uint32{1}) {
		t.Fatalf("released too early: %v", s.got)
	}
	r.Tick(t0.Add(20*time.Millisecond), hold)
	if !eq(s.got, []uint32{1, 3, 4}) {
		t.Fatalf("got %v after the hold", s.got)
	}
	// The missing packet turns up late: delivered once, then deduplicated.
	r.Push(2, pkt(2), false, t0.Add(30*time.Millisecond), hold)
	r.Push(2, pkt(2), false, t0.Add(31*time.Millisecond), hold)
	if !eq(s.got, []uint32{1, 3, 4, 2}) || r.Stats.Late != 1 || r.Stats.Duplicates != 1 {
		t.Fatalf("got %v, late %d, dup %d", s.got, r.Stats.Late, r.Stats.Duplicates)
	}
}

func TestReorderImmediateSkipsTheQueueButNotDedup(t *testing.T) {
	var s sink
	r := s.reorder()
	hold := 50 * time.Millisecond
	r.Push(10, pkt(10), false, t0, hold)
	r.Push(12, pkt(12), false, t0, hold) // held behind 11
	r.Push(13, pkt(13), true, t0, hold)  // immediate: out now
	r.Push(13, pkt(13), true, t0, hold)  // its duplicate
	r.Push(11, pkt(11), false, t0, hold) // fills the gap
	if want := []uint32{10, 13, 11, 12}; !eq(s.got, want) {
		t.Fatalf("got %v, want %v", s.got, want)
	}
	if r.Stats.Duplicates != 1 {
		t.Fatalf("duplicates %d", r.Stats.Duplicates)
	}
}

func TestReorderWrapsAndJumps(t *testing.T) {
	var s sink
	r := s.reorder()
	hold := time.Millisecond
	start := uint32(0xffff_fff0)
	for i := uint32(0); i < 40; i++ {
		r.Push(start+i, pkt(start+i), false, t0, hold)
	}
	if len(s.got) != 40 || s.got[39] != start+39 {
		t.Fatalf("across the wrap: %d delivered, last %d", len(s.got), s.got[len(s.got)-1])
	}
	// A packet far ahead releases everything and moves on.
	far := start + 40 + 3*RingSize
	r.Push(far, pkt(far), false, t0, hold)
	r.Tick(t0.Add(time.Second), hold)
	if s.got[len(s.got)-1] != far {
		t.Fatalf("far jump not delivered: last %d", s.got[len(s.got)-1])
	}
}

func lanes(rates ...float64) *Scheduler {
	s := &Scheduler{Mode: Bond}
	for i, r := range rates {
		l := NewLane(uint8(i), Primary, 0)
		l.Rate = r
		l.Usable = true
		l.SRTT = 20 * time.Millisecond
		s.AddLane(l)
	}
	return s
}

func TestBondSplitsByRate(t *testing.T) {
	s := lanes(3e6, 1e6)
	count := map[uint8]int{}
	now := t0
	for i := 0; i < 4000; i++ {
		ids := s.Pick(now, 1200, false, nil)
		count[ids[0]]++
		now = now.Add(200 * time.Microsecond) // 6 MB/s offered, more than both
	}
	ratio := float64(count[0]) / float64(count[1])
	if ratio < 2.5 || ratio > 3.5 {
		t.Fatalf("split %v, ratio %.2f, want about 3", count, ratio)
	}
}

func TestBondPrefersLowLatencyWhenIdle(t *testing.T) {
	s := lanes(1e6, 1e6)
	s.Lanes[1].SRTT = 5 * time.Millisecond
	now := t0
	for i := 0; i < 50; i++ {
		ids := s.Pick(now, 100, false, nil)
		if ids[0] != 1 {
			t.Fatalf("packet %d went on the slow path while idle", i)
		}
		now = now.Add(10 * time.Millisecond)
	}
}

func TestBackupOnlyWhenNothingElse(t *testing.T) {
	s := lanes(1e6, 1e6)
	s.Lanes[1].Priority = Backup
	for i := 0; i < 100; i++ {
		if ids := s.Pick(t0, 1000, false, nil); ids[0] != 0 {
			t.Fatal("backup used while the primary was up")
		}
	}
	s.Lanes[0].Usable = false
	if ids := s.Pick(t0, 1000, false, nil); len(ids) != 1 || ids[0] != 1 {
		t.Fatalf("backup not used when the primary died: %v", ids)
	}
	s.Lanes[1].Usable = false
	if ids := s.Pick(t0, 1000, false, nil); len(ids) != 0 {
		t.Fatalf("picked %v with nothing usable", ids)
	}
}

func TestAdaptiveDuplicatesOnTwoBest(t *testing.T) {
	s := lanes(1e6, 1e6, 1e6)
	s.Mode = Adaptive
	s.Lanes[0].SRTT = 50 * time.Millisecond
	ids := s.Pick(t0, 100, true, nil)
	if len(ids) != 2 || ids[0] == 0 || ids[1] == 0 {
		t.Fatalf("duplicated onto %v; want the two faster lanes", ids)
	}
}

func TestFailoverSticksThenFailsBack(t *testing.T) {
	s := lanes(1e6, 1e6)
	s.Mode = Failover
	s.Lanes[1].Priority = Secondary
	if ids := s.Pick(t0, 100, false, nil); ids[0] != 0 {
		t.Fatal("failover did not start on the primary")
	}
	s.Lanes[0].Usable = false
	if ids := s.Pick(t0, 100, false, nil); ids[0] != 1 {
		t.Fatal("did not fail over")
	}
	s.Lanes[0].Usable = true
	s.Lanes[0].UsableSince = t0.Add(time.Second)
	if ids := s.Pick(t0.Add(2*time.Second), 100, false, nil); ids[0] != 1 {
		t.Fatal("failed back before the primary was stable")
	}
	if ids := s.Pick(t0.Add(7*time.Second), 100, false, nil); ids[0] != 0 {
		t.Fatal("never failed back")
	}
}

func TestRateFollowsDeliveryWhenFull(t *testing.T) {
	l := NewLane(0, Primary, 0)
	now := t0
	l.OnFeedback(now, Feedback{}) // first call only starts the interval
	for i := 0; i < 50; i++ {
		// Keep the lane overloaded: assign twice what it delivers, with a
		// standing queue at the far end.
		for j := 0; j < 100; j++ {
			l.assign(now, 2000)
		}
		now = now.Add(100 * time.Millisecond)
		l.OnFeedback(now, Feedback{Delivered: 1e6, Queue: 80 * time.Millisecond, QueueMin: 60 * time.Millisecond})
	}
	if l.Rate < 0.95e6 || l.Rate > 1.05e6 {
		t.Fatalf("rate %.0f on a full path; want the 1e6 delivered", l.Rate)
	}
	// Load stops while the queue drains; that must not drag it down.
	for i := 0; i < 50; i++ {
		now = now.Add(100 * time.Millisecond)
		l.OnFeedback(now, Feedback{Delivered: 1e3, Loss: 0.2, QueueMin: 30 * time.Millisecond})
	}
	if l.Rate < 0.9e6 {
		t.Fatalf("idle feedback pulled the rate down to %.0f", l.Rate)
	}
}

func TestRandomLossIsNotFull(t *testing.T) {
	l := NewLane(0, Primary, 0)
	l.Rate = 1e6
	now := t0
	l.OnFeedback(now, Feedback{})
	for i := 0; i < 20; i++ {
		for j := 0; j < 60; j++ {
			l.assign(now, 2000)
		}
		now = now.Add(100 * time.Millisecond)
		l.OnFeedback(now, Feedback{Delivered: 0.95 * l.Rate, Loss: 0.05})
	}
	if l.Rate < 1e6 {
		t.Fatalf("5%% random loss without a queue pulled the rate to %.0f", l.Rate)
	}
}

func TestRateGrowsWhenPushedWithoutQueue(t *testing.T) {
	l := NewLane(0, Primary, 5e6)
	l.Rate = 1e6
	now := t0
	l.OnFeedback(now, Feedback{})
	for i := 0; i < 30; i++ {
		for j := 0; j < 60; j++ {
			l.assign(now, 2000) // ~1.2 MB/s over 100 ms
		}
		now = now.Add(100 * time.Millisecond)
		l.OnFeedback(now, Feedback{Delivered: l.Rate})
	}
	if l.Rate <= 1.2e6 {
		t.Fatalf("rate %.0f never grew", l.Rate)
	}
	if l.Rate > 5e6 {
		t.Fatalf("rate %.0f ignored the cap", l.Rate)
	}
}

func TestMeasuredQueueRaisesETA(t *testing.T) {
	s := lanes(1e6, 1e6)
	s.Lanes[1].SRTT = 40 * time.Millisecond
	// Lane 0 is nearer, so it wins while both are empty...
	if ids := s.Pick(t0, 100, false, nil); ids[0] != 0 {
		t.Fatal("idle pick went to the far lane")
	}
	// ...until the far end says its queue is deep.
	s.Lanes[0].OnFeedback(t0, Feedback{Queue: 50 * time.Millisecond})
	if ids := s.Pick(t0, 100, false, nil); ids[0] != 1 {
		t.Fatal("a measured queue did not move traffic off the lane")
	}
}

func TestBondFillsSlowPathOnceFastQueues(t *testing.T) {
	// 3 MB/s at 10 ms one way, 1 MB/s at 40 ms: offered 5 MB/s, the fast
	// path takes everything until its queue is ~30 ms deep, then both run
	// at their rates.
	s := lanes(3e6, 1e6)
	s.Lanes[0].SRTT, s.Lanes[1].SRTT = 20*time.Millisecond, 80*time.Millisecond
	count := map[uint8]int{}
	now := t0
	for i := 0; i < 20000; i++ {
		ids := s.Pick(now, 1200, false, nil)
		if i >= 5000 {
			count[ids[0]]++
		}
		now = now.Add(250 * time.Microsecond)
	}
	ratio := float64(count[0]) / float64(count[1])
	if ratio < 2.5 || ratio > 3.5 {
		t.Fatalf("split %v, ratio %.2f, want about 3", count, ratio)
	}
	if b := s.Lanes[0].Backlog(now); b < 20*time.Millisecond {
		t.Fatalf("fast lane backlog %v; it should queue about the 30 ms difference", b)
	}
}

func TestOWDMeasuresQueueing(t *testing.T) {
	var o OWD
	now := t0
	// Clocks 1e9 µs apart (wrapping), 10 ms of distance, no queue.
	offset := uint32(0xfffff000)
	sent := uint32(0)
	for i := 0; i < 20; i++ {
		o.Sample(sent, sent+offset+10000, now)
		sent += 1000
		now = now.Add(time.Millisecond)
	}
	if q, qmin := o.Report(); q > time.Millisecond || qmin != 0 {
		t.Fatalf("empty path: queue %v min %v", q, qmin)
	}
	// A 30 ms standing queue builds.
	for i := 0; i < 100; i++ {
		o.Sample(sent, sent+offset+40000, now)
		sent += 1000
		now = now.Add(time.Millisecond)
	}
	q, qmin := o.Report()
	if q < 25*time.Millisecond || q > 31*time.Millisecond || qmin != 30*time.Millisecond {
		t.Fatalf("standing queue: queue %v min %v", q, qmin)
	}
	// Too few frames: no minimum; none at all: no queue either.
	o.Sample(sent, sent+offset+40000, now)
	if _, qmin := o.Report(); qmin != 0 {
		t.Fatalf("one frame gave an interval minimum %v", qmin)
	}
	if q, _ := o.Report(); q != 0 {
		t.Fatalf("idle interval reported queue %v", q)
	}
}

func ipv4(proto uint8, tos uint8, l4 []byte) []byte {
	b := make([]byte, 20+len(l4))
	b[0], b[1], b[9] = 0x45, tos, proto
	binary.BigEndian.PutUint16(b[2:4], uint16(len(b)))
	copy(b[12:], []byte{10, 77, 0, 2, 1, 1, 1, 1})
	copy(b[20:], l4)
	return b
}

func udp(sport, dport uint16, n int) []byte {
	b := make([]byte, 8+n)
	binary.BigEndian.PutUint16(b[0:2], sport)
	binary.BigEndian.PutUint16(b[2:4], dport)
	return b
}

func tcp(flags uint8, payload int) []byte {
	b := make([]byte, 20+payload)
	b[12], b[13] = 5<<4, flags
	return b
}

func TestClassify(t *testing.T) {
	c := NewClassifier(2000)
	cases := []struct {
		name string
		pkt  []byte
		want Class
	}{
		{"dns", ipv4(17, 0, udp(40000, 53, 40)), Realtime},
		{"sip", ipv4(17, 0, udp(5060, 5060, 200)), Realtime},
		{"ef marked", ipv4(6, 46<<2, tcp(0x10, 1000)), Realtime},
		{"icmp", ipv4(1, 0, make([]byte, 8)), Realtime},
		{"syn", ipv4(6, 0, tcp(0x02, 0)), Control},
		{"pure ack", ipv4(6, 0, tcp(0x10, 0)), Control},
		{"tcp data", ipv4(6, 0, tcp(0x18, 1200)), Bulk},
	}
	for _, tc := range cases {
		if got := c.Classify(tc.pkt, t0); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestClassifyUDPFlowByRate(t *testing.T) {
	c := NewClassifier(2000) // 250 kB/s
	call := ipv4(17, 0, udp(50000, 40000, 160))
	bulk := ipv4(17, 0, udp(50001, 443, 1200))
	now := t0
	var callRT, bulkRT int
	for i := 0; i < 2000; i++ { // 2 s
		if c.Classify(call, now) == Realtime {
			callRT++
		}
		for j := 0; j < 5; j++ { // 5 × 1228 B per ms ≈ 6 MB/s
			if c.Classify(bulk, now) == Realtime {
				bulkRT++
			}
		}
		now = now.Add(time.Millisecond)
	}
	if callRT != 2000 {
		t.Errorf("a 190 kB/s call was real-time for %d of 2000 packets", callRT)
	}
	if bulkRT > 500 {
		t.Errorf("a 6 MB/s UDP download was real-time for %d packets", bulkRT)
	}
}

func TestRTTMinAndSmoothing(t *testing.T) {
	var r RTT
	now := t0
	for i := 0; i < 20; i++ {
		r.Update(20*time.Millisecond, now)
		now = now.Add(100 * time.Millisecond)
	}
	r.Update(200*time.Millisecond, now)
	if r.Min(now) != 20*time.Millisecond {
		t.Fatalf("min %v", r.Min(now))
	}
	if r.SRTT < 30*time.Millisecond || r.SRTT > 50*time.Millisecond {
		t.Fatalf("srtt %v after one spike", r.SRTT)
	}
	// The minimum expires after the window.
	later := now.Add(MinWindow + time.Second)
	r.Update(80*time.Millisecond, later)
	if r.Min(later) != 80*time.Millisecond {
		t.Fatalf("min %v did not expire", r.Min(later))
	}
}
