// Package config is weaver's configuration: the JSON file the daemon reads
// (written by the OpenWrt init script from UCI), the WEAVER_* environment the
// container uses instead, defaults, and validation. docs/DESIGN.md documents
// every field.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"reflect"
	"strconv"
	"strings"

	"github.com/stoatworks-labs/weaver/internal/bond"
	"github.com/stoatworks-labs/weaver/internal/handshake"
)

// Roles.
const (
	RoleClient = "client"
	RoleServer = "server"
)

// DefaultPort is weaver's UDP port ("weav" on a phone keypad, truncated).
const DefaultPort = 9328

// Config is the whole daemon configuration.
type Config struct {
	Role            string   `json:"role"`
	PrivateKey      string   `json:"private_key"`
	ListenPort      int      `json:"listen_port,omitempty"`
	Server          string   `json:"server,omitempty"`
	ServerPublicKey string   `json:"server_public_key,omitempty"`
	Tun             Tun      `json:"tun"`
	Mode            string   `json:"mode,omitempty"`
	AutoPaths       *bool    `json:"auto_paths,omitempty"`
	ExcludeDevices  []string `json:"exclude_devices,omitempty"`
	Paths           []Path   `json:"paths,omitempty"`
	Peers           []Peer   `json:"peers,omitempty"`
	Routing         Routing  `json:"routing"`

	ProbeIntervalMs     int `json:"probe_interval_ms,omitempty"`
	IdleProbeIntervalMs int `json:"idle_probe_interval_ms,omitempty"`
	PathTimeoutMs       int `json:"path_timeout_ms,omitempty"`
	ReorderTimeoutMs    int `json:"reorder_timeout_ms,omitempty"`
	RealtimeKbps        int `json:"realtime_kbps,omitempty"`
	RekeyIntervalS      int `json:"rekey_interval_s,omitempty"`

	ControlSocket string `json:"control_socket,omitempty"`
	LogLevel      string `json:"log_level,omitempty"`
}

// Tun is the tunnel device.
type Tun struct {
	Name    string `json:"name,omitempty"`
	MTU     int    `json:"mtu,omitempty"`
	Address string `json:"address,omitempty"`
}

// Path is one client WAN.
type Path struct {
	Name     string `json:"name"`
	Device   string `json:"device"`
	Priority string `json:"priority,omitempty"`
	UpKbps   int    `json:"up_kbps,omitempty"`
	DownKbps int    `json:"down_kbps,omitempty"`
	Metered  bool   `json:"metered,omitempty"`
}

// Peer is one client, as a server knows it.
type Peer struct {
	Name       string   `json:"name"`
	PublicKey  string   `json:"public_key"`
	Address    string   `json:"address"`
	AllowedIPs []string `json:"allowed_ips,omitempty"`
}

// Routing is what the daemon does to the host's routes.
type Routing struct {
	DefaultRoute   *bool    `json:"default_route,omitempty"`
	Subnets        []string `json:"subnets,omitempty"`
	FallbackDirect *bool    `json:"fallback_direct,omitempty"`
	PolicyRouting  *bool    `json:"policy_routing,omitempty"`
	FwmarkBase     uint32   `json:"fwmark_base,omitempty"`
	TableBase      int      `json:"table_base,omitempty"`
	RulePriority   int      `json:"rule_priority,omitempty"`
}

func boolp(b bool) *bool { return &b }

// On reads an optional bool.
func On(b *bool) bool { return b != nil && *b }

// Load reads, defaults and validates a JSON config file.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c := &Config{}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.Finish(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// Finish fills defaults and validates.
func (c *Config) Finish() error {
	c.Role = strings.ToLower(strings.TrimSpace(c.Role))
	if c.Role != RoleClient && c.Role != RoleServer {
		return fmt.Errorf("role must be client or server, not %q", c.Role)
	}
	if c.ListenPort == 0 {
		c.ListenPort = DefaultPort
	}
	if c.Tun.Name == "" {
		c.Tun.Name = "weaver0"
	}
	if c.Tun.MTU == 0 {
		c.Tun.MTU = 1380
	}
	if c.Tun.Address == "" {
		if c.Role == RoleServer {
			c.Tun.Address = "10.77.0.1/24"
		} else {
			return errors.New("tun.address is required for a client")
		}
	}
	if c.Mode == "" {
		c.Mode = "adaptive"
	}
	if c.AutoPaths == nil {
		c.AutoPaths = boolp(true)
	}
	if c.Routing.DefaultRoute == nil {
		c.Routing.DefaultRoute = boolp(true)
	}
	if c.Routing.FallbackDirect == nil {
		c.Routing.FallbackDirect = boolp(true)
	}
	if c.Routing.PolicyRouting == nil {
		c.Routing.PolicyRouting = boolp(true)
	}
	if c.Routing.FwmarkBase == 0 {
		c.Routing.FwmarkBase = 0x57450000
	}
	if c.Routing.TableBase == 0 {
		c.Routing.TableBase = 5745
	}
	if c.Routing.RulePriority == 0 {
		c.Routing.RulePriority = 5745
	}
	def := func(v *int, d int) {
		if *v <= 0 {
			*v = d
		}
	}
	def(&c.ProbeIntervalMs, 200)
	def(&c.IdleProbeIntervalMs, 1000)
	def(&c.PathTimeoutMs, 1000)
	def(&c.RealtimeKbps, 2000)
	def(&c.RekeyIntervalS, 300)
	if c.ControlSocket == "" {
		c.ControlSocket = "/var/run/weaver.sock"
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}

	if _, err := handshake.ParseKey(c.PrivateKey); err != nil {
		return fmt.Errorf("private_key: %w", err)
	}
	if _, err := netip.ParsePrefix(c.Tun.Address); err != nil {
		return fmt.Errorf("tun.address: %w", err)
	}
	if c.Tun.MTU < 576 || c.Tun.MTU > 9000 {
		return fmt.Errorf("tun.mtu %d is out of range", c.Tun.MTU)
	}
	if _, err := bond.ParseMode(c.Mode); err != nil {
		return err
	}
	switch c.Role {
	case RoleClient:
		if c.Server == "" {
			return errors.New("server is required for a client")
		}
		if _, _, err := splitHostPort(c.Server); err != nil {
			return fmt.Errorf("server: %w", err)
		}
		if _, err := handshake.ParseKey(c.ServerPublicKey); err != nil {
			return fmt.Errorf("server_public_key: %w", err)
		}
		seen := map[string]bool{}
		for i, p := range c.Paths {
			if p.Device == "" {
				return fmt.Errorf("paths[%d]: device is required", i)
			}
			if p.Name == "" {
				c.Paths[i].Name = p.Device
			}
			if seen[c.Paths[i].Name] {
				return fmt.Errorf("paths[%d]: duplicate name %q", i, c.Paths[i].Name)
			}
			seen[c.Paths[i].Name] = true
			if _, err := bond.ParsePriority(p.Priority); err != nil {
				return fmt.Errorf("paths[%d]: %w", i, err)
			}
		}
		if len(c.Paths) == 0 && !On(c.AutoPaths) {
			return errors.New("no paths and auto_paths is off")
		}
		for _, s := range c.Routing.Subnets {
			if _, err := netip.ParsePrefix(s); err != nil {
				return fmt.Errorf("routing.subnets: %w", err)
			}
		}
	case RoleServer:
		names := map[string]bool{}
		for i, p := range c.Peers {
			if p.Name == "" {
				return fmt.Errorf("peers[%d]: name is required", i)
			}
			if names[p.Name] {
				return fmt.Errorf("peers[%d]: duplicate name %q", i, p.Name)
			}
			names[p.Name] = true
			if _, err := handshake.ParseKey(p.PublicKey); err != nil {
				return fmt.Errorf("peers[%d] (%s): public_key: %w", i, p.Name, err)
			}
			if _, err := netip.ParseAddr(p.Address); err != nil {
				return fmt.Errorf("peers[%d] (%s): address: %w", i, p.Name, err)
			}
			for _, a := range p.AllowedIPs {
				if _, err := netip.ParsePrefix(a); err != nil {
					return fmt.Errorf("peers[%d] (%s): allowed_ips: %w", i, p.Name, err)
				}
			}
		}
	}
	return nil
}

func splitHostPort(s string) (string, int, error) {
	i := strings.LastIndexByte(s, ':')
	if i <= 0 {
		return "", 0, fmt.Errorf("%q is not host:port", s)
	}
	host := strings.Trim(s[:i], "[]")
	port, err := strconv.Atoi(s[i+1:])
	if err != nil || port <= 0 || port > 65535 {
		return "", 0, fmt.Errorf("%q has no valid port", s)
	}
	return host, port, nil
}

// NeedsRestart reports whether moving from old to new needs a fresh process
// rather than a SIGHUP reload: anything about identity, role, the TUN device,
// the socket, or the server's peer list.
func NeedsRestart(old, new *Config) bool {
	return old.Role != new.Role ||
		old.PrivateKey != new.PrivateKey ||
		old.ListenPort != new.ListenPort ||
		old.Server != new.Server ||
		old.ServerPublicKey != new.ServerPublicKey ||
		old.Tun != new.Tun ||
		old.ControlSocket != new.ControlSocket ||
		!reflect.DeepEqual(old.Peers, new.Peers) ||
		!reflect.DeepEqual(old.Routing, new.Routing)
}

// FromEnv builds a config from WEAVER_* variables, for the container. The
// private key comes from WEAVER_PRIVATE_KEY, or from WEAVER_PRIVATE_KEY_FILE,
// which is generated if it doesn't exist; generated reports whether it was.
func FromEnv(getenv func(string) string) (c *Config, generated bool, err error) {
	c = &Config{
		Role:            getenv("WEAVER_ROLE"),
		Server:          getenv("WEAVER_SERVER"),
		ServerPublicKey: getenv("WEAVER_SERVER_PUBLIC_KEY"),
		Mode:            getenv("WEAVER_MODE"),
		LogLevel:        getenv("WEAVER_LOG_LEVEL"),
		ControlSocket:   getenv("WEAVER_CONTROL_SOCKET"),
	}
	c.Tun.Address = getenv("WEAVER_ADDRESS")
	c.Tun.Name = getenv("WEAVER_TUN")
	if v := getenv("WEAVER_LISTEN_PORT"); v != "" {
		if c.ListenPort, err = strconv.Atoi(v); err != nil {
			return nil, false, fmt.Errorf("WEAVER_LISTEN_PORT: %w", err)
		}
	}
	if v := getenv("WEAVER_MTU"); v != "" {
		if c.Tun.MTU, err = strconv.Atoi(v); err != nil {
			return nil, false, fmt.Errorf("WEAVER_MTU: %w", err)
		}
	}

	c.PrivateKey = strings.TrimSpace(getenv("WEAVER_PRIVATE_KEY"))
	if c.PrivateKey == "" {
		file := getenv("WEAVER_PRIVATE_KEY_FILE")
		if file == "" {
			return nil, false, errors.New("set WEAVER_PRIVATE_KEY or WEAVER_PRIVATE_KEY_FILE")
		}
		b, err := os.ReadFile(file)
		switch {
		case err == nil:
			c.PrivateKey = strings.TrimSpace(string(b))
		case errors.Is(err, os.ErrNotExist):
			k, err := handshake.GenerateKey()
			if err != nil {
				return nil, false, err
			}
			if err := os.WriteFile(file, []byte(k.String()+"\n"), 0o600); err != nil {
				return nil, false, fmt.Errorf("writing %s: %w", file, err)
			}
			c.PrivateKey, generated = k.String(), true
		default:
			return nil, false, err
		}
	}

	// From here on a key may have just been written: say so even on an
	// error, so its public half can be logged and a bad WEAVER_PEERS fixed
	// with it.
	peers, err := ParsePeers(getenv("WEAVER_PEERS"))
	if err != nil {
		return nil, generated, fmt.Errorf("WEAVER_PEERS: %w", err)
	}
	c.Peers = peers
	if err := c.Finish(); err != nil {
		return nil, generated, err
	}
	return c, generated, nil
}

// ParsePeers reads `name,pubkey,address[,allowed_ip...]` entries separated by
// ';' or newlines. Base64 never contains ',' or ';'.
func ParsePeers(s string) ([]Peer, error) {
	var out []Peer
	for _, e := range strings.FieldsFunc(s, func(r rune) bool { return r == ';' || r == '\n' }) {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		f := strings.Split(e, ",")
		for i := range f {
			f[i] = strings.TrimSpace(f[i])
		}
		if len(f) < 3 {
			return nil, fmt.Errorf("%q: want name,public_key,address[,allowed_ip...]", e)
		}
		out = append(out, Peer{Name: f[0], PublicKey: f[1], Address: f[2], AllowedIPs: f[3:]})
	}
	return out, nil
}
