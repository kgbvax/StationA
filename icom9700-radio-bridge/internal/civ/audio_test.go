// SPDX-License-Identifier: AGPL-3.0-or-later

package civ

import (
	"encoding/binary"
	"net"
	"testing"
	"time"
)

// audioStub answers the audio stream's start handshake and streams PCM
// datagrams to whoever talked to it (the client's audio socket).
type audioStub struct {
	conn *net.UDPConn
}

func startAudioStub(t *testing.T) *audioStub {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			pkt := make([]byte, n)
			copy(pkt, buf[:n])
			var reply []byte
			switch {
			case prefixEqual(pkt, sigAreYouThere):
				reply = header(sigIAmHere, 0x1234, binary.BigEndian.Uint32(pkt[8:12]))
			case prefixEqual(pkt, sigReady):
				reply = header(sigReady, 0x1234, binary.BigEndian.Uint32(pkt[8:12]))
			case isPing(pkt) && pkt[16] == pingRequest:
				seq, id := parsePingRequest(pkt)
				reply = buildPing(0x1234, binary.BigEndian.Uint32(pkt[12:16]), seq, id)
			default:
				continue
			}
			_, _ = conn.WriteToUDP(reply, from)
		}
	}()
	return &audioStub{conn: conn}
}

// sendAudio streams one audio datagram (sig + seq + PCM) to dst.
func (s *audioStub) sendAudio(dst *net.UDPAddr, seq uint16, pcm []byte) {
	p := make([]byte, audioHeaderLen+len(pcm))
	copy(p, sigAudioMain)
	binary.LittleEndian.PutUint16(p[6:8], seq)
	p[16] = 0x80
	binary.BigEndian.PutUint16(p[18:20], seq-1)
	binary.BigEndian.PutUint16(p[22:24], uint16(len(pcm)))
	copy(p[audioHeaderLen:], pcm)
	_, _ = s.conn.WriteToUDP(p, dst)
}

func TestOpenAudioReceivesPCM(t *testing.T) {
	f := NewFakeRadio(t)
	stub := startAudioStub(t)

	o := fastOptions(f)
	o.AudioPort = stub.conn.LocalAddr().(*net.UDPAddr).Port
	cli := dialFake(t, f, o)

	if err := cli.OpenAudio(); err != nil {
		t.Fatalf("OpenAudio: %v", err)
	}
	// Idempotent open.
	if err := cli.OpenAudio(); err != nil {
		t.Fatalf("second OpenAudio: %v", err)
	}

	dst := cli.audio.conn.LocalAddr().(*net.UDPAddr)
	pcm1 := make([]byte, 556)
	for i := range pcm1 {
		pcm1[i] = byte(i)
	}
	pcm2 := make([]byte, 1364)
	stub.sendAudio(dst, 1, pcm1)
	stub.sendAudio(dst, 2, pcm2)

	for i, want := range [][]byte{pcm1, pcm2} {
		select {
		case got := <-cli.AudioFrames():
			if string(got) != string(want) {
				t.Errorf("pcm chunk %d: %d bytes, want %d", i, len(got), len(want))
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for pcm chunk %d", i)
		}
	}

	// A gap (seq 3 dropped, 4 arrives) must trigger a retransmit request and
	// still deliver after the buffer's lock timeout.
	stub.sendAudio(dst, 4, pcm1)
	select {
	case got := <-cli.AudioFrames():
		if string(got) != string(pcm1) {
			t.Error("post-gap chunk content")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for post-gap chunk")
	}

	// CloseAudio closes the frames channel; AudioFrames reports not-open.
	cli.CloseAudio()
	if ch := cli.AudioFrames(); ch != nil {
		select {
		case _, ok := <-ch:
			if ok {
				t.Error("AudioFrames still delivering after CloseAudio")
			}
		default:
			t.Error("AudioFrames open (not closed) after CloseAudio")
		}
	}
	if cli.audio != nil {
		t.Error("audio stream not cleared after CloseAudio")
	}
}

func TestOpenAudioFailureLeavesSessionAlive(t *testing.T) {
	// AudioPort pointing at a closed socket: OpenAudio fails (handshake
	// timeout), and the control/CI-V session keeps working.
	f := NewFakeRadio(t)
	o := fastOptions(f)
	o.AudioPort = 1 // nothing listens on port 1
	cli := dialFake(t, f, o)

	if err := cli.OpenAudio(); err == nil {
		t.Fatal("OpenAudio succeeded against a dead port")
	}

	// The CI-V session still answers: SendCIV a frame and expect it echoed
	// back through the fake's civ stream (the fake transmits frames back on
	// demand — use the recorded traffic instead: a lost session would have
	// closed Frames).
	select {
	case err := <-cli.Lost:
		t.Fatalf("session lost after audio failure: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := cli.SendCIV([]byte{0x14, 0x0a}); err != nil {
		t.Errorf("SendCIV after audio failure: %v", err)
	}
}
