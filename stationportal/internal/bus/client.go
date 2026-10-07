package bus

import (
	"context"
	"log/slog"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	smqtt "codeberg.org/kgbvax/stationa/shared/mqtt"
)

// Options configure the bus connection.
type Options struct {
	Broker, ClientID, User, Password string
	Site                             string
}

// retryDelays is the initial-connect backoff; the last value repeats. A var
// so tests can compress it.
var retryDelays = []time.Duration{2 * time.Second, 5 * time.Second, 15 * time.Second, 30 * time.Second}

// Run connects as a passive consumer (no LWT, no publishes), subscribes
// <site>/# and feeds the store until ctx is done. A broker that is down at
// startup is retried with backoff — the page keeps serving the static
// inventory meanwhile; later drops are handled by paho's auto-reconnect.
func Run(ctx context.Context, o Options, store *Store, log *slog.Logger) {
	jobs := make(chan func(), 4096)
	go smqtt.RunJobs(ctx, jobs)

	topic := o.Site + "/#"
	handler := func(_ paho.Client, msg paho.Message) {
		// paho's buffer is only valid inside the handler; the update runs on
		// the jobs worker, never on paho's dispatch goroutine.
		payload := append([]byte(nil), msg.Payload()...)
		t, now := msg.Topic(), time.Now()
		smqtt.Enqueue(jobs, func() { store.Update(t, payload, now) })
	}

	opts := paho.NewClientOptions().
		AddBroker(o.Broker).
		SetClientID(o.ClientID).
		SetCleanSession(true). // retained planes replay on every subscribe
		SetAutoReconnect(true).
		SetMaxReconnectInterval(30 * time.Second).
		SetConnectionLostHandler(func(_ paho.Client, err error) {
			store.SetConnected(false, time.Now())
			log.Warn("mqtt connection lost", "err", err)
		}).
		SetOnConnectHandler(func(c paho.Client) {
			store.SetConnected(true, time.Now())
			log.Info("mqtt connected", "broker", o.Broker, "sub", topic)
			if tok := c.Subscribe(topic, 0, handler); tok.Wait() && tok.Error() != nil {
				log.Error("mqtt subscribe failed", "topic", topic, "err", tok.Error())
			}
		})
	if o.User != "" {
		opts.SetUsername(o.User)
	}
	if o.Password != "" {
		opts.SetPassword(o.Password)
	}
	client := paho.NewClient(opts)

	for attempt := 0; ; attempt++ {
		err := smqtt.Connect(ctx, client)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return
		}
		d := retryDelays[min(attempt, len(retryDelays)-1)]
		log.Warn("mqtt connect failed — retrying", "broker", o.Broker, "err", err, "in", d)
		select {
		case <-ctx.Done():
			return
		case <-time.After(d):
		}
	}

	<-ctx.Done()
	client.Disconnect(250)
	store.SetConnected(false, time.Now())
}
