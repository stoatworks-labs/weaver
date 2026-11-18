package bond

import (
	"encoding/binary"
	"hash/maphash"
	"time"
)

// Class is what adaptive mode does with a packet.
type Class uint8

const (
	// Bulk is bonded and delivered in order.
	Bulk Class = iota
	// Control (TCP handshakes, teardown, pure ACKs) is duplicated and
	// delivered in order.
	Control
	// Realtime (calls, DNS, ICMP, low-rate UDP) is duplicated and delivered
	// immediately.
	Realtime
)

func (c Class) String() string {
	switch c {
	case Bulk:
		return "bulk"
	case Control:
		return "control"
	case Realtime:
		return "realtime"
	}
	return "class?"
}

const flowSlots = 4096

type flowSlot struct {
	key     uint64
	window  int64 // unix second
	bytes   uint32
	prevWin uint32
}

// Classifier sorts IP packets into classes. Its flow table is a fixed hash of
// the 5-tuple counting bytes per second; a collision only means a flow is
// misjudged for a second.
type Classifier struct {
	// RealtimeBps is the rate below which a UDP flow counts as real-time,
	// in bytes per second. Zero disables the rate rule.
	RealtimeBps float64

	seed  maphash.Seed
	flows [flowSlots]flowSlot
}

// NewClassifier returns a classifier with the given real-time threshold in
// kbit/s.
func NewClassifier(realtimeKbps int) *Classifier {
	return &Classifier{RealtimeBps: float64(realtimeKbps) * 1000 / 8, seed: maphash.MakeSeed()}
}

func realtimeDSCP(d uint8) bool {
	switch d {
	case 46, 32, 34, 40, 48, 56: // EF, CS4, AF41, CS5, CS6, CS7
		return true
	}
	return false
}

func realtimePort(p uint16) bool {
	switch {
	case p == 53, p == 123:
		return true
	case p >= 3478 && p <= 3479: // STUN/TURN
		return true
	case p >= 5060 && p <= 5061: // SIP
		return true
	case p >= 19302 && p <= 19309: // Google STUN, Meet
		return true
	}
	return false
}

// Classify returns the class of one IP packet.
func (c *Classifier) Classify(pkt []byte, now time.Time) Class {
	if len(pkt) < 1 {
		return Bulk
	}
	var (
		proto    uint8
		l4       []byte
		dscp     uint8
		addrs    []byte
		firstFra = true
	)
	switch pkt[0] >> 4 {
	case 4:
		if len(pkt) < 20 {
			return Bulk
		}
		ihl := int(pkt[0]&0x0f) * 4
		if ihl < 20 || len(pkt) < ihl {
			return Bulk
		}
		dscp = pkt[1] >> 2
		proto = pkt[9]
		addrs = pkt[12:20]
		if binary.BigEndian.Uint16(pkt[6:8])&0x1fff != 0 {
			firstFra = false // a later fragment has no L4 header
		}
		l4 = pkt[ihl:]
	case 6:
		if len(pkt) < 40 {
			return Bulk
		}
		dscp = uint8(binary.BigEndian.Uint16(pkt[0:2])>>6) & 0x3f
		proto = pkt[6]
		addrs = pkt[8:40]
		l4 = pkt[40:]
	default:
		return Bulk
	}
	if realtimeDSCP(dscp) {
		return Realtime
	}
	switch proto {
	case 1, 58: // ICMP, ICMPv6
		return Realtime
	case 6: // TCP
		if !firstFra || len(l4) < 20 {
			return Bulk
		}
		flags := l4[13]
		if flags&0x07 != 0 { // FIN, SYN, RST
			return Control
		}
		dataOff := int(l4[12]>>4) * 4
		if flags&0x10 != 0 && len(l4) <= dataOff { // ACK with no payload
			return Control
		}
		return Bulk
	case 17: // UDP
		if !firstFra || len(l4) < 8 {
			return Bulk
		}
		sp := binary.BigEndian.Uint16(l4[0:2])
		dp := binary.BigEndian.Uint16(l4[2:4])
		if realtimePort(sp) || realtimePort(dp) {
			return Realtime
		}
		if c.RealtimeBps <= 0 {
			return Bulk
		}
		if c.flowRate(addrs, l4[0:4], proto, len(pkt), now) < c.RealtimeBps {
			return Realtime
		}
		return Bulk
	}
	return Bulk
}

// flowRate counts the packet against its flow and returns the flow's rate in
// bytes per second: the larger of the last full second and this one so far.
func (c *Classifier) flowRate(addrs, ports []byte, proto uint8, size int, now time.Time) float64 {
	var h maphash.Hash
	h.SetSeed(c.seed)
	h.Write(addrs)
	h.Write(ports)
	h.WriteByte(proto)
	key := h.Sum64() | 1 // never 0, so an empty slot can't match
	s := &c.flows[key%flowSlots]
	sec := now.Unix()
	switch {
	case s.key != key:
		*s = flowSlot{key: key, window: sec}
	case sec == s.window+1:
		s.prevWin, s.bytes, s.window = s.bytes, 0, sec
	case sec > s.window+1:
		s.prevWin, s.bytes, s.window = 0, 0, sec
	}
	s.bytes += uint32(size)
	cur := float64(s.bytes)
	if p := float64(s.prevWin); p > cur {
		return p
	}
	return cur
}
