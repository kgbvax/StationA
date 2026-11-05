// Command station-log is the station's QSO record (muehle/hf/station-log,
// role log-store): ADIF import with duplicate merge, a read HTTP API, a
// retained /state and a per-QSO event topic. See CLAUDE.md and
// docs/station-log-mqtt-api.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"codeberg.org/kgbvax/stationa/shared/logging"

	"station-log/internal/app"
	"station-log/internal/config"
	"station-log/internal/importer"
	"station-log/internal/store"
)

func main() {
	fs := flag.NewFlagSet("station-log", flag.ExitOnError)
	flags := config.RegisterFlags(fs)
	check := fs.Bool("check", false, "validate the config, print it redacted, exit")
	file := fs.String("import-adif", "", "import this ADIF file into the database (no service, no events), print the report, exit")
	source := fs.String("source", "", "source tag of -import-adif (default \"adif\")")
	replace := fs.Bool("replace-source", false, "with -import-adif: delete the source's QSOs that the file does not hold (needs -source)")
	force := fs.Bool("force", false, "with -replace-source: allow deleting more than half of the source")
	_ = fs.Parse(os.Args[1:])

	cfg, err := config.Load(flags)
	if err != nil {
		fmt.Fprintf(os.Stderr, "station-log: %v\n", err)
		os.Exit(2)
	}
	if *check {
		fmt.Printf("slot %s/%s/%s  broker %s  data %s  http %s\n", cfg.MQTT.Site, cfg.Slot.Station, cfg.Slot.Slot, cfg.MQTT.Broker, cfg.DataDir, cfg.HTTP.Listen)
		fmt.Printf("default own call %q  dup window %s  log4om %q\n", cfg.DefaultOwnCall, cfg.DupWindow, cfg.Log4OM.Listen)
		for _, l := range cfg.WorkingCheck() {
			fmt.Println(l)
		}
		fmt.Println(cfg.WavelogCheck())
		fmt.Println("config check OK")
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

	if *file != "" {
		os.Exit(importFile(st, cfg, *file, *source, *replace, *force))
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	log.Info("station-log starting", "slot", cfg.MQTT.Site+"/"+cfg.Slot.Station+"/"+cfg.Slot.Slot, "broker", cfg.MQTT.Broker)
	if err := app.New(cfg, st, log).Run(ctx); err != nil && !logging.CleanStop(ctx, err) {
		log.Error("station-log exited", "err", err)
		os.Exit(1)
	}
	log.Info("station-log stopped")
}

// importFile merges one ADIF file offline and prints the report. A running
// service does not see it as events; its /state follows within a minute.
func importFile(st *store.Store, cfg config.Config, path, source string, replace, force bool) int {
	b, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "station-log: %v\n", err)
		return 2
	}
	opt := importer.Options{Source: source, ReplaceSource: replace, Force: force}
	if replace && source == "" {
		fmt.Fprintln(os.Stderr, "station-log: -replace-source needs -source")
		return 2
	}
	im := &importer.Importer{Store: st, Window: cfg.Window(), DefaultOwn: cfg.DefaultOwnCall}
	rep, err := im.Import(context.Background(), string(b), opt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "station-log: import failed: %v\n", err)
		return 1
	}
	fmt.Printf("records %d  added %d  merged %d  unchanged %d  skipped_invalid %d  deleted %d\n",
		rep.Records, rep.Added, rep.Merged, rep.Unchanged, rep.SkippedInvalid, rep.Deleted)
	for _, e := range rep.Errors {
		fmt.Println("  " + e)
	}
	return 0
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
	return slog.New(h).With("component", "station-log")
}
