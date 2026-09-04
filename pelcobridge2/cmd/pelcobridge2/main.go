// pelcobridge2 is the UHF rotator TUI: a Pelco-D/P pan/tilt head on RS-485,
// driven manually from this console and, once ARMED, by hamlib rotctld
// clients. One engine goroutine owns the serial link; this main only wires.
//
// No daemon: the TUI is the application. MQTT (optional) and the rotctld
// listener are background services of the same process; arming is always a
// keyboard act and never remote-controlled.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	pahomqtt "github.com/eclipse/paho.mqtt.golang"

	sharedmqtt "codeberg.org/kgbvax/stationa/shared/mqtt"

	"pelcobridge2/internal/config"
	"pelcobridge2/internal/control"
	"pelcobridge2/internal/mqtt"
	"pelcobridge2/internal/rotctld"
	"pelcobridge2/internal/serialio"
	"pelcobridge2/internal/ui"
)

func main() {
	// Root logger per the logging convention: slog text → stderr, one constant
	// `component` attr, no app-side timestamps (journald stamps its own).
	// The TUI renders on stdout's alt screen; stderr Warn+ lines are the exact
	// pattern the MQTT slot already used before this migration, so they are
	// proven not to disturb the TUI any more than the slot's old stderrLogger did.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})).With("component", "pelcobridge2"))

	var (
		cfgFlag    = flag.String("config", "", "path to config.toml (default: $PELCOBRIDGE2_CONFIG > exe dir > ./config.toml)")
		portFlag   = flag.String("port", "", "serial port (overrides [serial].port)")
		addrFlag   = flag.Int("addr", -1, "head's Pelco address (overrides [serial].addr)")
		baudFlag   = flag.Int("baud", -1, "baud rate (overrides [serial].baud)")
		listPorts  = flag.Bool("list-ports", false, "enumerate serial ports (with USB identity) and exit")
		noRotctld  = flag.Bool("no-rotctld", false, "do not listen for rotctld clients")
		crawlFlag  = flag.Bool("crawl", false, "gotos converge by 1 s low-speed jog bursts instead of absolute sets (overrides [control] crawl)")
		crawlSpd   = flag.Int("crawl-speed", -1, "crawl jog speed byte 1–0x3F (0 = the unset sentinel, becomes the default 4)")
		crawlTol   = flag.Float64("crawl-tolerance", -1, "crawl 'finished' tolerance in degrees (default 4.0)")
		showConfig = flag.Bool("print-config", false, "print the resolved configuration and exit")
	)
	flag.Parse()

	if *listPorts {
		ports, err := serialio.EnumeratePorts()
		if err != nil {
			fatal("list ports: %v", err)
		}
		serialio.WritePorts(os.Stdout, ports)
		return
	}

	cfg, cfgPath, err := loadConfig(*cfgFlag)
	if err != nil {
		fatal("%v", err)
	}
	flag.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "port":
			cfg.Serial.Port = *portFlag
		case "addr":
			cfg.Serial.Addr = byte(*addrFlag)
		case "baud":
			cfg.Serial.Baud = *baudFlag
		case "crawl":
			cfg.Control.Crawl = *crawlFlag
		case "crawl-speed":
			cfg.Control.CrawlSpeed = *crawlSpd
		case "crawl-tolerance":
			cfg.Control.CrawlToleranceDeg = *crawlTol
		}
	})
	if *showConfig {
		// Print the ENGINE's values (post-clamp), not the raw TOML/flag ones:
		// an operator bench-tuning via -print-config must see exactly what
		// goes on the wire (EngineConfig clamps out-of-range speeds/tolerances).
		eng := cfg.EngineConfig()
		fmt.Printf("config: %s\nserial: port=%s baud=%d addr=%d\nrotctld: %s (enabled=%v)\ncontrol: jog=0x%02X settle=%dms tol=%.2f hold=%dms crawl=%v crawl_speed=0x%02X crawl_tol=%.1f\nmqtt: enabled=%v broker=%s slot=%s\n",
			orNone(cfgPath), cfg.Serial.Port, cfg.Serial.Baud, cfg.Serial.Addr,
			cfg.RotctldAddr(), cfg.Rotctld.Enabled,
			eng.JogSpeed, eng.Settle.Milliseconds(), cfg.Control.SetToleranceDeg, cfg.Control.JogHoldMS,
			eng.Crawl, eng.CrawlSpeed, eng.CrawlTol,
			cfg.MQTT.Enabled, cfg.MQTT.Broker, cfg.MQTT.Site+"/"+cfg.MQTT.Station+"/"+cfg.MQTT.Slot)
		return
	}
	if cfg.Serial.Port == "" {
		fatal("no serial port configured — use -port or [serial] port in config.toml")
	}

	// The serial link is this program's reason to exist: opening it is fatal
	// on failure (the operator sees the error, not a dead TUI). A "tcp:" port
	// reaches pelcobridge2-mock over the network (Windows has no pty).
	var tr serialio.Transport
	var reopen func() error
	if host, ok := strings.CutPrefix(cfg.Serial.Port, "tcp:"); ok {
		tt, err := dialTCP(host)
		if err != nil {
			fatal("connect %s: %v", host, err)
		}
		tr, reopen = tt, tt.reconnect
	} else {
		sp, err := serialio.OpenPort(cfg.Serial.Port, cfg.Serial.Baud)
		if err != nil {
			fatal("open %s @ %d: %v", cfg.Serial.Port, cfg.Serial.Baud, err)
		}
		tr, reopen = sp, sp.Reopen
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Engine: one goroutine owning the wire and all state.
	reqCh := make(chan control.Request, 64)
	sink := make(chan control.Event, 512)
	eng := control.New(cfg.EngineConfig(), tr, reopen, reqCh, sink)
	go eng.Run(ctx)

	// Event pump: engine sink → MQTT /state + TUI log/snapshot. Created before
	// the rotctld goroutine so that one can report into the TUI log too.
	evCh := make(chan control.Event, 256)

	// rotctld server (the engine IS the Rot: it exposes Call with a source).
	limits := rotctld.Limits{
		MinAz: cfg.Control.MinAz, MaxAz: cfg.Control.MaxAz,
		MinEl: cfg.Control.MinEl, MaxEl: cfg.Control.MaxEl,
	}
	rotSrv := rotctld.New(eng, rotctldInfo(cfg), limits)
	if cfg.Rotctld.Enabled && !*noRotctld {
		go func() {
			if err := rotSrv.ListenAndServe(ctx, cfg.RotctldAddr()); err != nil && ctx.Err() == nil {
				// Never cancel: a busy port or bad bind must not kill the
				// whole console — say it in the TUI log pane AND the journal
				// (a lost listener is a real failure, hence Error level).
				slog.Error("rotctld server failed", "err", err)
				select {
				case evCh <- control.Event{Log: "rotctld: " + err.Error()}:
				default:
				}
			}
		}()
	}

	// MQTT slot: background, never fatal, never allowed to arm or move.
	var slot *mqtt.Slot
	mqttUp := func() bool { return false }
	// The rotator's slot address for the journal: engine events and MQTT lines
	// are filterable by slot per the logging convention, even with MQTT off.
	slotAddr := cfg.MQTT.Site + "/" + cfg.MQTT.Station + "/" + cfg.MQTT.Slot
	if cfg.MQTT.Enabled {
		mcfg := cfg.MQTTConfig(os.Getenv("PELCOBRIDGE2_MQTT_PASSWORD"))
		// The client needs the slot's OnConnect; the slot needs the client.
		// Break the cycle via the callback's own client reference — OnConnect
		// publishes through the client paho passes in, never the stored one.
		var client pahomqtt.Client
		client = mqtt.NewClient(mcfg, func(c pahomqtt.Client) {
			if slot != nil {
				slot.OnConnect(c)
			}
		})
		slotLog := slog.Default().With("slot", slotAddr)
		slot = mqtt.NewSlot(mcfg, &mqtt.PahoPublisher{Client: client}, slogSlotLogger{slotLog},
			func(it control.Intent) error { return control.Submit(reqCh, control.SrcMQTT, it) },
			func() int { return rotSrv.Clients() })
		// Connect is ctx-aware and non-fatal: the TUI works without a broker.
		// A connect failure is a real failure (Error), but must not end the TUI.
		go func() {
			if err := sharedmqtt.Connect(ctx, client); err != nil {
				slotLog.Error("mqtt connect failed", "err", err)
			}
		}()
		go sharedmqtt.RunJobs(ctx, slot.Jobs())
		mqttUp = client.IsConnected
	}

	// Event pump: engine sink → MQTT /state + TUI log/snapshot. Degraded
	// states are mirrored to slog at Warn+ so an operator session lands in the
	// journal even when this TUI runs unattended (convention: journalctl -p
	// warning is the station-wide error filter). Ordinary engine activity
	// (wire notes, frames) stays TUI-only — never mirrored.
	//
	// Dedup by transition: the engine's errStr never clears within a run, so
	// compare each snapshot against the previous one and log only changes;
	// device_online likewise flips only on link death / proof of life.
	go func() {
		var lastErr string
		var lastOnline bool
		for ev := range sink {
			if ev.Snap != nil {
				if slot != nil {
					slot.PublishState(ev.Snap)
				}
				if ev.Snap.Error != lastErr {
					if ev.Snap.Error != "" {
						slog.Warn("engine error", "slot", slotAddr, "err", ev.Snap.Error)
					}
					lastErr = ev.Snap.Error
				}
				if ev.Snap.DeviceOnline != lastOnline {
					lastOnline = ev.Snap.DeviceOnline
					if lastOnline {
						slog.Info("head link online", "slot", slotAddr)
					} else {
						slog.Warn("head link offline", "slot", slotAddr)
					}
				}
			}
			select {
			case evCh <- ev:
			default: // the TUI drops stale frames rather than falling behind
			}
		}
	}()

	// The TUI is the application; its return ends everything (ctx cancel makes
	// the engine send one final all-stop on the way out).
	stateFile := statePath(cfgPath)
	m := ui.New(ui.Options{
		ReqCh:    reqCh,
		EvCh:     evCh,
		PortName: cfg.Serial.Port,
		Baud:     cfg.Serial.Baud,
		Addr:     cfg.Serial.Addr,
		JogHold:  time.Duration(cfg.Control.JogHoldMS) * time.Millisecond,
		Prefill:  config.LoadState(stateFile).LastOffsetDeg,
		OnArm: func(deg float64) {
			_ = config.SaveState(stateFile, config.State{LastOffsetDeg: deg})
		},
		Crawl:   cfg.Control.Crawl,
		MQTTOn:  mqttUp,
		Clients: rotSrv.Clients,
	})
	if err := ui.Run(m); err != nil {
		slog.Error("tui exited", "err", err)
	}
	cancel()
}

// loadConfig resolves and loads the TOML config. Missing file is fine
// (seed-once convention: defaults + flags carry the first run).
func loadConfig(flagPath string) (config.Config, string, error) {
	cfgPath := config.ResolvePath(flagPath, os.Getenv("PELCOBRIDGE2_CONFIG"))
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return config.Default(), cfgPath, err
	}
	return cfg, cfgPath, nil
}

// statePath keeps state.toml next to the config file (or the exe on a
// flagless first run).
func statePath(cfgPath string) string {
	if cfgPath != "" {
		return filepath.Join(filepath.Dir(cfgPath), "state.toml")
	}
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), "state.toml")
	}
	return "state.toml"
}

func rotctldInfo(cfg config.Config) string {
	name := cfg.MQTT.DeviceName
	if name == "" {
		name = "UHF Rotator"
	}
	return "pelcobridge2 · " + name
}

// slogSlotLogger adapts the slot's Logger interface to slog. The adapter (not
// the slot code) carries the `slot` attr, and Warnf maps to slog.Warn — never
// to Info, or `journalctl -p warning` would miss every MQTT failure
// (logging-convention §5).
type slogSlotLogger struct{ l *slog.Logger }

func (s slogSlotLogger) Infof(format string, args ...any) { s.l.Info(fmt.Sprintf(format, args...)) }
func (s slogSlotLogger) Warnf(format string, args ...any) { s.l.Warn(fmt.Sprintf(format, args...)) }

func orNone(s string) string {
	if s == "" {
		return "(defaults — no config file found)"
	}
	return s
}

// tcpTransport adapts a TCP connection to a serialio.Transport so the loopback
// smoke test (pelcobridge2-mock -listen) also works on Windows, which has no
// pty/socat. Reconnect heals a dropped mock the way serial reopen heals USB;
// the engine restarts its reader generation after a successful reconnect.
// conn is mutex-guarded for the same reason SerialPort.port is: the reader
// goroutine and the engine loop (write/reconnect) touch it concurrently.
type tcpTransport struct {
	addr string

	mu   sync.Mutex
	conn net.Conn
}

func dialTCP(addr string) (*tcpTransport, error) {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	return &tcpTransport{addr: addr, conn: conn}, nil
}

func (t *tcpTransport) Read(p []byte) (int, error) {
	t.mu.Lock()
	conn := t.conn
	t.mu.Unlock()
	if conn == nil {
		return 0, fmt.Errorf("connection is closed")
	}
	return conn.Read(p)
}

func (t *tcpTransport) Write(b []byte) error {
	t.mu.Lock()
	conn := t.conn
	t.mu.Unlock()
	if conn == nil {
		return fmt.Errorf("connection is closed")
	}
	// A wedged peer must not wedge the engine loop: the e-stop rides on this
	// write. Bound it the way every other engine window is bounded.
	_ = conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	_, err := conn.Write(b)
	return err
}

func (t *tcpTransport) Close() error {
	t.mu.Lock()
	conn := t.conn
	t.conn = nil
	t.mu.Unlock()
	if conn == nil {
		return nil
	}
	return conn.Close()
}

func (t *tcpTransport) reconnect() error {
	t.mu.Lock()
	old := t.conn
	t.conn = nil
	t.mu.Unlock()
	if old != nil {
		_ = old.Close() // unblocks the reader goroutine on the dead conn
	}
	conn, err := net.DialTimeout("tcp", t.addr, 3*time.Second)
	if err != nil {
		return err
	}
	t.mu.Lock()
	t.conn = conn
	t.mu.Unlock()
	return nil
}

func fatal(format string, args ...any) {
	slog.Error(fmt.Sprintf(format, args...))
	// Double-clicked exe on Windows: the console window closes instantly and
	// the error is never read. Hold it open until Enter (EOF on closed stdin
	// returns immediately, so non-interactive callers are unaffected).
	fmt.Fprint(os.Stderr, "press Enter to exit…")
	_, _ = fmt.Scanln()
	os.Exit(1)
}
