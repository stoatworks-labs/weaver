// Package replay is a sliding-window replay filter for packet counters, in the
// style of RFC 6479. weaver keeps one per path per keypair, so a slow path's
// counters never age a fast path's out of the window.
package replay

// WindowSize is how far behind the highest counter seen a packet may arrive.
const WindowSize = 2048

const (
	blockBits = 64
	blocks    = WindowSize/blockBits + 1 // one spare block, so the window is never short
	ringBits  = blocks * blockBits
)

// Window tracks the counters seen recently. The zero value accepts any first
// counter.
type Window struct {
	top    uint64 // highest counter seen, plus one; 0 means none yet
	bitmap [blocks]uint64
}

// Accept reports whether counter is new and inside the window, and records it
// if so. Call it only after the packet has authenticated.
func (w *Window) Accept(counter uint64) bool {
	c := counter + 1 // so that 0 can mean "nothing seen"
	if c > w.top {
		// Moving forward: clear the blocks the window slides over.
		oldBlock := w.top / blockBits
		newBlock := c / blockBits
		gap := newBlock - oldBlock
		if gap > blocks {
			gap = blocks
		}
		for i := uint64(1); i <= gap; i++ {
			w.bitmap[(oldBlock+i)%blocks] = 0
		}
		w.top = c
	} else if w.top-c >= WindowSize {
		return false // too old
	}
	idx := (c / blockBits) % blocks
	bit := uint64(1) << (c % blockBits)
	if w.bitmap[idx]&bit != 0 {
		return false // seen
	}
	w.bitmap[idx] |= bit
	return true
}

// Highest returns the highest counter accepted and whether there was one.
func (w *Window) Highest() (uint64, bool) {
	if w.top == 0 {
		return 0, false
	}
	return w.top - 1, true
}
