// Command pelcots drives and observes a Pelco-D PTZ / rotator. It connects over
// a directly attached serial port or a TCP serial bridge, and runs either as an
// interactive terminal UI (tap-step keys, live position readback) or as a
// headless daemon that acts purely as a network rotator controller speaking the
// Hamlib rotctld protocol. Optional cable-wrap protection guards
// infinite-azimuth rotators against over-winding.
//
// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"pelcots/internal/config"
	"pelcots/internal/engine"
	"pelcots/internal/ui"
)

func main() {
	cfgPath := flag.String("config", "pelcots.yaml", "path to the YAML settings file")
	port := flag.String("port", "", "serial device path (overrides config)")
	baud := flag.Int("baud", 0, "serial baud rate (overrides config)")
	addr := flag.Uint("addr", 0, "Pelco-D camera address 1-255 (overrides config)")
	protocol := flag.String("protocol", "", "wire protocol: d (Pelco-D) | p (Pelco-P) (overrides config)")
	logPath := flag.String("log", "", "append TX/RX trace to this file (overrides config)")
	logLevel := flag.String("loglevel", "", "log verbosity: error | warn | info | debug | trace (overrides config)")
	tcp := flag.String("tcp", "", "TCP serial-bridge address host:port (overrides config)")
	transport := flag.String("transport", "", "outbound transport: serial | tcp | sim (overrides config; sim = in-memory emulator, no hardware)")
	daemon := flag.Bool("d", false, "run headless as a network controller (no TUI)")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}

	// Override config with only the flags the user explicitly set.
	set := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if set["port"] {
		cfg.Serial.Port = *port
	}
	if set["baud"] {
		cfg.Serial.Baud = *baud
	}
	if set["addr"] {
		if *addr < 1 || *addr > 255 {
			fmt.Fprintln(os.Stderr, "addr must be in 1-255")
			os.Exit(2)
		}
		cfg.Addr = byte(*addr)
	}
	if set["protocol"] {
		cfg.Protocol = strings.ToLower(strings.TrimSpace(*protocol))
	}
	// Validate however the value arrived (flag or pelcots.yaml): a typo like
	// `protocol: pelco-p` must not silently run as Pelco-D.
	switch cfg.Protocol {
	case config.ProtocolD, config.ProtocolP:
	default:
		fmt.Fprintf(os.Stderr, "protocol must be %q or %q\n", config.ProtocolD, config.ProtocolP)
		os.Exit(2)
	}
	if set["log"] {
		cfg.Log = *logPath
	}
	if set["loglevel"] {
		lvl, err := config.ParseLogLevel(*logLevel)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		cfg.LogLevel = lvl
	}
	if set["tcp"] {
		cfg.TCP.Address = *tcp
	}
	if set["transport"] {
		cfg.Transport = *transport
	} else if set["tcp"] {
		cfg.Transport = config.TransportTCP // -tcp implies TCP unless -transport says otherwise
	}

	logw, closeLog := openLog(cfg.Log, *daemon)
	defer closeLog()

	eng := engine.New(engine.Options{
		Transport:   cfg.Transport,
		SerialPort:  cfg.Serial.Port,
		TCPAddr:     cfg.TCP.Address,
		Baud:        cfg.Serial.Baud,
		Addr:        cfg.Addr,
		Protocol:    cfg.Protocol,
		Sim:         cfg.Sim,
		SelfCheck:   cfg.SelfCheck,
		Bind:        cfg.Control.Bind,
		WrapEnabled: cfg.Wrap.Enabled,
		WrapLimit:   cfg.Wrap.Limit,
		WrapAccum:   cfg.Wrap.Accumulated,
		AzOffset:    cfg.AzOffset,
		TiltInvert:  cfg.TiltInvert,
		TiltCal:     cfg.TiltCal,
		Goto:        cfg.Goto,
		Rotctld:     cfg.Control.Rotctld,
		Logw:        logw,
		LogLevel:    cfg.LogLevel,
	})
	eng.Start()

	if *daemon {
		runDaemon(eng, cfg, *cfgPath)
		return
	}
	runTUI(eng, cfg, *cfgPath)
}

// runTUI runs the interactive terminal UI and persists settings on quit.
func runTUI(eng *engine.Engine, cfg config.Config, cfgPath string) {
	model := ui.New(eng, cfg, cfg.Log)
	prog := tea.NewProgram(model, tea.WithAltScreen())
	final, err := prog.Run()
	_ = eng.Close()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if m, ok := final.(ui.Model); ok {
		saveConfig(cfgPath, m.Config())
	}
}

// runDaemon runs headless until SIGINT/SIGTERM, acting as a network controller.
// It persists the cable-wind accumulator periodically and on shutdown.
func runDaemon(eng *engine.Engine, cfg config.Config, cfgPath string) {
	if !cfg.Control.Rotctld.Enabled {
		fmt.Fprintln(os.Stderr, "daemon: no control server enabled (set control.rotctld.enabled)")
		_ = eng.Close()
		os.Exit(2)
	}
	fmt.Fprintf(os.Stderr, "pelcots daemon: network controller (bind %s), Ctrl-C to stop\n", cfg.Control.Bind)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			saveConfig(cfgPath, configFromState(cfgPath, eng.Snapshot())) // crash-resilient wind persistence
		case <-sig:
			_ = eng.Close()
			saveConfig(cfgPath, configFromState(cfgPath, eng.Snapshot()))
			return
		}
	}
}

// openLog opens the trace file (append). In daemon mode the trace is also
// mirrored to stderr. Returns the writer (may be nil) and a close func.
func openLog(path string, daemon bool) (io.Writer, func()) {
	var file *os.File
	if path != "" {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "cannot open log file %s: %v\n", path, err)
		} else {
			file = f
			fmt.Fprintf(f, "\n=== pelcots session %s ===\n", time.Now().Format(time.RFC3339))
		}
	}
	switch {
	case file != nil && daemon:
		return io.MultiWriter(file, os.Stderr), func() { _ = file.Close() }
	case file != nil:
		return file, func() { _ = file.Close() }
	case daemon:
		return os.Stderr, func() {}
	default:
		return nil, func() {}
	}
}

// configFromState re-reads the config file and refreshes only the engine-owned
// fields (transport, servers, wind) from the live snapshot. Re-reading — rather
// than starting from the in-memory config loaded at startup — preserves manual
// edits to other fields (e.g. log_level) made while the daemon runs, which the
// periodic save would otherwise clobber.
func configFromState(path string, s engine.State) config.Config {
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot reload config %s: %v\n", path, err)
	}
	cfg.Transport = s.Transport
	cfg.Serial.Port = s.SerialPort
	cfg.Serial.Baud = s.Baud
	cfg.TCP.Address = s.TCPAddr
	cfg.Addr = s.Addr
	cfg.Protocol = s.Protocol
	cfg.AzOffset = s.AzOffset
	cfg.Control.Bind = s.Bind
	cfg.Control.Rotctld = config.ServerConfig{Enabled: s.RotctldOn, Port: s.RotctldPort}
	cfg.Wrap = config.WrapConfig{Enabled: s.WrapEnabled, Limit: s.WrapLimit, Accumulated: s.Wrap}
	return cfg
}

func saveConfig(path string, c config.Config) {
	if err := config.Save(path, c); err != nil {
		fmt.Fprintf(os.Stderr, "cannot save config %s: %v\n", path, err)
	}
}
