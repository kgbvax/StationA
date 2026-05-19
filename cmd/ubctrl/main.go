package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"time"

	mqttbridge "ubctrl/internal/mqtt"
	"ubctrl/internal/ub/service"
	"ubctrl/internal/ub/transport"
	"ubctrl/internal/web"
)

func main() {
	httpAddr := flag.String("http", "127.0.0.1:8080", "HTTP listen address")
	port := flag.String("port", "", "Serial port path (leave empty to use mock)")
	baud := flag.Int("baud", 19200, "Serial baud rate")
	mqttBroker := flag.String("mqtt-broker", "", "MQTT broker URL (optional)")
	mqttClientID := flag.String("mqtt-client-id", "ubctrl", "MQTT client ID")
	mqttPrefix := flag.String("mqtt-prefix", "ubctrl", "MQTT topic prefix")
	mqttUser := flag.String("mqtt-user", "", "MQTT username (optional)")
	mqttPassword := flag.String("mqtt-password", "", "MQTT password (optional)")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var dev transport.Client
	var err error
	if *port == "" {
		dev = transport.NewMock()
	} else {
		dev, err = transport.OpenSerial(*port, *baud)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	ctrl := service.NewController(dev)
	ui := web.New(ctrl)

	var mqttClient *mqttbridge.Client
	if *mqttBroker != "" {
		mqttClient, err = mqttbridge.New(*mqttBroker, *mqttClientID, *mqttPrefix, *mqttUser, *mqttPassword, ctrl)
		if err != nil {
			fmt.Fprintf(os.Stderr, "mqtt disabled: %v\n", err)
		} else {
			mqttClient.PublishDiscovery()
			mqttClient.BindCommands(ctx)
		}
	}

	pollCtx, cancelPoll := context.WithCancel(ctx)
	defer cancelPoll()
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-pollCtx.Done():
				return
			case <-ticker.C:
				_ = ctrl.Refresh(pollCtx)
				_ = ctrl.PollMotorStatus(pollCtx)
				ui.PublishStatus(ctrl.State())
				if mqttClient != nil {
					mqttClient.PublishState(ctrl.State())
				}
			}
		}
	}()

	if err := ctrl.Refresh(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "initial refresh failed: %v\n", err)
	}
	ui.PublishStatus(ctrl.State())
	if mqttClient != nil {
		mqttClient.PublishState(ctrl.State())
	}

	srv := &http.Server{
		Addr:    *httpAddr,
		Handler: ui.Routes(),
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		if mqttClient != nil {
			mqttClient.Close()
		}
	}()

	fmt.Printf("ubctrl listening on http://%s\n", *httpAddr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
