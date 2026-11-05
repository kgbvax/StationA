// chaseq-ui shows chase-queue's ranked list in a native window on the shack
// PC. Enter on a row tunes the radio and puts the callsign into Log4OM;
// Skip and Undo go back to chase-queue. It is a consumer of
// muehle/hf/chase and a /cmd sender, not a slot.
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
	"runtime"
	"syscall"
	"time"

	"chaseq-ui/internal/action"
	"chaseq-ui/internal/bus"
	"chaseq-ui/internal/config"
	"chaseq-ui/internal/desktop"
	"chaseq-ui/internal/loggers"
	"chaseq-ui/internal/model"
	"chaseq-ui/internal/web"
)

// The native window must run on the main OS thread.
func init() { runtime.LockOSThread() }

func main() {
	cfgPath := flag.String("config", defaultConfig(), "path to config.toml")
	ui := flag.String("ui", "window", "window | browser | headless")
	flag.Parse()

	path := *cfgPath
	if _, err := os.Stat(path); err != nil && !flagSet("config") {
		path = "" // no config next to the exe: the defaults
	}
	cfg, err := config.Load(path)
	if err != nil {
		fatal(err)
	}
	closeLog := setupLog(cfg.Resolve(cfg.Log.File))
	defer closeLog()
	log := slog.Default().With("component", "chaseq-ui")
	log.Info("starting", "config", cfg.String(), "ui", *ui)

	ln, err := net.Listen("tcp", cfg.HTTP.Listen)
	if err != nil {
		// A second copy finds the port taken: nothing to do.
		log.Error("cannot listen (already running?)", "addr", cfg.HTTP.Listen, "err", err)
		fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	m := model.New(nil)
	filtersPath := cfg.Resolve(cfg.FiltersFile)
	m.SetFilters(model.LoadFilters(filtersPath))
	radio := &action.Radio{}

	var srv *web.Server
	notify := func() {
		if srv != nil {
			srv.Notify()
		}
	}
	sess := bus.NewSession(bus.Options{
		Broker: cfg.MQTT.Broker, User: cfg.MQTT.User, Password: cfg.MQTT.Password, ClientID: cfg.MQTT.ClientID,
		Log:    log,
		Topics: bus.Topics{ChaseState: cfg.ChaseState(), ChaseStatus: cfg.ChaseStatus(), RadioState: cfg.RadioState()},
		Handlers: bus.Handlers{
			Chase:  func(s bus.ChaseState) { m.SetChase(s); notify() },
			Status: func(on bool) { m.SetStatus(on); notify() },
			Radio: func(s bus.RadioState) {
				radio.Set(s)
				// A band lock follows the radio, whoever tuned it; re-render
				// and save only when the band filter really changed.
				if _, changed := m.SetRadioBand(s.ListenBand()); changed {
					if err := m.SaveTo(filtersPath); err != nil {
						log.Warn("saving filters", "err", err)
					}
					notify()
				}
			},
		},
	})
	senders, err := buildSenders(cfg.Loggers)
	if err != nil {
		fatal(err)
	}
	for _, s := range senders {
		log.Info("logger sender", "name", s.Name())
	}
	runner := &action.Runner{
		Model: m, Radio: radio, Pub: sess,
		ChaseCmd: cfg.ChaseCmd(), RadioCmd: cfg.RadioCmd(),
		Loggers: senders,
		Timing:  action.DefaultTiming,
	}
	srv, err = web.New(m, runner, filtersPath, stop)
	if err != nil {
		fatal(err)
	}

	go func() {
		if err := sess.Run(ctx); err != nil {
			log.Error("mqtt", "err", err)
			stop()
		}
	}()
	go func() {
		if err := srv.Serve(ctx, ln); err != nil {
			log.Error("http", "err", err)
			stop()
		}
	}()
	// The staleness rule has no event of its own; the SSE ticker covers the page.

	url := "http://" + ln.Addr().String() + "/"
	switch *ui {
	case "window":
		if err := desktop.Window(ctx, url, "chaseq-ui", 520, 760); err != nil {
			log.Warn("falling back to the browser", "err", err)
			_ = desktop.OpenBrowser(url)
			<-ctx.Done()
		}
	case "browser":
		_ = desktop.OpenBrowser(url)
		<-ctx.Done()
	default:
		log.Info("headless", "url", url)
		<-ctx.Done()
	}
	log.Info("stopped")
}

// buildSenders turns the enabled [[loggers.sender]] entries (or the legacy
// log4om_* keys) into loggers.Senders; a bad entry stops the start.
func buildSenders(lc config.LoggersConfig) ([]loggers.Sender, error) {
	var specs []loggers.Spec
	for i, sc := range lc.Senders() {
		if !sc.On() {
			continue
		}
		var timeout time.Duration
		if sc.Timeout != "" {
			d, err := time.ParseDuration(sc.Timeout)
			if err != nil {
				return nil, fmt.Errorf("loggers.sender #%d: timeout %q: %w", i+1, sc.Timeout, err)
			}
			timeout = d
		}
		specs = append(specs, loggers.Spec{Kind: sc.Kind, Addr: sc.Addr, Timeout: timeout})
	}
	return loggers.Build(specs)
}

func flagSet(name string) bool {
	set := false
	flag.Visit(func(f *flag.Flag) { set = set || f.Name == name })
	return set
}

func defaultConfig() string {
	exe, err := os.Executable()
	if err != nil {
		return "config.toml"
	}
	return filepath.Join(filepath.Dir(exe), "config.toml")
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "chaseq-ui:", err)
	os.Exit(1)
}

// setupLog logs to path (rotated to path.1 above 5 MB) because a windowsgui
// exe has no stderr; with no path it logs to stderr.
func setupLog(path string) func() {
	if path == "" {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
		return func() {}
	}
	if fi, err := os.Stat(path); err == nil && fi.Size() > 5<<20 {
		_ = os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
		return func() {}
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(f, nil)))
	return func() { f.Close() }
}
