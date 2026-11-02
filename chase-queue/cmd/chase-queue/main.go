// Command chase-queue is the chase queue slot (muehle/hf/chase): it reads the
// stations bmrec hears calling CQ from the bus, knows the DX spots and the
// operator's history (DXLog in contests, Log4OM and Club Log otherwise), and
// publishes them as one ordered list of what is needed. See CLAUDE.md and
// docs/chase-queue-mqtt-api.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"codeberg.org/kgbvax/stationa/shared/logging"

	"chase-queue/internal/app"
	"chase-queue/internal/clublog"
	"chase-queue/internal/config"
	"chase-queue/internal/store"
)

func main() {
	fs := flag.NewFlagSet("chase-queue", flag.ExitOnError)
	flags := config.RegisterFlags(fs)
	check := fs.Bool("check", false, "validate the config, print it redacted, exit")
	probe := fs.Bool("clublog-probe", false, "fetch the Club Log matrix once and print how many cells carry each status value, then exit")
	_ = fs.Parse(os.Args[1:])

	cfg, err := config.Load(flags)
	if err != nil {
		fmt.Fprintf(os.Stderr, "chase-queue: %v\n", err)
		os.Exit(2)
	}
	if *check {
		fmt.Printf("slot %s/%s/%s  broker %s  data %s  http %s\n", cfg.MQTT.Site, cfg.Slot.Station, cfg.Slot.Slot, cfg.MQTT.Broker, cfg.DataDir, cfg.HTTP.Listen)
		fmt.Printf("dxlog %q  log4om %q  clublog call %q token set %v\n", cfg.Contest.Listen, cfg.Log4OM.Listen, cfg.ClubLog.Call, cfg.ClubLog.Token != "")
		fmt.Println("config check OK")
		return
	}
	if *probe {
		c := &clublog.Confirmations{Token: cfg.ClubLog.Token, Call: cfg.ClubLog.Call}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		counts, err := c.Probe(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, "chase-queue: club log probe failed:", err)
			os.Exit(1)
		}
		fmt.Println("matrix cells per status value (compare with the Club Log DXCC chart; set clublog.confirmed_status):")
		for st, n := range counts {
			fmt.Printf("  status %d: %d cells\n", st, n)
		}
		return
	}

	log := newLogger(cfg.Log.Level)
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		log.Error("data_dir unusable", "err", err)
		os.Exit(1)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		log.Error("opening the database failed", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	log.Info("chase-queue starting", "slot", cfg.MQTT.Site+"/"+cfg.Slot.Station+"/"+cfg.Slot.Slot, "broker", cfg.MQTT.Broker)
	if err := app.New(cfg, st, log).Run(ctx); err != nil && !logging.CleanStop(ctx, err) {
		log.Error("chase-queue exited", "err", err)
		os.Exit(1)
	}
	log.Info("chase-queue stopped")
}

func newLogger(level string) *slog.Logger {
	lv := slog.LevelInfo
	switch level {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	}
	h := logging.NewHandler(os.Stderr, &slog.HandlerOptions{Level: lv})
	return slog.New(h).With("component", "chase-queue")
}
