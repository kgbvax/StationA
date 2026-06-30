package transport

import (
	"context"
	"time"

	"ubctrl/internal/ub/protocol"
)

// Trace is a single observed exchange direction: a request sent to the device
// ("tx") or the reply/result received from it ("rx").
type Trace struct {
	At   time.Time
	Dir  string // "tx" or "rx"
	Com  byte   // command byte (tx) or reply byte (rx)
	Data []byte // payload (request data for tx, reply data for rx)
	Err  string // non-empty on rx when the exchange failed
}

// TraceSink receives Trace entries. It must be safe for concurrent use.
type TraceSink func(Trace)

// tracingClient decorates a Client and emits a tx Trace before each exchange
// and an rx Trace after, but only while enabled() reports true. It works for
// any Client (serial Device or mock), so debug streaming is transport-agnostic.
type tracingClient struct {
	inner   Client
	enabled func() bool
	sink    TraceSink
}

// NewTracing wraps inner so that exchanges are reported to sink whenever
// enabled() returns true. When enabled() is false the wrapper adds no work
// beyond the predicate check. enabled and sink must be non-nil.
func NewTracing(inner Client, enabled func() bool, sink TraceSink) Client {
	return &tracingClient{inner: inner, enabled: enabled, sink: sink}
}

func (t *tracingClient) Exchange(ctx context.Context, com byte, data []byte, timeout time.Duration) (protocol.Packet, error) {
	on := t.enabled()
	if on {
		t.sink(Trace{At: time.Now(), Dir: "tx", Com: com, Data: cloneBytes(data)})
	}
	pkt, err := t.inner.Exchange(ctx, com, data, timeout)
	if on {
		entry := Trace{At: time.Now(), Dir: "rx", Com: pkt.Com, Data: cloneBytes(pkt.Data)}
		if err != nil {
			entry.Err = err.Error()
		}
		t.sink(entry)
	}
	return pkt, err
}

func (t *tracingClient) Close() error { return t.inner.Close() }

func cloneBytes(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}
