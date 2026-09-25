// SPDX-License-Identifier: AGPL-3.0-or-later

// Radio-audio source for the preview: receives the IC-9700's demodulated
// audio (S16LE 48 kHz mono UDP datagrams from icom9700-radio-bridge) and
// re-publishes it on loopback TCP for the preview ffmpeg.
//
// The silence fill is load-bearing: ffmpeg interleaves inputs, so a quiet
// UDP feed would stall the whole preview (video included) whenever the radio
// session is down. This source therefore ALWAYS writes at a 10 ms cadence —
// real PCM when datagrams arrive, zeros otherwise — making the radio audio
// a constant-rate input the muxer can rely on.
package preview

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// tickBytes is 10 ms of S16LE mono at 48 kHz.
	audioSampleRate = 48000
	tickInterval    = 10 * time.Millisecond
	tickBytes       = audioSampleRate * 2 / 100
	// maxBuffer bounds the jitter buffer (500 ms); older bytes are dropped —
	// a preview prefers fresh silence over stale audio.
	maxBuffer = audioSampleRate * 2 / 2
)

// AudioSourceStatus tracks whether real radio PCM is arriving (silence-fill
// does not count — zeros are written locally without touching this).
type AudioSourceStatus struct {
	lastRx atomic.Int64 // unix nanos of the last received datagram
}

// Alive reports whether a radio-audio datagram arrived recently.
func (s *AudioSourceStatus) Alive() bool {
	ts := s.lastRx.Load()
	return ts != 0 && time.Since(time.Unix(0, ts)) < 3*time.Second
}

// StartAudioSource binds udpAddr (bridge PCM in), listens on tcpAddr (ffmpeg
// inputs — multiple allowed: every sink ffmpeg gets its own connection) and
// pumps a constant-rate stream to each. Runs until ctx is done; UDP errors
// are logged and retried. status (may be nil) is stamped on every real
// datagram.
func StartAudioSource(ctx context.Context, udpAddr, tcpAddr string, status *AudioSourceStatus, log interface{ Warn(string, ...any) }) error {
	udp, err := net.ListenPacket("udp", udpAddr)
	if err != nil {
		return err
	}
	defer udp.Close()
	ln, err := net.Listen("tcp", tcpAddr)
	if err != nil {
		return err
	}
	defer ln.Close()
	log.Warn("radio audio source listening", "udp", udpAddr, "tcp", tcpAddr)

	// PCM inbox: datagrams land here; the writer drains at its cadence.
	var mu sync.Mutex
	var buf []byte

	go func() {
		buf2 := make([]byte, 2048)
		for {
			n, _, err := udp.ReadFrom(buf2)
			if err != nil {
				select {
				case <-ctx.Done():
					return
				default:
				}
				log.Warn("radio audio udp read", "err", err)
				time.Sleep(time.Second)
				continue
			}
			mu.Lock()
			buf = append(buf, buf2[:n]...)
			if len(buf) > maxBuffer {
				buf = buf[len(buf)-maxBuffer:]
			}
			mu.Unlock()
			if status != nil {
				status.lastRx.Store(time.Now().UnixNano())
			}
		}
	}()

	// Accept loop: multiple ffmpeg inputs are clients here (the YouTube sink
	// and the preview both read the radio audio) — each connection is
	// registered and fanned out to.
	clients := make(map[net.Conn]struct{})
	var cmu sync.Mutex
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				select {
				case <-ctx.Done():
					return
				default:
				}
				time.Sleep(time.Second)
				continue
			}
			cmu.Lock()
			clients[c] = struct{}{}
			cmu.Unlock()
			log.Warn("radio audio client connected", "remote", c.RemoteAddr().String())
		}
	}()

	silence := make([]byte, tickBytes) // zeros
	t := time.NewTicker(tickInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			cmu.Lock()
			for c := range clients {
				c.Close()
			}
			cmu.Unlock()
			return nil
		case <-t.C:
			mu.Lock()
			n := len(buf)
			if n > tickBytes {
				n = tickBytes
			}
			var out []byte
			if n > 0 {
				out = buf[:n]
				buf = buf[n:]
			}
			mu.Unlock()
			if n < tickBytes {
				out = append(append([]byte{}, out...), silence[:tickBytes-n]...)
			}
			cmu.Lock()
			for c := range clients {
				if _, err := c.Write(out); err != nil {
					delete(clients, c)
					c.Close()
				}
			}
			cmu.Unlock()
		}
	}
}
