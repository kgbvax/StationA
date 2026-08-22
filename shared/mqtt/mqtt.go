// Package mqtt holds the foundational MQTT plumbing shared by every stationa
// bridge and logic component. It is a thin layer each module wraps; it is NOT a
// shared client. The two pieces here exist to centralize two paho foot-guns that
// have bitten stationa bridges live (recorded in the stationa memory):
//
//   - Connect is ctx-aware. paho's Client.Connect().Wait() blocks ignoring the
//     caller's context, so a SIGTERM issued while the broker is unreachable
//     cannot interrupt the connect and systemd must SIGKILL the unit after
//     TimeoutStopSec. Connect bridges the wait through a goroutine + select on
//     ctx.Done so shutdown resolves promptly. (Hit acom1200s-pa-bridge live;
//     flexbridge was latent.)
//   - Enqueue + RunJobs keep paho message handlers off the publish path. A paho
//     handler runs on paho's dispatch goroutine and must not call a blocking
//     Publish — that deadlocks the dispatch loop. Handlers instead Enqueue a
//     closure onto a bounded jobs channel; a single RunJobs worker serializes
//     state mutation + publishing. (Deadlocked hadiscovery live; antennaselect
//     would have on deploy.)
package mqtt

import (
	"context"

	pahomqtt "github.com/eclipse/paho.mqtt.golang"
)

// Connect establishes the MQTT connection, ctx-aware. It starts paho's Connect
// token, waits for it on a separate goroutine, and selects against ctx.Done so a
// cancelled context interrupts the connect (paho's Wait() alone does not). On
// ctx cancellation the client is disconnected and ctx.Err() is returned. On
// connect failure the client is disconnected and the token error returned.
func Connect(ctx context.Context, client pahomqtt.Client) error {
	tok := client.Connect()
	waitErr := make(chan error, 1)
	go func() {
		tok.Wait()
		waitErr <- tok.Error()
	}()
	select {
	case err := <-waitErr:
		if err != nil {
			client.Disconnect(0)
			return err
		}
	case <-ctx.Done():
		client.Disconnect(0)
		return ctx.Err()
	}
	return nil
}

// Enqueue runs f on the jobs worker; non-blocking: if the worker is saturated it
// drops the job rather than block paho's dispatch goroutine. Paho message
// handlers must never block — call Enqueue from a handler, never Publish.
func Enqueue(jobs chan<- func(), f func()) {
	select {
	case jobs <- f:
	default:
		// Buffer full: drop. The next native announce / cmd re-arms once the
		// worker drains. Preferred over blocking paho's dispatch goroutine.
	}
}

// RunJobs drains jobs and runs each closure on the calling goroutine until ctx
// is done or the jobs channel is closed. The caller owns the jobs channel and
// may close it to stop the worker; closing is optional — cancelling ctx also
// stops it. All closures for one slot/client should share one jobs channel +
// one RunJobs worker so state mutation and publishing are serialized.
//
// Receiving with comma-ok (not a bare receive) is load-bearing: a closed
// channel yields nil values forever, and calling a nil closure panics. The
// ok=false return lets the worker exit cleanly on close instead of panicking
// — a real failure mode when a caller closes the channel on an early return
// that is NOT a ctx cancellation (e.g. a Connect failure), so the worker must
// not assume ctx.Done() is the only exit.
func RunJobs(ctx context.Context, jobs <-chan func()) {
	for {
		select {
		case <-ctx.Done():
			return
		case f, ok := <-jobs:
			if !ok {
				return // channel closed; the worker is done
			}
			f()
		}
	}
}
