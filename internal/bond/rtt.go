// Package bond is the part of weaver that decides things: how good each path
// is, which path or paths a packet goes on, which packets are real-time, and
// how the receiver puts packets back in order. It does no I/O, takes the time
// as an argument everywhere, and is not safe for concurrent use; the engine
// holds a session lock around it.
package bond

import "time"

// RTT is a smoothed round-trip estimator (RFC 6298 gains) with a windowed
// minimum, which the scheduler uses to tell queueing from distance.
type RTT struct {
	SRTT   time.Duration
	RTTVar time.Duration
	n      int

	// minimum over the last MinWindow, kept as three rolling buckets
	buckets [3]minBucket
}

type minBucket struct {
	start time.Time
	min   time.Duration
}

// MinWindow is how long a minimum RTT sample is remembered.
const MinWindow = 30 * time.Second

// Update folds in one sample taken at now.
func (r *RTT) Update(sample time.Duration, now time.Time) {
	if sample <= 0 {
		sample = time.Microsecond
	}
	if r.n == 0 {
		r.SRTT = sample
		r.RTTVar = sample / 2
	} else {
		d := r.SRTT - sample
		if d < 0 {
			d = -d
		}
		r.RTTVar = (3*r.RTTVar + d) / 4
		r.SRTT = (7*r.SRTT + sample) / 8
	}
	r.n++

	span := MinWindow / time.Duration(len(r.buckets))
	b := &r.buckets[len(r.buckets)-1]
	if b.start.IsZero() || now.Sub(b.start) >= span {
		copy(r.buckets[:], r.buckets[1:])
		r.buckets[len(r.buckets)-1] = minBucket{start: now, min: sample}
		return
	}
	if sample < b.min {
		b.min = sample
	}
}

// Valid reports whether any sample has been taken.
func (r *RTT) Valid() bool { return r.n > 0 }

// Min returns the lowest sample in the last MinWindow, or SRTT if none.
func (r *RTT) Min(now time.Time) time.Duration {
	var m time.Duration
	for _, b := range r.buckets {
		if b.start.IsZero() || now.Sub(b.start) > MinWindow {
			continue
		}
		if m == 0 || b.min < m {
			m = b.min
		}
	}
	if m == 0 {
		return r.SRTT
	}
	return m
}

// Seed sets the estimate from a value the other end measured, without
// counting it as a local sample. The server uses it: only the client probes.
func (r *RTT) Seed(srtt, rttvar time.Duration, now time.Time) {
	if srtt <= 0 {
		return
	}
	r.SRTT, r.RTTVar = srtt, rttvar
	if r.n == 0 {
		r.n = 1
	}
	b := &r.buckets[len(r.buckets)-1]
	span := MinWindow / time.Duration(len(r.buckets))
	if b.start.IsZero() || now.Sub(b.start) >= span {
		copy(r.buckets[:], r.buckets[1:])
		r.buckets[len(r.buckets)-1] = minBucket{start: now, min: srtt}
	} else if srtt < b.min {
		b.min = srtt
	}
}
