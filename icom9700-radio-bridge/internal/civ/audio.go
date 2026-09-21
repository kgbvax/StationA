// Audio receive stream (radio :50003 -> client): RX-only pull of the radio's
// demodulated audio as raw S16LE 48 kHz mono PCM.
//
// Wire facts (kappanhang audiostream.go — the only reference that opens this
// stream; wfview's audiohandler agrees on the frame layout):
//   - The stream needs the same pkt3/pkt4/pkt6 start handshake as the CI-V
//     data stream and NO open packet afterwards.
//   - Keepalive is the ping line only — no pkt0 idles on this stream.
//   - Audio datagrams carry a 24-byte header (16-byte standard header, then
//     0x80 0x00, the echoed seq-1 big-endian, 0x00 0x00, big-endian payload
//     length); payloads are raw PCM. Two family prefixes exist (0x6c 0x05 and
//     0x44 0x02); the sequence number rides [6..7] little-endian as on every
//     stream. Payloads on real firmware are 556 or 1364 bytes.
//   - The radio streams continuously once the handshake is done — squelch
//     silence is zero-filled audio, not a packet gap.
//
// RX-only by design: TX-side audio (client -> radio) is out of scope until
// the unattended-TX gates close (see the icom9700-radio-bridge CLAUDE.md).
package civ

import (
	"context"
	"errors"
	"time"
)

const (
	// audioHeaderLen strips to the raw PCM payload (kappanhang rxSeqBuf.add
	// stores the datagram and the player consumes r[24:]).
	audioHeaderLen = 24
	// audioMinDatagram is kappanhang's minimum audio-datagram size gate.
	audioMinDatagram = 580
	// audioSilenceAfter warns when the radio stopped streaming audio; NOT
	// session loss (the control stream owns liveness).
	audioSilenceAfter = 5 * time.Second
)

var (
	// sigAudioMain / sigAudioSub are the two audio-data family prefixes.
	sigAudioMain = []byte{0x6c, 0x05, 0x00, 0x00, 0x00, 0x00}
	sigAudioSub  = []byte{0x44, 0x02, 0x00, 0x00, 0x00, 0x00}
)

// isAudioData reports whether r is an audio PCM datagram.
func isAudioData(r []byte) bool {
	return len(r) >= audioMinDatagram &&
		(prefixEqual(r, sigAudioMain) || prefixEqual(r, sigAudioSub))
}

// OpenAudio opens the audio receive stream on a live session (idempotent).
// The radio starts streaming demodulated audio once the stream's start
// handshake completes; PCM chunks arrive on AudioFrames. A failure here is
// contained to audio — the control/CI-V session is unaffected (callers log
// it; the next demand retries). The handshake deliberately runs WITHOUT
// wmu: holding it for the full handshake budget would starve the control
// stream's keepalives and trip the loss watchdog.
func (c *Client) OpenAudio() error {
	c.wmu.Lock()
	if c.audio != nil {
		c.wmu.Unlock()
		return nil
	}
	c.wmu.Unlock()

	a, err := c.dialStream("audio", c.opts.AudioPort, c.opts.BindAudio)
	if err != nil {
		return err
	}
	a.lossIsolated.Store(true) // a dead audio port never ends the session
	a.startReader()
	ctx, cancel := context.WithTimeout(context.Background(), c.opts.HandshakeBudget)
	defer cancel()
	if err := a.start(ctx); err != nil {
		a.quiet.Store(true)
		close(a.closed)
		_ = a.conn.Close()
		return c.hsErr(err)
	}

	c.wmu.Lock()
	if c.audio != nil { // raced with a concurrent open
		a.quiet.Store(true)
		close(a.closed)
		_ = a.conn.Close()
		c.wmu.Unlock()
		return nil
	}
	c.audio = a
	c.audioFrames = make(chan []byte, 64)
	a.rx = newRxSeqBuf(c.opts.RxBuffer, readQueueLen, a.requestRetransmit)
	c.wmu.Unlock()
	c.startPingOnly(a)
	go c.audioPump(a)
	c.log.Info("audio stream open")
	return nil
}

// CloseAudio closes the audio receive stream (idempotent, safe when never
// opened). The AudioFrames channel closes with it.
func (c *Client) CloseAudio() {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.closeAudioLocked()
}

func (c *Client) closeAudioLocked() {
	if c.audio == nil {
		return
	}
	a := c.audio
	c.audio = nil
	// Quiet close: the reader must not report a loss for our own teardown.
	a.quiet.Store(true)
	close(a.closed) // wake the pump for teardown
	_ = a.conn.Close()
	if c.audioFrames != nil {
		close(c.audioFrames)
		c.audioFrames = nil
	}
}

// AudioFrames returns the PCM chunk channel (nil when the audio stream is
// not open; the channel closes when the stream closes).
func (c *Client) AudioFrames() <-chan []byte {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.audioFrames
}

// startPingOnly runs the audio stream's keepalive: the ping line only —
// kappanhang's audio stream sends no pkt0 idles.
func (c *Client) startPingOnly(s *udpStream) {
	go func() {
		var pingID [4]byte
		pingID[3] = 0x06 // kappanhang's ping-ID family tag
		inner := uint16(0x8304)
		seq := uint16(1)
		t := time.NewTicker(c.opts.PingInterval)
		defer t.Stop()
		for {
			select {
			case <-c.done:
				return
			case <-t.C:
				c.wmu.Lock()
				if c.audio != s { // stream was closed/reopened — stop pinging
					c.wmu.Unlock()
					return
				}
				p := buildPing(s.localSID, s.remoteSID, seq, nil)
				pingID[1] = byte(inner)
				pingID[2] = byte(inner >> 8)
				copy(p[17:21], pingID[:])
				inner++
				seq++
				c.wmu.Unlock()
				_ = s.send(p)
			}
		}
	}()
}

// audioPump reorders audio datagrams and delivers raw PCM chunks (headers
// stripped) to AudioFrames. Same reorder/retransmit mechanics as civPump;
// a packet starvation longer than audioSilenceAfter is logged once per gap.
func (c *Client) audioPump(a *udpStream) {
	defer c.closeAudioPump(a)
	lastPacket := time.Now()
	warned := false
	for {
		e, retryIn, err := a.rx.next()
		switch {
		case err == nil && retryIn == 0:
			if isAudioData(e.data) {
				lastPacket = time.Now()
				warned = false
				c.deliverAudio(a, e.data[audioHeaderLen:])
			}
			continue // idles/strays occupy sequence space too
		case errors.Is(err, errRxOutOfOrder):
			continue
		}
		var lock *time.Timer
		if retryIn > 0 {
			lock = time.NewTimer(retryIn)
		}
		if lock != nil {
			select {
			case pkt := <-a.readCh:
				lock.Stop()
				c.feedAudio(a, pkt)
			case <-lock.C:
			case <-a.closed:
				return
			case <-c.done:
				return
			}
			continue
		}
		select {
		case pkt := <-a.readCh:
			c.feedAudio(a, pkt)
		case <-a.closed:
			return
		case <-c.done:
			return
		case <-time.After(audioSilenceAfter):
			if !warned && time.Since(lastPacket) >= audioSilenceAfter {
				warned = true
				c.log.Warn("audio stream silent", "for", time.Since(lastPacket).Truncate(time.Second))
			}
		}
	}
}

// feedAudio adds one audio datagram to the reorder buffer; non-audio
// datagrams (strays) are dropped.
func (c *Client) feedAudio(a *udpStream, pkt []byte) {
	if isAudioData(pkt) {
		a.rx.add(dataSeq(pkt), pkt)
	}
}

// deliverAudio hands one PCM chunk to AudioFrames, newest-wins on a full
// queue (audio wants the freshest samples, like the frame queue).
func (c *Client) deliverAudio(a *udpStream, pcm []byte) {
	c.wmu.Lock()
	ch := c.audioFrames
	open := c.audio == a
	c.wmu.Unlock()
	if !open || ch == nil {
		return
	}
	select {
	case ch <- pcm:
	default:
		select {
		case <-ch:
		default:
		}
		select {
		case ch <- pcm:
		default:
		}
	}
}

// closeAudioPump tears the audio stream down after the pump exits (session
// loss path — CloseAudio/Clean Close handled their own quiet teardown).
func (c *Client) closeAudioPump(a *udpStream) {
	c.wmu.Lock()
	if c.audio == a {
		c.audio = nil
		if c.audioFrames != nil {
			close(c.audioFrames)
			c.audioFrames = nil
		}
	}
	c.wmu.Unlock()
	_ = a.conn.Close()
}
