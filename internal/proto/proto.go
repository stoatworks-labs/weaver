// Package proto holds weaver's wire format: the outer packet headers that travel
// in the clear and the inner frames that travel inside the AEAD. It does no I/O
// and no crypto. docs/DESIGN.md describes the same layout in prose; this file is
// the authority.
package proto

import (
	"encoding/binary"
	"errors"
)

// Outer packet types.
const (
	TypeHandshakeInit = 1
	TypeHandshakeResp = 2
	TypeData          = 3
)

// Header lengths.
const (
	InitHeaderLen = 8  // type, pathID, 2 reserved, senderIndex
	RespHeaderLen = 12 // type, pathID, 2 reserved, senderIndex, receiverIndex
	DataHeaderLen = 16 // type, pathID, 2 reserved, receiverIndex, nonce
	AEADOverhead  = 16
)

// MaxCounter is the largest per-path counter: the top byte of a nonce is the path.
const MaxCounter = 1<<56 - 1

// Nonce builds the AEAD nonce for a counter on a path.
func Nonce(pathID uint8, counter uint64) uint64 {
	return uint64(pathID)<<56 | counter&MaxCounter
}

// SplitNonce is the inverse of Nonce.
func SplitNonce(n uint64) (pathID uint8, counter uint64) {
	return uint8(n >> 56), n & MaxCounter
}

// PutInitHeader writes a HandshakeInit header into b[:InitHeaderLen].
func PutInitHeader(b []byte, pathID uint8, sender uint32) {
	b[0], b[1], b[2], b[3] = TypeHandshakeInit, pathID, 0, 0
	binary.BigEndian.PutUint32(b[4:8], sender)
}

// PutRespHeader writes a HandshakeResp header into b[:RespHeaderLen].
func PutRespHeader(b []byte, pathID uint8, sender, receiver uint32) {
	b[0], b[1], b[2], b[3] = TypeHandshakeResp, pathID, 0, 0
	binary.BigEndian.PutUint32(b[4:8], sender)
	binary.BigEndian.PutUint32(b[8:12], receiver)
}

// PutDataHeader writes a Data header into b[:DataHeaderLen].
func PutDataHeader(b []byte, pathID uint8, receiver uint32, nonce uint64) {
	b[0], b[1], b[2], b[3] = TypeData, pathID, 0, 0
	binary.BigEndian.PutUint32(b[4:8], receiver)
	binary.BigEndian.PutUint64(b[8:16], nonce)
}

// DataHeader is a parsed Data header.
type DataHeader struct {
	PathID   uint8
	Receiver uint32
	Nonce    uint64
}

// ErrShort is returned for a packet too short for its type.
var ErrShort = errors.New("proto: packet too short")

// ErrBadNonce is returned when a data packet's nonce names a different path
// from its header.
var ErrBadNonce = errors.New("proto: nonce path does not match header")

// ParseDataHeader reads a Data header. The caller has already checked b[0].
func ParseDataHeader(b []byte) (DataHeader, error) {
	if len(b) < DataHeaderLen+AEADOverhead {
		return DataHeader{}, ErrShort
	}
	h := DataHeader{
		PathID:   b[1],
		Receiver: binary.BigEndian.Uint32(b[4:8]),
		Nonce:    binary.BigEndian.Uint64(b[8:16]),
	}
	if p, _ := SplitNonce(h.Nonce); p != h.PathID {
		return DataHeader{}, ErrBadNonce
	}
	return h, nil
}

// ParseInitHeader reads a HandshakeInit header.
func ParseInitHeader(b []byte) (pathID uint8, sender uint32, msg []byte, err error) {
	if len(b) <= InitHeaderLen {
		return 0, 0, nil, ErrShort
	}
	return b[1], binary.BigEndian.Uint32(b[4:8]), b[InitHeaderLen:], nil
}

// ParseRespHeader reads a HandshakeResp header.
func ParseRespHeader(b []byte) (pathID uint8, sender, receiver uint32, msg []byte, err error) {
	if len(b) <= RespHeaderLen {
		return 0, 0, 0, nil, ErrShort
	}
	return b[1], binary.BigEndian.Uint32(b[4:8]), binary.BigEndian.Uint32(b[8:12]), b[RespHeaderLen:], nil
}

// Inner frame types.
const (
	FrameData      = 0x01
	FrameProbe     = 0x02
	FrameProbeAck  = 0x03
	FrameReport    = 0x04
	FrameKeepalive = 0x05
	FramePathInfo  = 0x06
)

// FlagImmediate asks the receiver to deliver a DATA frame without waiting for
// earlier sequence numbers.
const FlagImmediate = 0x01

// DataFrameHeaderLen is the DATA frame header: type, flags, seq, and the
// sender's clock in microseconds (wrapping), from which the receiver measures
// each path's queueing delay.
const DataFrameHeaderLen = 10

// PutDataFrameHeader writes a DATA frame header into b[:DataFrameHeaderLen].
func PutDataFrameHeader(b []byte, flags uint8, seq, sentUs uint32) {
	b[0], b[1] = FrameData, flags
	binary.BigEndian.PutUint32(b[2:6], seq)
	binary.BigEndian.PutUint32(b[6:10], sentUs)
}

// ParseDataFrame splits a DATA frame.
func ParseDataFrame(f []byte) (flags uint8, seq, sentUs uint32, pkt []byte, err error) {
	if len(f) < DataFrameHeaderLen {
		return 0, 0, 0, nil, ErrShort
	}
	return f[1], binary.BigEndian.Uint32(f[2:6]), binary.BigEndian.Uint32(f[6:10]), f[DataFrameHeaderLen:], nil
}

// Probe is a PROBE or PROBEACK frame.
type Probe struct {
	ID     uint32
	SentAt uint64 // sender's monotonic clock, ns
	HoldUs uint32 // PROBEACK only: how long the answer was held
}

// AppendProbe appends a PROBE frame.
func AppendProbe(b []byte, p Probe) []byte {
	b = append(b, FrameProbe)
	b = binary.BigEndian.AppendUint32(b, p.ID)
	return binary.BigEndian.AppendUint64(b, p.SentAt)
}

// AppendProbeAck appends a PROBEACK frame.
func AppendProbeAck(b []byte, p Probe) []byte {
	b = append(b, FrameProbeAck)
	b = binary.BigEndian.AppendUint32(b, p.ID)
	b = binary.BigEndian.AppendUint64(b, p.SentAt)
	return binary.BigEndian.AppendUint32(b, p.HoldUs)
}

// ParseProbe reads a PROBE or PROBEACK frame (f[0] is the type).
func ParseProbe(f []byte) (Probe, error) {
	switch {
	case len(f) >= 17 && f[0] == FrameProbeAck:
		return Probe{
			ID:     binary.BigEndian.Uint32(f[1:5]),
			SentAt: binary.BigEndian.Uint64(f[5:13]),
			HoldUs: binary.BigEndian.Uint32(f[13:17]),
		}, nil
	case len(f) >= 13 && f[0] == FrameProbe:
		return Probe{
			ID:     binary.BigEndian.Uint32(f[1:5]),
			SentAt: binary.BigEndian.Uint64(f[5:13]),
		}, nil
	}
	return Probe{}, ErrShort
}

// Path states as carried in a report.
const (
	StateDown    = 0
	StateSuspect = 1
	StateUp      = 2
)

// PathReport is what one end received on one path, cumulatively.
type PathReport struct {
	PathID         uint8
	State          uint8
	RxPackets      uint32
	RxBytes        uint64
	HighestCounter uint64
	SRTTUs         uint32
	RTTVarUs       uint32
	// The queueing delay of DATA frames on the path, above the lowest delay
	// seen lately: smoothed at the time of the report, and the lowest in the
	// interval since the last one (0 if too few frames arrived to tell).
	QueueUs    uint32
	QueueMinUs uint32
}

const pathReportLen = 1 + 1 + 4 + 8 + 8 + 4 + 4 + 4 + 4

// Report is a REPORT frame.
type Report struct {
	At    uint64 // reporter's monotonic clock, ns
	Paths []PathReport
}

// AppendReport appends a REPORT frame.
func AppendReport(b []byte, r Report) []byte {
	b = append(b, FrameReport)
	b = binary.BigEndian.AppendUint64(b, r.At)
	b = append(b, uint8(len(r.Paths)))
	for _, p := range r.Paths {
		b = append(b, p.PathID, p.State)
		b = binary.BigEndian.AppendUint32(b, p.RxPackets)
		b = binary.BigEndian.AppendUint64(b, p.RxBytes)
		b = binary.BigEndian.AppendUint64(b, p.HighestCounter)
		b = binary.BigEndian.AppendUint32(b, p.SRTTUs)
		b = binary.BigEndian.AppendUint32(b, p.RTTVarUs)
		b = binary.BigEndian.AppendUint32(b, p.QueueUs)
		b = binary.BigEndian.AppendUint32(b, p.QueueMinUs)
	}
	return b
}

// ParseReport reads a REPORT frame.
func ParseReport(f []byte) (Report, error) {
	if len(f) < 10 || f[0] != FrameReport {
		return Report{}, ErrShort
	}
	r := Report{At: binary.BigEndian.Uint64(f[1:9])}
	n := int(f[9])
	f = f[10:]
	if len(f) < n*pathReportLen {
		return Report{}, ErrShort
	}
	r.Paths = make([]PathReport, n)
	for i := range r.Paths {
		p := f[i*pathReportLen:]
		r.Paths[i] = PathReport{
			PathID:         p[0],
			State:          p[1],
			RxPackets:      binary.BigEndian.Uint32(p[2:6]),
			RxBytes:        binary.BigEndian.Uint64(p[6:14]),
			HighestCounter: binary.BigEndian.Uint64(p[14:22]),
			SRTTUs:         binary.BigEndian.Uint32(p[22:26]),
			RTTVarUs:       binary.BigEndian.Uint32(p[26:30]),
			QueueUs:        binary.BigEndian.Uint32(p[30:34]),
			QueueMinUs:     binary.BigEndian.Uint32(p[34:38]),
		}
	}
	return r, nil
}

// PathInfo describes one client path to the server: its name, priority and
// the caps that apply to the server's sending on it.
type PathInfo struct {
	PathID   uint8
	Priority uint8
	Metered  bool
	UpKbps   uint32
	DownKbps uint32
	Name     string
}

// AppendPathInfo appends a PATHINFO frame listing every current client path.
func AppendPathInfo(b []byte, paths []PathInfo) []byte {
	b = append(b, FramePathInfo, uint8(len(paths)))
	for _, p := range paths {
		var flags uint8
		if p.Metered {
			flags |= 1
		}
		name := p.Name
		if len(name) > 32 {
			name = name[:32]
		}
		b = append(b, p.PathID, p.Priority, flags)
		b = binary.BigEndian.AppendUint32(b, p.UpKbps)
		b = binary.BigEndian.AppendUint32(b, p.DownKbps)
		b = append(b, uint8(len(name)))
		b = append(b, name...)
	}
	return b
}

// ParsePathInfo reads a PATHINFO frame.
func ParsePathInfo(f []byte) ([]PathInfo, error) {
	if len(f) < 2 || f[0] != FramePathInfo {
		return nil, ErrShort
	}
	n := int(f[1])
	f = f[2:]
	out := make([]PathInfo, 0, n)
	for i := 0; i < n; i++ {
		if len(f) < 12 {
			return nil, ErrShort
		}
		p := PathInfo{
			PathID:   f[0],
			Priority: f[1],
			Metered:  f[2]&1 != 0,
			UpKbps:   binary.BigEndian.Uint32(f[3:7]),
			DownKbps: binary.BigEndian.Uint32(f[7:11]),
		}
		l := int(f[11])
		f = f[12:]
		if len(f) < l {
			return nil, ErrShort
		}
		p.Name = string(f[:l])
		f = f[l:]
		out = append(out, p)
	}
	return out, nil
}

// HelloLen is the handshake payload: version and an instance ID.
const HelloLen = 5

// Version is the protocol version carried in both handshake payloads.
const Version = 1

// Hello is the payload inside both handshake messages. Instance is random per
// process start; a change tells the other end to reset its sequence and path
// state, because the sender started over.
type Hello struct {
	Version  uint8
	Instance uint32
}

// AppendHello appends a handshake payload.
func AppendHello(b []byte, h Hello) []byte {
	b = append(b, h.Version)
	return binary.BigEndian.AppendUint32(b, h.Instance)
}

// ParseHello reads a handshake payload.
func ParseHello(b []byte) (Hello, error) {
	if len(b) < HelloLen {
		return Hello{}, ErrShort
	}
	return Hello{Version: b[0], Instance: binary.BigEndian.Uint32(b[1:5])}, nil
}
