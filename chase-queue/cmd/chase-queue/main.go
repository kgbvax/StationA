// Command chase-queue is the chase queue slot (muehle/hf/chase): it reads the
// stations bmrec hears calling CQ from the bus, knows the DX spots and the
// operator's history (DXLog in contests, Log4OM and Club Log otherwise), and
// publishes them as one ordered list of what is needed. See CLAUDE.md and
// docs/chase-queue-mqtt-api.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"codeberg.org/kgbvax/stationa/shared/logging"

	"chase-queue/internal/app"
	"chase-queue/internal/award"
	"chase-queue/internal/clublog"
	"chase-queue/internal/config"
	"chase-queue/internal/qrz"
	"chase-queue/internal/store"
)

func main() {
	fs := flag.NewFlagSet("chase-queue", flag.ExitOnError)
	flags := config.RegisterFlags(fs)
	check := fs.Bool("check", false, "validate the config, print it redacted, exit")
	probe := fs.Bool("clublog-probe", false, "fetch the Club Log matrix once and print how many cells carry each status value, then exit")
	qrzLookup := fs.String("qrz-lookup", "", "look one callsign up on QRZ.com with the configured login, print the entry, exit")
	importXML := fs.String("import-award-xml", "", "convert a Log4OM award XML export (QSOFIELDS) into a definition file on stdout, then exit; REFERENCE awards are deferred")
	_ = fs.Parse(os.Args[1:])

	if *importXML != "" {
		os.Exit(importAwardXML(*importXML, os.Stdout, os.Stderr))
	}

	cfg, err := config.Load(flags)
	if err != nil {
		fmt.Fprintf(os.Stderr, "chase-queue: %v\n", err)
		os.Exit(2)
	}
	if *check {
		fmt.Printf("slot %s/%s/%s  broker %s  data %s  http %s\n", cfg.MQTT.Site, cfg.Slot.Station, cfg.Slot.Slot, cfg.MQTT.Broker, cfg.DataDir, cfg.HTTP.Listen)
		fmt.Printf("dxlog %q  log4om %q  clublog call %q token set %v\n", cfg.Contest.Listen, cfg.Log4OM.Listen, cfg.ClubLog.Call, cfg.ClubLog.Token != "")
		for _, c := range cfg.Cluster {
			fmt.Printf("cluster %q  address %q  call %q  skimmer %v  enabled %v\n", c.Name, c.Address, c.Call, c.Skimmer, c.IsEnabled())
		}
		fmt.Printf("bmrec gate %s  buddies %d\n", cfg.Bmrec.Gate, len(cfg.Buddies))
		fmt.Printf("qrz user %q  password set %v  refresh %d days\n", cfg.QRZ.User, cfg.QRZ.Password != "", cfg.QRZ.RefreshDays)
		cat, err := award.LoadDir(cfg.AwardsDir())
		if err != nil {
			fmt.Fprintf(os.Stderr, "chase-queue: %v\n", err)
			os.Exit(2)
		}
		fmt.Printf("awards dir %s  awards %v  goals %d\n", cfg.AwardsDir(), cat.Codes(), len(cfg.Goals))
		fmt.Println("config check OK")
		return
	}
	if *qrzLookup != "" {
		if !cfg.QRZ.Enabled() {
			fmt.Fprintln(os.Stderr, "chase-queue: set qrz.user and CHASE_QUEUE_QRZ_PASSWORD")
			os.Exit(2)
		}
		c := &qrz.Client{User: cfg.QRZ.User, Password: cfg.QRZ.Password, Agent: "chase-queue"}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		r, err := c.Lookup(ctx, strings.ToUpper(*qrzLookup))
		if err != nil {
			fmt.Fprintln(os.Stderr, "chase-queue: qrz lookup failed:", err)
			os.Exit(1)
		}
		fmt.Printf("subscription: %s\n%+v\n", c.Subscription(), r)
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

// importAwardXML prints the definition file for a Log4OM award export; it
// returns the exit code: 0 done, 1 unreadable or unusable, 3 a REFERENCE
// award (deferred).
func importAwardXML(path string, stdout, stderr io.Writer) int {
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(stderr, "chase-queue: %v\n", err)
		return 1
	}
	defer f.Close()
	def, notes, err := award.ImportXML(f)
	if errors.Is(err, award.ErrDeferred) {
		fmt.Fprintf(stderr, "chase-queue: deferred: %v\n", err)
		return 3
	}
	if err != nil {
		fmt.Fprintf(stderr, "chase-queue: %v\n", err)
		return 1
	}
	// the same checks the service makes at start
	if err := award.NewCatalog().Add(def); err != nil {
		fmt.Fprintf(stderr, "chase-queue: the imported award is not usable: %v\n", err)
		return 1
	}
	out, err := award.EncodeTOML(def)
	if err != nil {
		fmt.Fprintf(stderr, "chase-queue: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "# imported from %s; put it into the awards directory, then add a [[goal]]\n", filepath.Base(path))
	for _, n := range notes {
		fmt.Fprintf(stdout, "# note: %s\n", n)
	}
	_, _ = stdout.Write(out)
	return 0
}
