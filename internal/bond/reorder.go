package bond

import "time"

// RingSize is the reorder ring: how many sequence numbers the receiver tracks.
const RingSize = 8192

const ringMask = RingSize - 1

// maxAhead is how far past the next expected sequence a packet may be before
// the receiver gives up on the gap and jumps.
const maxAhead = RingSize / 2

const (
	slotEmpty = iota
	slotHeld
	slotDone
	slotSkipped // given up on; at is when its gap opened
)

type slot struct {
	seq   uint32
	state uint8
	at    time.Time
	pkt   []byte
}

// ReorderStats are counters for the status page.
type ReorderStats struct {
	Delivered  uint64
	Held       int
	Timeouts   uint64 // gaps given up on
	Duplicates uint64
	Late       uint64 // arrived after their gap was skipped, delivered anyway
	Dropped    uint64 // too late to tell from a duplicate
}

// LateBy is the longest a skipped packet actually took to arrive, measured
// from when its gap opened, since the last call. The engine grows the hold to
// cover it, so a path whose packets keep arriving after the hold stops
// costing the inner TCP spurious retransmits.
func (r *Reorder) LateBy() time.Duration {
	d := r.lateBy
	r.lateBy = 0
	return d
}

// Reorder puts a sender's sequence back in order and drops duplicates.
//
// Deliver is called for every packet released, in order; it owns the slice.
// Drop, if set, is called for packets that are discarded, so the caller can
// recycle their buffers.
type Reorder struct {
	Deliver func([]byte)
	Drop    func([]byte)

	ring     [RingSize]slot
	expect   uint32
	started  bool
	held     int
	deadline time.Time // when the current gap is given up on; zero if none
	lateBy   time.Duration

	Stats ReorderStats
}

func (r *Reorder) drop(pkt []byte) {
	if r.Drop != nil {
		r.Drop(pkt)
	}
}

// Reset forgets everything; the next packet starts a new sequence. Held
// packets are dropped.
func (r *Reorder) Reset() {
	for i := range r.ring {
		if r.ring[i].state == slotHeld {
			r.drop(r.ring[i].pkt)
		}
		r.ring[i] = slot{}
	}
	r.started, r.held, r.deadline = false, 0, time.Time{}
}

// Push takes one packet with its sequence number. immediate delivers it at
// once even past a gap (it is still recorded, so a duplicate is dropped). hold
// is how long a gap may wait.
func (r *Reorder) Push(seq uint32, pkt []byte, immediate bool, now time.Time, hold time.Duration) {
	if !r.started {
		r.started = true
		r.expect = seq
	}
	d := int32(seq - r.expect)

	if d < 0 {
		s := &r.ring[seq&ringMask]
		if s.seq == seq && s.state == slotSkipped {
			if wait := now.Sub(s.at); wait > r.lateBy {
				r.lateBy = wait
			}
			*s = slot{seq: seq, state: slotDone}
			r.Stats.Late++
			r.deliver(pkt)
			return
		}
		if s.seq == seq && s.state != slotEmpty {
			r.Stats.Duplicates++
			r.drop(pkt)
			return
		}
		if -d >= RingSize {
			r.Stats.Dropped++
			r.drop(pkt)
			return
		}
		*s = slot{seq: seq, state: slotDone}
		r.Stats.Late++
		r.deliver(pkt)
		return
	}

	if d >= maxAhead {
		// Too far ahead to buffer: release everything up to the window it
		// needs and carry on from there.
		r.skipTo(seq-maxAhead+1, now, hold)
	}

	s := &r.ring[seq&ringMask]
	if s.seq == seq && s.state != slotEmpty {
		r.Stats.Duplicates++
		r.drop(pkt)
		return
	}

	switch {
	case immediate:
		*s = slot{seq: seq, state: slotDone}
		r.deliver(pkt)
		if seq == r.expect {
			r.drain(now, hold)
		}
	case seq == r.expect:
		*s = slot{seq: seq, state: slotDone}
		r.deliver(pkt)
		r.drain(now, hold)
	default:
		*s = slot{seq: seq, state: slotHeld, at: now, pkt: pkt}
		r.held++
		r.Stats.Held = r.held
		if r.deadline.IsZero() {
			r.deadline = now.Add(hold)
		}
	}
}

func (r *Reorder) deliver(pkt []byte) {
	r.Stats.Delivered++
	r.Deliver(pkt)
}

// drain advances expect past every consecutive slot already received,
// delivering held ones, then re-arms the deadline for the next gap.
func (r *Reorder) drain(now time.Time, hold time.Duration) {
	for {
		s := &r.ring[r.expect&ringMask]
		if s.seq != r.expect || s.state == slotEmpty {
			break
		}
		if s.state == slotHeld {
			pkt := s.pkt
			s.pkt, s.state = nil, slotDone
			r.held--
			r.deliver(pkt)
		}
		r.expect++
	}
	r.Stats.Held = r.held
	r.rearm(hold)
}

// rearm sets the deadline from the oldest held packet, or clears it.
func (r *Reorder) rearm(hold time.Duration) {
	if r.held == 0 {
		r.deadline = time.Time{}
		return
	}
	var oldest time.Time
	seen := 0
	for i := uint32(0); i < maxAhead && seen < r.held; i++ {
		s := &r.ring[(r.expect+i)&ringMask]
		if s.state == slotHeld && s.seq == r.expect+i {
			seen++
			if oldest.IsZero() || s.at.Before(oldest) {
				oldest = s.at
			}
		}
	}
	if oldest.IsZero() {
		r.deadline = time.Time{}
		return
	}
	r.deadline = oldest.Add(hold)
}

// skipTo gives up on everything before target: held packets before it are
// delivered in order and the gaps between them are abandoned.
func (r *Reorder) skipTo(target uint32, now time.Time, hold time.Duration) {
	for int32(target-r.expect) > 0 {
		s := &r.ring[r.expect&ringMask]
		if s.seq == r.expect && s.state == slotHeld {
			pkt := s.pkt
			s.pkt, s.state = nil, slotDone
			r.held--
			r.deliver(pkt)
		} else if s.seq != r.expect || s.state == slotEmpty {
			r.Stats.Timeouts++
			*s = slot{seq: r.expect, state: slotSkipped, at: now.Add(-hold)}
		}
		r.expect++
	}
	r.drain(now, hold)
}

// Deadline is when Tick next has work to do; zero if nothing is held.
func (r *Reorder) Deadline() time.Time { return r.deadline }

// Tick gives up on the current gap if its deadline has passed, and keeps going
// while later gaps have also expired.
func (r *Reorder) Tick(now time.Time, hold time.Duration) {
	for r.held > 0 && !r.deadline.IsZero() && !now.Before(r.deadline) {
		// Skip to the first held packet.
		next := r.expect
		for i := uint32(0); i < maxAhead; i++ {
			s := &r.ring[(r.expect+i)&ringMask]
			if s.state == slotHeld && s.seq == r.expect+i {
				next = r.expect + i
				break
			}
		}
		if next == r.expect {
			// Nothing held where we looked; the count is stale.
			r.held = 0
			r.deadline = time.Time{}
			break
		}
		r.skipTo(next, now, hold)
	}
	r.Stats.Held = r.held
}
