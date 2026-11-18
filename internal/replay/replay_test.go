package replay

import "testing"

func TestWindow(t *testing.T) {
	var w Window
	for _, c := range []uint64{0, 1, 2, 5, 4, 3} {
		if !w.Accept(c) {
			t.Fatalf("fresh counter %d rejected", c)
		}
	}
	for _, c := range []uint64{0, 3, 5} {
		if w.Accept(c) {
			t.Fatalf("replayed counter %d accepted", c)
		}
	}
	if !w.Accept(5 + WindowSize - 1) {
		t.Fatal("a jump within range rejected")
	}
	if w.Accept(5) {
		t.Fatal("replay after a jump accepted")
	}
	if !w.Accept(10) {
		t.Fatal("unseen counter still inside the window rejected")
	}
	if !w.Accept(100000) {
		t.Fatal("big jump rejected")
	}
	if w.Accept(100000 - WindowSize) {
		t.Fatal("counter outside the window accepted")
	}
	if !w.Accept(100000 - WindowSize + 1) {
		t.Fatal("oldest counter inside the window rejected")
	}
	if h, ok := w.Highest(); !ok || h != 100000 {
		t.Fatalf("highest %d %v", h, ok)
	}
}

func TestWindowEveryCounterOnce(t *testing.T) {
	var w Window
	// Deliver 0..9999 in a scrambled but bounded order, each twice.
	const n = 10000
	for base := uint64(0); base < n; base += 100 {
		for i := uint64(99); ; i-- {
			if !w.Accept(base + i) {
				t.Fatalf("first copy of %d rejected", base+i)
			}
			if w.Accept(base + i) {
				t.Fatalf("second copy of %d accepted", base+i)
			}
			if i == 0 {
				break
			}
		}
	}
}
