package bond

import "time"

// OWD measures one path's queueing delay at the receiving end, from the
// sender's clock stamped on each DATA frame.
//
// The two clocks are unrelated, so a single arrival says nothing; but their
// offset is constant (both are monotonic), so the arrival-minus-stamp value
// moves only with the delay the frame met. Its lowest value lately is the
// path with empty queues, and anything above that is queueing.
//
// The sender cannot see this itself: its RTT probes cross both directions,
// are few, and are smoothed. This is per packet, one way, and reaches the
// sender in the next REPORT.
type OWD struct {
	base    [3]owdBucket // lowest raw value, as rolling buckets
	smooth  time.Duration
	haveAny bool

	intervalMin time.Duration
	intervalN   int
}

type owdBucket struct {
	start time.Time
	min   uint32
	set   bool
}

// OWDBaseWindow is how long the lowest delay is remembered. A path whose
// delay rises for good (a new route) reads as queueing until it passes.
const OWDBaseWindow = 30 * time.Second

// owdMinSamples is how many frames an interval needs before its lowest
// queueing delay is reported: a few frames may all have met a burst.
const owdMinSamples = 8

// before reports whether wrapping microsecond value a is below b.
func before(a, b uint32) bool { return int32(a-b) < 0 }

// Sample records one DATA frame stamped sentUs by the sender and received at
// nowUs on this end's clock (both wrapping microseconds), at now.
func (o *OWD) Sample(sentUs, nowUs uint32, now time.Time) {
	raw := nowUs - sentUs

	span := OWDBaseWindow / time.Duration(len(o.base))
	last := &o.base[len(o.base)-1]
	switch {
	case !last.set || now.Sub(last.start) >= span:
		copy(o.base[:], o.base[1:])
		o.base[len(o.base)-1] = owdBucket{start: now, min: raw, set: true}
	case before(raw, last.min):
		last.min = raw
	}

	base := raw
	for _, b := range o.base {
		if b.set && now.Sub(b.start) <= OWDBaseWindow && before(b.min, base) {
			base = b.min
		}
	}
	q := time.Duration(raw-base) * time.Microsecond

	if !o.haveAny {
		o.smooth, o.haveAny = q, true
	} else {
		o.smooth += (q - o.smooth) / 8
	}
	if o.intervalN == 0 || q < o.intervalMin {
		o.intervalMin = q
	}
	o.intervalN++
}

// Report returns the smoothed queueing delay now (0 if no frame arrived
// since the last Report, when it would be stale) and the lowest seen since
// the last Report (0 if fewer than a handful of frames arrived), and starts a
// new interval.
func (o *OWD) Report() (smooth, intervalMin time.Duration) {
	if o.intervalN > 0 {
		smooth = o.smooth
	}
	if o.intervalN >= owdMinSamples {
		intervalMin = o.intervalMin
	}
	o.intervalN, o.intervalMin = 0, 0
	return smooth, intervalMin
}

// Reset forgets everything: the path's delay may have changed.
func (o *OWD) Reset() { *o = OWD{} }
