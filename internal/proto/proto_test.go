package proto

import (
	"reflect"
	"testing"
)

func TestNonceCarriesPath(t *testing.T) {
	n := Nonce(7, 12345)
	if p, c := SplitNonce(n); p != 7 || c != 12345 {
		t.Fatalf("split %d %d", p, c)
	}
	b := make([]byte, DataHeaderLen+AEADOverhead)
	PutDataHeader(b, 7, 0xdeadbeef, n)
	h, err := ParseDataHeader(b)
	if err != nil || h.PathID != 7 || h.Receiver != 0xdeadbeef || h.Nonce != n {
		t.Fatalf("%+v %v", h, err)
	}
	// A header whose path disagrees with its nonce is rejected.
	b[1] = 8
	if _, err := ParseDataHeader(b); err != ErrBadNonce {
		t.Fatalf("mismatched path accepted: %v", err)
	}
}

func TestFramesRoundTrip(t *testing.T) {
	r := Report{At: 99, Paths: []PathReport{
		{PathID: 0, State: StateUp, RxPackets: 10, RxBytes: 1 << 40, HighestCounter: 1 << 50, SRTTUs: 20000, RTTVarUs: 3000, QueueUs: 4500, QueueMinUs: 1200},
		{PathID: 3, State: StateSuspect},
	}}
	got, err := ParseReport(AppendReport(nil, r))
	if err != nil || !reflect.DeepEqual(got, r) {
		t.Fatalf("report: %+v %v", got, err)
	}

	pi := []PathInfo{{PathID: 1, Priority: 2, Metered: true, UpKbps: 5000, DownKbps: 20000, Name: "modem"}, {PathID: 2, Name: "wan"}}
	gotPI, err := ParsePathInfo(AppendPathInfo(nil, pi))
	if err != nil || !reflect.DeepEqual(gotPI, pi) {
		t.Fatalf("pathinfo: %+v %v", gotPI, err)
	}

	p := Probe{ID: 5, SentAt: 1234567, HoldUs: 77}
	if got, err := ParseProbe(AppendProbeAck(nil, p)); err != nil || got != p {
		t.Fatalf("probeack: %+v %v", got, err)
	}
	p.HoldUs = 0
	if got, err := ParseProbe(AppendProbe(nil, p)); err != nil || got != p {
		t.Fatalf("probe: %+v %v", got, err)
	}

	h := Hello{Version: Version, Instance: 0xabcdef01}
	if got, err := ParseHello(AppendHello(nil, h)); err != nil || got != h {
		t.Fatalf("hello: %+v %v", got, err)
	}

	f := make([]byte, DataFrameHeaderLen+3)
	PutDataFrameHeader(f, FlagImmediate, 42, 0xfffffff0)
	copy(f[DataFrameHeaderLen:], "abc")
	flags, seq, sent, pkt, err := ParseDataFrame(f)
	if err != nil || flags != FlagImmediate || seq != 42 || sent != 0xfffffff0 || string(pkt) != "abc" {
		t.Fatalf("data frame: %d %d %d %q %v", flags, seq, sent, pkt, err)
	}
}

func TestTruncatedFramesError(t *testing.T) {
	full := AppendReport(nil, Report{At: 1, Paths: []PathReport{{PathID: 1}}})
	for i := 0; i < len(full); i++ {
		if _, err := ParseReport(full[:i]); err == nil {
			t.Fatalf("report truncated to %d parsed", i)
		}
	}
	pi := AppendPathInfo(nil, []PathInfo{{PathID: 1, Name: "wan"}})
	for i := 0; i < len(pi); i++ {
		if _, err := ParsePathInfo(pi[:i]); err == nil {
			t.Fatalf("pathinfo truncated to %d parsed", i)
		}
	}
}
