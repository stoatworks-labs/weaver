// Command weaver bonds a router's WANs into one tunnel. The same binary is the
// client (on the router with the WANs) and the server (on a VPS or another
// router). See docs/DESIGN.md.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/stoatworks-labs/weaver/internal/config"
	"github.com/stoatworks-labs/weaver/internal/control"
	"github.com/stoatworks-labs/weaver/internal/engine"
	"github.com/stoatworks-labs/weaver/internal/handshake"
	"github.com/stoatworks-labs/weaver/internal/platform"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "0.1.0-dev"

// exitRestart tells a supervisor (procd, Docker) that the config changed in a
// way that needs a fresh process; both restart on any exit.
const exitRestart = 75

const usage = `weaver bonds WAN links into one tunnel.

Usage:
  weaver run [-c config.json]     run the daemon (config from WEAVER_* if no -c)
  weaver genkey                   print a new private key
  weaver pubkey                   read a private key on stdin, print its public key
  weaver status [--json] [--socket PATH]
  weaver version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "run":
		err = cmdRun(os.Args[2:])
	case "genkey":
		err = cmdGenkey()
	case "pubkey":
		err = cmdPubkey(os.Stdin)
	case "status":
		err = cmdStatus(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println(version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "weaver: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	var re restartError
	if errors.As(err, &re) {
		fmt.Fprintln(os.Stderr, "weaver:", err)
		os.Exit(exitRestart)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "weaver:", err)
		os.Exit(1)
	}
}

type restartError struct{ why string }

func (r restartError) Error() string { return "restart needed: " + r.why }

func cmdGenkey() error {
	k, err := handshake.GenerateKey()
	if err != nil {
		return err
	}
	fmt.Println(k.String())
	return nil
}

func cmdPubkey(r io.Reader) error {
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	k, err := handshake.ParseKey(strings.TrimSpace(line))
	if err != nil {
		return err
	}
	fmt.Println(handshake.PublicKey(k).String())
	return nil
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "trace":
		return slog.LevelDebug - 4
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return slog.LevelInfo
}

func loadConfig(path string) (*config.Config, error) {
	if path != "" {
		return config.Load(path)
	}
	if os.Getenv("WEAVER_ROLE") == "" {
		return nil, errors.New("no config: pass -c config.json or set WEAVER_ROLE and friends")
	}
	cfg, generated, err := config.FromEnv(os.Getenv)
	if generated {
		// Logged even when the rest of the environment is wrong: this key
		// is what the clients need, and the file now holds it.
		file := os.Getenv("WEAVER_PRIVATE_KEY_FILE")
		if b, rerr := os.ReadFile(file); rerr == nil {
			if k, perr := handshake.ParseKey(strings.TrimSpace(string(b))); perr == nil {
				fmt.Printf("weaver: generated a private key in %s; public key %s\n", file, handshake.PublicKey(k))
			}
		}
	}
	if err != nil {
		return nil, err
	}
	return cfg, nil
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	path := fs.String("c", "", "config file (JSON)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*path)
	if err != nil {
		return err
	}
	// stdout, not stderr: procd files a service's stderr as daemon.err
	// whatever the line says, and its stdout as daemon.info.
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: parseLevel(cfg.LogLevel)}))

	addr, _ := netip.ParsePrefix(cfg.Tun.Address)
	tun, err := platform.OpenTUN(cfg.Tun.Name, cfg.Tun.MTU, addr)
	if err != nil {
		return err
	}
	eng, err := engine.New(cfg, tun, platform.New(tun.Name()), log, version)
	if err != nil {
		tun.Close()
		return err
	}
	log.Info("weaver "+version, "role", cfg.Role, "tun", tun.Name(), "address", cfg.Tun.Address, "mode", cfg.Mode)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		if err := control.Serve(ctx, cfg.ControlSocket, func() any { return eng.Status() }); err != nil {
			log.Warn("control socket", "path", cfg.ControlSocket, "err", err)
		}
	}()

	var exitErr error
	sig := make(chan os.Signal, 4)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		for s := range sig {
			if s != syscall.SIGHUP {
				log.Info("stopping", "signal", s.String())
				cancel()
				return
			}
			if *path == "" {
				log.Info("SIGHUP ignored: config comes from the environment")
				continue
			}
			next, err := config.Load(*path)
			if err != nil {
				log.Error("reload failed; keeping the running config", "err", err)
				continue
			}
			if config.NeedsRestart(cfg, next) {
				log.Info("config change needs a restart; exiting for the supervisor to restart")
				exitErr = restartError{"keys, role, tunnel, peers or routing changed"}
				cancel()
				return
			}
			cfg = next
			eng.Reload(next)
		}
	}()

	if err := eng.Run(ctx); err != nil {
		return err
	}
	return exitErr
}

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print raw JSON")
	sock := fs.String("socket", "/var/run/weaver.sock", "control socket")
	if err := fs.Parse(args); err != nil {
		return err
	}
	b, err := control.Fetch(*sock)
	if err != nil {
		return fmt.Errorf("is weaver running? %w", err)
	}
	if *asJSON {
		_, err = os.Stdout.Write(b)
		return err
	}
	var st engine.Status
	if err := json.Unmarshal(b, &st); err != nil {
		return err
	}
	printStatus(os.Stdout, st)
	return nil
}

func printStatus(w io.Writer, st engine.Status) {
	fmt.Fprintf(w, "weaver %s  role %s  mode %s  up %ds\n", st.Version, st.Role, st.Mode, st.UptimeS)
	fmt.Fprintf(w, "tunnel %s %s mtu %d  public key %s\n", st.Tun.Name, st.Tun.Address, st.Tun.MTU, st.PubKey)
	for _, s := range st.Sessions {
		fmt.Fprintf(w, "\n%s: %s", s.Peer, s.State)
		if s.Endpoint != "" {
			fmt.Fprintf(w, " via %s", s.Endpoint)
		}
		fmt.Fprintf(w, "  tx %s  rx %s  reorder hold %.1fms held %d skipped %d dup %d\n",
			human(s.TxBytes), human(s.RxBytes), s.Reorder.HoldMs, s.Reorder.Held, s.Reorder.Timeouts, s.Reorder.Duplicates)
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  PATH\tDEVICE\tPRIORITY\tSTATE\tRTT ms\tLOSS %\tTX kbit/s\tRX kbit/s\tRATE kbit/s")
		for _, p := range s.Paths {
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%.1f\t%.2f\t%.0f\t%.0f\t%.0f\n",
				p.Name, p.Device, p.Priority, p.State, p.RTTMs, p.LossPct, p.TxKbps, p.RxKbps, p.RateKbps)
		}
		tw.Flush()
	}
}

func human(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%dB", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
