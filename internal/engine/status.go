package engine

import (
	"math"
	"time"
)

// Status is what GET /status returns; docs/DESIGN.md documents it.
type Status struct {
	Version string    `json:"version"`
	Role    string    `json:"role"`
	Mode    string    `json:"mode"`
	UptimeS int64     `json:"uptime_s"`
	PubKey  string    `json:"public_key"`
	Tun     TunStatus `json:"tun"`
	// SendErrors counts datagrams the kernel refused (a full socket buffer,
	// a WAN that went away).
	SendErrors uint64          `json:"send_errors"`
	Sessions   []SessionStatus `json:"sessions"`
}

// TunStatus describes the tunnel device.
type TunStatus struct {
	Name    string `json:"name"`
	Address string `json:"address"`
	MTU     int    `json:"mtu"`
}

// SessionStatus describes one session.
type SessionStatus struct {
	Peer           string        `json:"peer"`
	PublicKey      string        `json:"public_key"`
	State          string        `json:"state"`
	Endpoint       string        `json:"endpoint,omitempty"`
	EstablishedS   int64         `json:"established_s"`
	LastHandshakeS int64         `json:"last_handshake_s"`
	TxBytes        uint64        `json:"tx_bytes"`
	RxBytes        uint64        `json:"rx_bytes"`
	Reorder        ReorderStatus `json:"reorder"`
	Paths          []PathStatus  `json:"paths"`
}

// ReorderStatus summarises the receiver's reorder buffer.
type ReorderStatus struct {
	Held       int     `json:"held"`
	HoldMs     float64 `json:"hold_ms"`
	Timeouts   uint64  `json:"timeouts"`
	Duplicates uint64  `json:"duplicates"`
	Late       uint64  `json:"late"`
}

// PathStatus describes one path.
type PathStatus struct {
	ID        uint8   `json:"id"`
	Name      string  `json:"name"`
	Device    string  `json:"device,omitempty"`
	Priority  string  `json:"priority"`
	Metered   bool    `json:"metered,omitempty"`
	State     string  `json:"state"`
	Remote    string  `json:"remote,omitempty"`
	RTTMs     float64 `json:"rtt_ms"`
	RTTVarMs  float64 `json:"rttvar_ms"`
	MinRTTMs  float64 `json:"min_rtt_ms"`
	LossPct   float64 `json:"loss_pct"`
	TxKbps    float64 `json:"tx_kbps"`
	RxKbps    float64 `json:"rx_kbps"`
	RateKbps  float64 `json:"rate_kbps"`
	QueueMs   float64 `json:"queue_ms"`
	TxBytes   uint64  `json:"tx_bytes"`
	RxBytes   uint64  `json:"rx_bytes"`
	TxPackets uint64  `json:"tx_packets"`
	RxPackets uint64  `json:"rx_packets"`
	LastRxMs  int64   `json:"last_rx_ms"`
}

func round(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}

func secondsSince(t, now time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return int64(now.Sub(t).Seconds())
}

// Status reports the engine's state now.
func (e *Engine) Status() Status {
	now := time.Now()
	cfg, tm := e.config()
	st := Status{
		Version: e.version,
		Role:    cfg.Role,
		Mode:    tm.mode.String(),
		UptimeS: int64(now.Sub(e.start).Seconds()),
		PubKey:  e.pub.String(),
		Tun:     TunStatus{Name: cfg.Tun.Name, Address: cfg.Tun.Address, MTU: cfg.Tun.MTU},

		SendErrors: e.sendErrors.Load(),
		Sessions:   []SessionStatus{},
	}
	e.mu.RLock()
	sessions := append([]*Session(nil), e.sessions...)
	e.mu.RUnlock()
	for _, s := range sessions {
		st.Sessions = append(st.Sessions, s.status(now))
	}
	return st
}

func (s *Session) status(now time.Time) SessionStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss := SessionStatus{
		Peer:           s.name,
		PublicKey:      s.peerKey.String(),
		State:          s.state,
		EstablishedS:   secondsSince(s.established, now),
		LastHandshakeS: secondsSince(s.lastHandshake, now),
		TxBytes:        s.txBytes,
		RxBytes:        s.rxBytes,
		Reorder: ReorderStatus{
			Held:       s.reorder.Stats.Held,
			HoldMs:     round(ms(s.holdTime()), 1),
			Timeouts:   s.reorder.Stats.Timeouts,
			Duplicates: s.reorder.Stats.Duplicates,
			Late:       s.reorder.Stats.Late,
		},
		Paths: []PathStatus{},
	}
	if s.state != stateUp {
		ss.EstablishedS = 0
	}
	if !s.e.server {
		if a := s.e.server4(); a.IsValid() {
			ss.Endpoint = a.String()
		}
	}
	for _, p := range s.pathList() {
		state := stateName(p.state)
		if s.e.server {
			state = "down"
			if s.usable(p, now) {
				state = "up"
			} else if p.peerStateKnown && p.peerState == 1 {
				state = "suspect"
			}
		}
		ps := PathStatus{
			ID:        p.ID,
			Name:      p.Name,
			Device:    p.Device,
			Priority:  p.Priority.String(),
			Metered:   p.Metered,
			State:     state,
			RTTMs:     round(ms(p.rtt.SRTT), 1),
			RTTVarMs:  round(ms(p.rtt.RTTVar), 1),
			MinRTTMs:  round(ms(p.rtt.Min(now)), 1),
			LossPct:   round(p.loss*100, 2),
			TxKbps:    round(p.txMeter.kbps(now), 1),
			RxKbps:    round(p.rxMeter.kbps(now), 1),
			RateKbps:  round(p.lane.Rate*8/1000, 0),
			QueueMs:   round(float64(p.lastRep.QueueUs)/1000, 1),
			TxBytes:   p.txBytes,
			RxBytes:   p.rxBytes,
			TxPackets: p.txPackets,
			RxPackets: p.rxPackets,
			LastRxMs:  -1,
		}
		if p.remote.IsValid() {
			ps.Remote = p.remote.String()
		}
		if !p.lastRx.IsZero() {
			ps.LastRxMs = now.Sub(p.lastRx).Milliseconds()
		}
		ss.Paths = append(ss.Paths, ps)
	}
	return ss
}
