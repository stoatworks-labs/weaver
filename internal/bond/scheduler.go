package bond

import (
	"fmt"
	"sort"
	"time"
)

// Priority says when a path carries traffic.
type Priority uint8

const (
	// Primary paths are always used.
	Primary Priority = iota
	// Secondary paths take overflow: their delivery estimate carries a penalty.
	Secondary
	// Backup paths carry nothing while a primary or secondary is usable.
	Backup
	// Disabled paths are not used at all.
	Disabled
)

func (p Priority) String() string {
	switch p {
	case Primary:
		return "primary"
	case Secondary:
		return "secondary"
	case Backup:
		return "backup"
	case Disabled:
		return "disabled"
	}
	return fmt.Sprintf("priority(%d)", uint8(p))
}

// ParsePriority reads a priority name; empty means primary.
func ParsePriority(s string) (Priority, error) {
	switch s {
	case "", "primary":
		return Primary, nil
	case "secondary":
		return Secondary, nil
	case "backup":
		return Backup, nil
	case "disabled":
		return Disabled, nil
	}
	return 0, fmt.Errorf("unknown priority %q (want primary, secondary, backup or disabled)", s)
}

// Mode is how a tunnel spreads packets over its paths.
type Mode uint8

const (
	// Adaptive duplicates real-time and control packets and bonds the rest.
	Adaptive Mode = iota
	// Bond sends each packet on the path that will deliver it soonest.
	Bond
	// Redundant sends every packet on every usable path.
	Redundant
	// Failover sends everything on the single best path.
	Failover
)

func (m Mode) String() string {
	switch m {
	case Adaptive:
		return "adaptive"
	case Bond:
		return "bond"
	case Redundant:
		return "redundant"
	case Failover:
		return "failover"
	}
	return fmt.Sprintf("mode(%d)", uint8(m))
}

// ParseMode reads a mode name; empty means adaptive.
func ParseMode(s string) (Mode, error) {
	switch s {
	case "", "adaptive":
		return Adaptive, nil
	case "bond":
		return Bond, nil
	case "redundant":
		return Redundant, nil
	case "failover":
		return Failover, nil
	}
	return 0, fmt.Errorf("unknown mode %q (want adaptive, bond, redundant or failover)", s)
}

// Tuning constants. DESIGN.md explains each.
const (
	SecondaryPenalty  = 50 * time.Millisecond
	DefaultRate       = 10e6 / 8 // bytes/s, before any feedback
	MinRate           = 100e3 / 8
	maxBacklog        = 2 * time.Second
	failbackStable    = 5 * time.Second
	fullQueue         = 5 * time.Millisecond // a standing queue this deep: the path is full
	heavyLoss         = 0.1                  // loss this high without a queue: a policer
	fastGrowth        = 1.1
	slowGrowth        = 1.03
	settleAfterFull   = 2 * time.Second
	saturatedFraction = 0.9
	// PacketOverhead is what a tunnelled packet costs on the wire beyond the
	// inner packet: outer IPv4 and UDP, the data header, the DATA frame
	// header and the AEAD tag.
	PacketOverhead = 20 + 8 + 16 + 10 + 16
)

// Lane is the scheduler's view of one path in one direction.
type Lane struct {
	ID       uint8
	Priority Priority

	// Usable is set by the engine: the path is up (not suspect, not down).
	Usable      bool
	UsableSince time.Time
	// Degraded means suspect: used only when no lane at all is usable.
	Degraded bool

	SRTT   time.Duration
	MinRTT time.Duration // lowest recent RTT: the path's distance without queueing

	// Rate is the send-rate estimate in bytes per second; Cap, if set,
	// bounds it (from up_kbps or down_kbps).
	Rate float64
	Cap  float64

	busyUntil time.Time
	assigned  uint64    // bytes put on the lane since the last feedback
	lastFeed  time.Time // when feedback was last applied
	lastFull  time.Time // when feedback last said the path was full
}

// NewLane returns a lane with the default or hinted starting rate.
func NewLane(id uint8, prio Priority, capBps float64) *Lane {
	l := &Lane{ID: id, Priority: prio, Cap: capBps, Rate: DefaultRate}
	if capBps > 0 {
		l.Rate = capBps
	}
	return l
}

// Backlog is how far ahead of now the lane's virtual queue runs.
func (l *Lane) Backlog(now time.Time) time.Duration {
	if l.busyUntil.After(now) {
		return l.busyUntil.Sub(now)
	}
	return 0
}

func (l *Lane) penalty() time.Duration {
	if l.Priority == Secondary {
		return SecondaryPenalty
	}
	return 0
}

func (l *Lane) txTime(size int) time.Duration {
	r := l.Rate
	if r < MinRate {
		r = MinRate
	}
	return time.Duration(float64(size+PacketOverhead) / r * float64(time.Second))
}

// eta is when a packet of size bytes put on the lane now would arrive: after
// the lane's virtual queue drains, plus the path's one-way distance.
//
// The virtual queue is what this end has put on the lane at its estimated
// rate, raised to the queue the far end last measured. With a fast path and a
// slow one, the fast path fills until its queue is as deep as the difference
// in their distances, then both carry traffic and packets arrive in step.
func (l *Lane) eta(now time.Time, size int) time.Time {
	owd := l.SRTT / 2
	if l.MinRTT > 0 && l.MinRTT <= l.SRTT {
		owd = l.MinRTT / 2
	}
	start := now
	if l.busyUntil.After(start) {
		start = l.busyUntil
	}
	return start.Add(l.txTime(size) + owd + l.penalty())
}

func (l *Lane) assign(now time.Time, size int) {
	start := now
	if l.busyUntil.After(now) {
		start = l.busyUntil
	}
	l.busyUntil = start.Add(l.txTime(size))
	if l.busyUntil.Sub(now) > maxBacklog {
		l.busyUntil = now.Add(maxBacklog)
	}
	l.assigned += uint64(size + PacketOverhead)
}

// Feedback is what the far end reported about one interval on a lane.
type Feedback struct {
	Delivered float64       // bytes/s the far end received, on its clock
	Loss      float64       // fraction 0..1, smoothed
	Queue     time.Duration // queueing delay the far end measured at the report
	QueueMin  time.Duration // lowest queueing delay in the interval; 0 if unknown
	SRTT      time.Duration // current smoothed RTT
	MinRTT    time.Duration // lowest RTT in the last 30 s
}

// OnFeedback adjusts the lane from a report.
//
// A path whose queue never emptied during the interval was busy throughout,
// so what it delivered is its capacity: the rate moves there. A path pushed
// to its estimate without a standing queue has room, so the rate grows, more
// gently for a while after it was last found full. Loss alone is not a sign
// of a full path, since radio links lose packets at any rate, unless it is
// heavy, as a policer's is.
func (l *Lane) OnFeedback(now time.Time, f Feedback) {
	if f.Queue > 0 {
		q := f.Queue
		if q > maxBacklog {
			q = maxBacklog
		}
		if t := now.Add(q); t.After(l.busyUntil) {
			l.busyUntil = t
		}
	}

	interval := now.Sub(l.lastFeed)
	sent := l.assigned
	l.assigned = 0
	l.lastFeed = now
	if interval <= 0 || interval > 5*time.Second {
		return
	}
	sentRate := float64(sent) / interval.Seconds()

	full := f.QueueMin > fullQueue || f.Loss > heavyLoss
	pushing := sent > 0 && sentRate >= saturatedFraction*l.Rate
	// Only a lane we were feeding tells us about its capacity. Once the load
	// stops, a draining queue or a lingering loss average must not drag the
	// estimate down to what idle probes deliver.
	loaded := sent > 0 && sentRate >= 0.8*f.Delivered

	switch {
	case full && (pushing || loaded):
		l.lastFull = now
		if f.Delivered > 0 {
			l.Rate = 0.25*l.Rate + 0.75*f.Delivered
		} else {
			l.Rate *= 0.5
		}
	case !full && pushing:
		g := fastGrowth
		if !l.lastFull.IsZero() && now.Sub(l.lastFull) < settleAfterFull {
			g = slowGrowth
		}
		up := l.Rate * g
		if d := f.Delivered * g; d > up {
			up = d
		}
		l.Rate = up
	}
	if l.Rate < MinRate {
		l.Rate = MinRate
	}
	if l.Cap > 0 && l.Rate > l.Cap {
		l.Rate = l.Cap
	}
}

// Scheduler picks the path or paths for each packet.
type Scheduler struct {
	Mode  Mode
	Lanes []*Lane

	// Redundancy is how many paths a duplicated packet goes on (default 2);
	// Redundant mode uses every eligible path regardless.
	Redundancy int

	failoverCurrent *Lane
}

// Lane returns the lane with an ID, or nil.
func (s *Scheduler) Lane(id uint8) *Lane {
	for _, l := range s.Lanes {
		if l.ID == id {
			return l
		}
	}
	return nil
}

// AddLane adds or replaces the lane with l's ID.
func (s *Scheduler) AddLane(l *Lane) {
	for i, old := range s.Lanes {
		if old.ID == l.ID {
			s.Lanes[i] = l
			return
		}
	}
	s.Lanes = append(s.Lanes, l)
}

// RemoveLane drops a lane.
func (s *Scheduler) RemoveLane(id uint8) {
	for i, l := range s.Lanes {
		if l.ID == id {
			s.Lanes = append(s.Lanes[:i], s.Lanes[i+1:]...)
			if s.failoverCurrent == l {
				s.failoverCurrent = nil
			}
			return
		}
	}
}

// eligible appends the lanes allowed to carry traffic now: usable primaries and
// secondaries, or, if there are none, usable backups.
func (s *Scheduler) eligible(buf []*Lane) []*Lane {
	buf = buf[:0]
	for _, l := range s.Lanes {
		if l.Usable && (l.Priority == Primary || l.Priority == Secondary) {
			buf = append(buf, l)
		}
	}
	if len(buf) > 0 {
		return buf
	}
	for _, l := range s.Lanes {
		if l.Usable && l.Priority == Backup {
			buf = append(buf, l)
		}
	}
	if len(buf) > 0 {
		return buf
	}
	// Nothing is up. A suspect lane may still get packets through, which
	// beats dropping them all.
	for _, l := range s.Lanes {
		if l.Degraded && l.Priority != Disabled {
			buf = append(buf, l)
		}
	}
	return buf
}

// Pick chooses the lanes for one packet of size bytes and appends their IDs
// to out. dup asks for the packet to be duplicated (adaptive mode's real-time
// and control classes). It returns out unchanged if no lane is usable.
func (s *Scheduler) Pick(now time.Time, size int, dup bool, out []uint8) []uint8 {
	var stack [8]*Lane
	el := s.eligible(stack[:0])
	if len(el) == 0 {
		return out
	}

	switch s.Mode {
	case Failover:
		l := s.failoverPick(now, el)
		l.assign(now, size)
		return append(out, l.ID)

	case Redundant:
		for _, l := range el {
			l.assign(now, size)
			out = append(out, l.ID)
		}
		return out
	}

	if dup && s.Mode == Adaptive && len(el) > 1 {
		n := s.Redundancy
		if n <= 0 {
			n = 2
		}
		if n > len(el) {
			n = len(el)
		}
		sort.Slice(el, func(i, j int) bool { return el[i].eta(now, size).Before(el[j].eta(now, size)) })
		for _, l := range el[:n] {
			l.assign(now, size)
			out = append(out, l.ID)
		}
		return out
	}

	best := el[0]
	bestETA := best.eta(now, size)
	for _, l := range el[1:] {
		if e := l.eta(now, size); e.Before(bestETA) {
			best, bestETA = l, e
		}
	}
	best.assign(now, size)
	return append(out, best.ID)
}

// failoverPick keeps the current lane while it is usable, and moves back to a
// better-priority lane once that has been usable for failbackStable.
func (s *Scheduler) failoverPick(now time.Time, el []*Lane) *Lane {
	better := func(a, b *Lane) bool {
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		return a.SRTT < b.SRTT
	}
	best := el[0]
	for _, l := range el[1:] {
		if better(l, best) {
			best = l
		}
	}
	cur := s.failoverCurrent
	inEl := false
	for _, l := range el {
		if l == cur {
			inEl = true
			break
		}
	}
	switch {
	case cur == nil || !inEl:
		s.failoverCurrent = best
	case best.Priority < cur.Priority && now.Sub(best.UsableSince) >= failbackStable:
		s.failoverCurrent = best
	}
	return s.failoverCurrent
}
