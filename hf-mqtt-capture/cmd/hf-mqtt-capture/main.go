// Command hf-mqtt-capture subscribes to the station MQTT bus under a site/station
// subtree and writes every message to timestamped, hourly-rotated log files on disk.
// It is a passive diagnostic recorder: it never publishes and has no control surface.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	pahomqtt "github.com/eclipse/paho.mqtt.golang"

	sharedmqtt "codeberg.org/kgbvax/stationa/shared/mqtt"

	"hf-mqtt-capture/internal/config"
)

func main() {
	cfgPath := flag.String("config", "/etc/hf-mqtt-capture/config.toml", "path to TOML config")
	flag.Parse()

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hf-mqtt-capture: load config: %v\n", err)
		os.Exit(2)
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "hf-mqtt-capture: %v\n", err)
		os.Exit(2)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	logger.Info("hf-mqtt-capture starting", "broker", cfg.Broker, "site", cfg.Site, "station", cfg.Station, "log_dir", cfg.LogDir)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	writer, err := newRotatingWriter(cfg.LogDir, cfg.RetentionHours)
	if err != nil {
		logger.Error("open log writer", "err", err)
		os.Exit(1)
	}
	defer writer.close()

	if err := run(ctx, cfg, writer, logger); err != nil {
		logger.Error("run failed", "err", err)
		os.Exit(1)
	}
	logger.Info("hf-mqtt-capture stopped")
}

func loadConfig(path string) (config.Config, error) {
	cfg, err := config.Load(path)
	if err != nil {
		if os.IsNotExist(err) {
			return config.Default(), nil
		}
		return config.Config{}, err
	}
	return cfg, nil
}

func run(ctx context.Context, cfg config.Config, w *rotatingWriter, logger *slog.Logger) error {
	topic := fmt.Sprintf("%s/%s/#", cfg.Site, cfg.Station)

	opts := pahomqtt.NewClientOptions().
		AddBroker(cfg.Broker).
		SetClientID(fmt.Sprintf("%s-%s-hf-mqtt-capture", cfg.Site, cfg.Station)).
		SetAutoReconnect(true).
		SetCleanSession(true).
		SetConnectionLostHandler(func(_ pahomqtt.Client, err error) {
			logger.Warn("mqtt connection lost", "err", err)
		})
	opts.SetOnConnectHandler(func(c pahomqtt.Client) {
		logger.Info("mqtt connected", "broker", cfg.Broker)
		w.writeLog("", fmt.Sprintf("[capture] connected broker=%s topic=%s", cfg.Broker, topic))
		if tok := c.Subscribe(topic, 1, func(_ pahomqtt.Client, msg pahomqtt.Message) {
			w.writeLog(msg.Topic(), string(msg.Payload()))
		}); tok.Wait() && tok.Error() != nil {
			logger.Error("subscribe failed", "err", tok.Error())
		}
	})
	if cfg.User != "" {
		opts.SetUsername(cfg.User)
	}
	if cfg.Password != "" {
		opts.SetPassword(cfg.Password)
	}

	client := pahomqtt.NewClient(opts)
	if err := sharedmqtt.Connect(ctx, client); err != nil {
		return fmt.Errorf("mqtt connect: %w", err)
	}
	defer client.Disconnect(250)

	<-ctx.Done()
	w.writeLog("", "[capture] shutting down")
	return nil
}

// rotatingWriter writes one timestamped line per MQTT message to an hourly log file.
type rotatingWriter struct {
	logDir         string
	retentionHours int

	mu   sync.Mutex
	file *os.File
	bw   *bufio.Writer
	hour time.Time // truncated to hour
}

func newRotatingWriter(logDir string, retentionHours int) (*rotatingWriter, error) {
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil, fmt.Errorf("create log dir: %w", err)
	}
	w := &rotatingWriter{
		logDir:         logDir,
		retentionHours: retentionHours,
	}
	if err := w.rotateIfNeeded(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *rotatingWriter) writeLog(topic, payload string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if err := w.rotateIfNeeded(); err != nil {
		// Best-effort: log to stderr and keep going; do not drop the message.
		fmt.Fprintf(os.Stderr, "rotate failed: %v\n", err)
	}

	ts := time.Now().UTC().Format(time.RFC3339Nano)
	if topic == "" {
		_, _ = fmt.Fprintf(w.bw, "%s %s\n", ts, payload)
	} else {
		_, _ = fmt.Fprintf(w.bw, "%s %s %s\n", ts, topic, payload)
	}
	_ = w.bw.Flush()
}

func (w *rotatingWriter) rotateIfNeeded() error {
	now := time.Now().UTC().Truncate(time.Hour)
	if w.hour.Equal(now) && w.file != nil {
		return nil
	}
	if w.file != nil {
		_ = w.bw.Flush()
		_ = w.file.Close()
		w.file = nil
		w.bw = nil
	}
	path := filepath.Join(w.logDir, now.Format("2006-01-02"), now.Format("15")+".log")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create log subdir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open log file %s: %w", path, err)
	}
	w.file = f
	w.bw = bufio.NewWriter(f)
	w.hour = now
	w.cleanupOldLogs()
	return nil
}

func (w *rotatingWriter) cleanupOldLogs() {
	cutoff := time.Now().UTC().Add(-time.Duration(w.retentionHours) * time.Hour).Truncate(time.Hour)
	entries, err := os.ReadDir(w.logDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		d, err := time.Parse("2006-01-02", e.Name())
		if err != nil {
			continue
		}
		if d.Before(cutoff.Truncate(24 * time.Hour)) {
			_ = os.RemoveAll(filepath.Join(w.logDir, e.Name()))
		}
	}
}

func (w *rotatingWriter) close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.bw != nil {
		_ = w.bw.Flush()
	}
	if w.file != nil {
		return w.file.Close()
	}
	return nil
}
