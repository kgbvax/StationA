// Command powerseq runs the station startup/shutdown sequencer (integration model
// `sequencer` role, logic slot muehle/hf/power-seq). It subscribes the /status of
// the power-distribution + HF slots (and hf/pa/state) and, on the operator
// one-button /cmd (start|stop), runs an ordered startup or shutdown over those
// slots' retained /cmd with delays and liveness confirmations. See
// docs/powerseq-mqtt-api.md and ../stationa/docs/station-integration-model.md §7.1.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"powerseq/internal/config"
	"powerseq/internal/mqtt"
)

func main() {
	fs := flag.NewFlagSet("powerseq", flag.ExitOnError)
	flags := config.RegisterFlags(fs)
	_ = fs.Parse(os.Args[1:])

	cfg, err := config.Load(flags)
	if err != nil {
		fmt.Fprintf(os.Stderr, "powerseq: load config: %v\n", err)
		os.Exit(2)
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "powerseq: %v\n", err)
		os.Exit(2)
	}

	logger := mqtt.NewLogger(cfg.Log.Level)
	logger.Info("powerseq starting",
		"slot", cfg.MQTT.Site+"/"+cfg.MQTT.Station+"/"+cfg.MQTT.Slot,
		"network_delay_s", cfg.Timing.NetworkDelayS,
		"step_timeout_s", cfg.Timing.StepTimeoutS)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	client, seq, err := mqtt.New(ctx, cfg, logger)
	if err != nil {
		logger.Error("powerseq: mqtt init failed", "err", err)
		os.Exit(1)
	}
	defer client.Close()

	// Run the sequencer state machine on its own goroutine; it emits the
	// ordered /cmd sequence and publishes its /state.
	go seq.Run(ctx)

	logger.Info("powerseq running")
	<-ctx.Done()
	logger.Info("powerseq stopped")
}
