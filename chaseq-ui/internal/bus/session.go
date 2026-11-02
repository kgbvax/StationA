package bus

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	pahomqtt "github.com/eclipse/paho.mqtt.golang"

	sharedmqtt "codeberg.org/kgbvax/stationa/shared/mqtt"
)

// Topics are the topics a Session uses.
type Topics struct {
	ChaseState, ChaseStatus, CQFinderState, RadioState string
}

// Handlers receive decoded messages. They run on the Session's jobs worker,
// never on paho's dispatch goroutine, and must not block for long.
type Handlers struct {
	Chase    func(ChaseState)
	Status   func(online bool)
	CQFinder func(CQFinderState)
	Radio    func(RadioState)
}

// Options configure a Session.
type Options struct {
	Broker, User, Password, ClientID string
	Topics                           Topics
	Handlers                         Handlers
	Log                              *slog.Logger
}

// Session is the broker connection.
type Session struct {
	opt    Options
	client pahomqtt.Client
	jobs   chan func()
}

// NewSession builds (does not connect) a Session. The password never reaches
// a log line.
func NewSession(o Options) *Session {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	s := &Session{opt: o, jobs: make(chan func(), 64)}
	co := pahomqtt.NewClientOptions().
		AddBroker(o.Broker).
		SetClientID(o.ClientID).
		SetCleanSession(true).
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetConnectRetryInterval(5 * time.Second).
		SetKeepAlive(30 * time.Second).
		SetOnConnectHandler(s.onConnect)
	if o.User != "" {
		co.SetUsername(o.User).SetPassword(o.Password)
	}
	s.client = pahomqtt.NewClient(co)
	return s
}

// Run connects and processes messages until ctx is done.
func (s *Session) Run(ctx context.Context) error {
	go sharedmqtt.RunJobs(ctx, s.jobs)
	if err := sharedmqtt.Connect(ctx, s.client); err != nil {
		return fmt.Errorf("mqtt connect: %w", err)
	}
	<-ctx.Done()
	s.client.Disconnect(250)
	return nil
}

func (s *Session) onConnect(c pahomqtt.Client) {
	t := s.opt.Topics
	sub := func(topic string, f func([]byte)) {
		tok := c.Subscribe(topic, 1, func(_ pahomqtt.Client, m pahomqtt.Message) {
			p := append([]byte(nil), m.Payload()...)
			sharedmqtt.Enqueue(s.jobs, func() { f(p) })
		})
		go func() {
			tok.Wait()
			if err := tok.Error(); err != nil {
				s.opt.Log.Warn("subscribe failed", "topic", topic, "err", err)
			}
		}()
	}
	h := s.opt.Handlers
	sub(t.ChaseState, func(p []byte) {
		if st, err := DecodeChase(p); err != nil {
			s.opt.Log.Warn("bad payload", "err", err)
		} else if h.Chase != nil {
			h.Chase(st)
		}
	})
	sub(t.ChaseStatus, func(p []byte) {
		if h.Status != nil {
			h.Status(string(p) == "online")
		}
	})
	sub(t.CQFinderState, func(p []byte) {
		if st, err := DecodeCQFinder(p); err != nil {
			s.opt.Log.Warn("bad payload", "err", err)
		} else if h.CQFinder != nil {
			h.CQFinder(st)
		}
	})
	sub(t.RadioState, func(p []byte) {
		if st, err := DecodeRadio(p); err != nil {
			s.opt.Log.Warn("bad payload", "err", err)
		} else if h.Radio != nil {
			h.Radio(st)
		}
	})
}

// Publish sends a non-retained message at QoS 1. It blocks up to 3 s, so call
// it from the action goroutine, never from a Handlers callback.
func (s *Session) Publish(topic string, payload []byte) error {
	tok := s.client.Publish(topic, 1, false, payload)
	if !tok.WaitTimeout(3 * time.Second) {
		return fmt.Errorf("publish %s: timeout", topic)
	}
	return tok.Error()
}
