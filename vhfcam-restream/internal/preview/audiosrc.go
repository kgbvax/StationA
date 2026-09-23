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

// StartAudioSource binds udpAddr (bridge PCM in), listens on tcpAddr (the
// ffmpeg input, one client — newest wins) and pumps a constant-rate stream.
// Runs until ctx is done; UDP errors are logged and retried. status (may be
// nil) is stamped on every real datagram.
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

	// Accept loop: one connected ffmpeg at a time — a new client replaces
	// the old one (the preview restarts on every config reload).
	connCh := make(chan net.Conn, 1)
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
			for {
				select {
				case old := <-connCh:
					old.Close()
				default:
				}
				break
			}
			connCh <- c
			log.Warn("radio audio client connected", "remote", c.RemoteAddr().String())
		}
	}()

	silence := make([]byte, tickBytes) // zeros
	t := time.NewTicker(tickInterval)
	defer t.Stop()
	var client net.Conn
	for {
		select {
		case <-ctx.Done():
			if client != nil {
				client.Close()
			}
			return nil
		case c := <-connCh:
			if client != nil {
				client.Close()
			}
			client = c
		case <-t.C:
			if client == nil {
				continue
			}
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
			if _, err := client.Write(out); err != nil {
				client.Close()
				client = nil
			}
		}
	}
}
