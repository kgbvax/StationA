package civ

import (
	"encoding/binary"
	"net"
	"sync"
	"testing"
)

// fakeRadio is an in-process UDP server speaking the same packet grammar as
// the IC-9700 (kappanhang's controlstream/serialstream from the radio side),
// scripted per test: drop datagrams, refuse logins, stop answering, flood.
// It records every received packet for byte-exact wire assertions.
type fakeRadio struct {
	t *testing.T

	ctrl *net.UDPConn
	civ  *net.UDPConn

	mu          sync.Mutex
	cliCtrl     *net.UDPAddr
	cliCiv      *net.UDPAddr
	radioSID    uint32
	authID      [6]byte
	loginCount  int
	auth05Count int
	requested   bool
	civOpened   int
	civClosed   int
	renewed     int

	dropNext    int  // drop the next N control datagrams from the client
	refuseLogin bool // answer 0x60 with ff ff ff fe
	refuseSess  bool // answer the stream request with 0x50 ff ff ff
	silent      bool // stop answering entirely (session-loss rehearsal)

	// tx log of CI-V data packets sent to the client, by outer seq — the
	// retransmit-serve path for rx-gap tests.
	civTx      map[uint16][]byte
	civSendSeq uint16
	civInner   uint16

	// the client's civ-stream localSID, learned from its datagrams [8:12]
	cliRemoteSIDCiv uint32

	ctrlPackets [][]byte // every control datagram the client sent
	civPackets  [][]byte // every civ datagram the client sent

	civAddrKnown chan struct{}
}

func newFakeRadio(t *testing.T) *fakeRadio {
	t.Helper()
	f := &fakeRadio{
		t:            t,
		radioSID:     0x11223344,
		civTx:        map[uint16][]byte{},
		civAddrKnown: make(chan struct{}, 1),
	}
	copy(f.authID[:], []byte{0xde, 0xad, 0xbe, 0xef, 0x01, 0x02})

	var err error
	f.ctrl, err = net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen ctrl: %v", err)
	}
	f.civ, err = net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen civ: %v", err)
	}
	go f.serve(f.ctrl, "control")
	go f.serve(f.civ, "civ")
	t.Cleanup(func() {
		_ = f.ctrl.Close()
		_ = f.civ.Close()
	})
	return f
}

// addr returns the fake's control address for Dial.
func (f *fakeRadio) addr() *net.UDPAddr { return f.ctrl.LocalAddr().(*net.UDPAddr) }

// civAddr returns the fake's CI-V port.
func (f *fakeRadio) civPort() int { return f.civ.LocalAddr().(*net.UDPAddr).Port }

func (f *fakeRadio) serve(conn *net.UDPConn, stream string) {
	buf := make([]byte, 2048)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		pkt := make([]byte, n)
		copy(pkt, buf[:n])
		f.handle(stream, conn, from, pkt)
	}
}

func (f *fakeRadio) record(stream string, pkt []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if stream == "control" {
		f.ctrlPackets = append(f.ctrlPackets, pkt)
	} else {
		f.civPackets = append(f.civPackets, pkt)
	}
}

func (f *fakeRadio) counts() (logins, auth05, opens, closes, renewals int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loginCount, f.auth05Count, f.civOpened, f.civClosed, f.renewed
}

func (f *fakeRadio) ctrlLog() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]byte, len(f.ctrlPackets))
	copy(out, f.ctrlPackets)
	return out
}

func (f *fakeRadio) civLog() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]byte, len(f.civPackets))
	copy(out, f.civPackets)
	return out
}

func (f *fakeRadio) setSilent(v bool) {
	f.mu.Lock()
	f.silent = v
	f.mu.Unlock()
}

func (f *fakeRadio) handle(stream string, conn *net.UDPConn, from *net.UDPAddr, pkt []byte) {
	f.record(stream, pkt)

	f.mu.Lock()
	if stream == "control" {
		f.cliCtrl = from
	} else {
		if f.cliCiv == nil {
			select {
			case f.civAddrKnown <- struct{}{}:
			default:
			}
		}
		f.cliCiv = from
		f.cliRemoteSIDCiv = binary.BigEndian.Uint32(pkt[8:12])
	}
	if f.dropNext > 0 {
		f.dropNext--
		f.mu.Unlock()
		return
	}
	if f.silent {
		f.mu.Unlock()
		return
	}
	radioSID := f.radioSID
	authID := f.authID
	refuseLogin := f.refuseLogin
	refuseSess := f.refuseSess

	var reply []byte
	switch {
	case stream == "control" && prefixEqual(pkt, sigAreYouThere):
		reply = header(sigIAmHere, radioSID, binary.BigEndian.Uint32(pkt[8:12]))
	case stream == "control" && prefixEqual(pkt, sigReady):
		reply = header(sigReady, radioSID, binary.BigEndian.Uint32(pkt[8:12]))
	case stream == "civ" && prefixEqual(pkt, sigAreYouThere):
		reply = header(sigIAmHere, radioSID, binary.BigEndian.Uint32(pkt[8:12]))
	case stream == "civ" && prefixEqual(pkt, sigReady):
		reply = header(sigReady, radioSID, binary.BigEndian.Uint32(pkt[8:12]))
	case stream == "control" && len(pkt) == 128 && pkt[0] == 0x80:
		// Login answer: 96 bytes with the session auth ID (or the explicit
		// ff ff ff fe credential rejection).
		f.loginCount++
		ans := make([]byte, 96)
		copy(ans, sigLoginAnswer)
		binary.BigEndian.PutUint32(ans[8:12], radioSID)
		binary.BigEndian.PutUint32(ans[12:16], binary.BigEndian.Uint32(pkt[12:16]))
		if refuseLogin {
			ans[48], ans[49], ans[50], ans[51] = 0xff, 0xff, 0xff, 0xfe
		} else {
			copy(ans[authIDOffset:authIDOffset+authIDLen], authID[:])
		}
		reply = ans
	case stream == "control" && len(pkt) == 64 && pkt[0] == 0x40:
		// Auth (first 0x02, renewal/deauth 0x05 / 0x01): 64-byte answer.
		magic := pkt[offAuthMagic]
		if magic == 0x05 {
			f.auth05Count++
			if f.auth05Count >= 2 {
				f.renewed++
			}
			ans := make([]byte, 64)
			copy(ans, sigAuthAnswer)
			binary.BigEndian.PutUint32(ans[8:12], radioSID)
			binary.BigEndian.PutUint32(ans[12:16], binary.BigEndian.Uint32(pkt[12:16]))
			ans[offAuthMagic] = 0x05
			reply = ans
		}
		// After the FIRST auth the radio volunteers its 0xa8 identity.
		if magic == 0x02 && !refuseLogin {
			a8 := make([]byte, 168)
			a8[0] = 0xa8
			binary.BigEndian.PutUint32(a8[8:12], radioSID)
			copy(a8[66:82], []byte("radio-id-12345678"))
			f.mu.Unlock()
			_ = f.sendTo(conn, from, a8)
			return
		}
	case stream == "control" && pkt[0] == 0x90 && prefixEqual(pkt, sigRequestAnswer):
		// Request-stream.
		f.requested = true
		if refuseSess {
			fail := make([]byte, 80)
			fail[0] = 0x50
			binary.BigEndian.PutUint32(fail[8:12], radioSID)
			fail[48], fail[49], fail[50] = 0xff, 0xff, 0xff
			reply = fail
		} else {
			ans := make([]byte, 144)
			ans[0] = 0x90
			binary.BigEndian.PutUint32(ans[8:12], radioSID)
			binary.BigEndian.PutUint32(ans[12:16], binary.BigEndian.Uint32(pkt[12:16]))
			copy(ans[authIDOffset:authIDOffset+authIDLen], authID[:])
			ans[requestOKFlag] = 0x01
			copy(ans[requestDevNameOffset:], "IC-9700\x00")
			reply = ans
		}
	case isPing(pkt) && pkt[16] == pingRequest:
		ans := make([]byte, pingLen)
		copy(ans, []byte{0x15, 0x00, 0x00, 0x00, pingFamily, 0x00})
		binary.BigEndian.PutUint32(ans[8:12], radioSID)
		binary.BigEndian.PutUint32(ans[12:16], binary.BigEndian.Uint32(pkt[8:12]))
		binary.LittleEndian.PutUint16(ans[6:8], binary.LittleEndian.Uint16(pkt[6:8]))
		ans[16] = pingReplyFlg
		copy(ans[17:21], pkt[17:21])
		reply = ans
	case stream == "civ" && pkt[0] == 0x16 && len(pkt) == 22:
		if pkt[21] == 0x05 {
			f.civOpened++
		} else {
			f.civClosed++
		}
	case stream == "civ" && prefixEqual(pkt, sigRetransmitSingle):
		// Serve the retransmit from the fake's tx log — the radio-side
		// behavior the client's gap detection depends on.
		seq := binary.LittleEndian.Uint16(pkt[6:8])
		if testing.Verbose() {
			f.t.Logf("fake: retx-req seq=%d have=%v", seq, f.civTx[seq] != nil)
		}
		if d, ok := f.civTx[seq]; ok {
			f.mu.Unlock()
			_ = f.sendTo(f.civ, f.cliCiv, d)
			_ = f.sendTo(f.civ, f.cliCiv, d)
			return
		}
	case stream == "civ" && isData(pkt):
		// Tracked client CI-V data — recorded (the caller asserts payloads).
	}
	f.mu.Unlock()

	if reply != nil {
		_ = f.sendTo(conn, from, reply)
	}
}

func (f *fakeRadio) sendTo(conn *net.UDPConn, to *net.UDPAddr, pkt []byte) error {
	if to == nil {
		return nil
	}
	_, err := conn.WriteToUDP(pkt, to)
	return err
}

// sendCIVFrame sends one CI-V frame to the client as a tracked data packet,
// logging it for retransmit serving. With drop set, the frame is withheld
// until the client's retransmit request arrives (the fake serves its tx log
// on request — the rx-gap rehearsal).
func (f *fakeRadio) sendCIVFrame(payload []byte, drop bool) {
	f.mu.Lock()
	f.civSendSeq++
	seq := f.civSendSeq
	inner := f.civInner
	f.civInner++
	pkt := make([]byte, dataHeaderLen+len(payload))
	pkt[0] = byte(dataSubHeaderLen + len(payload))
	binary.LittleEndian.PutUint16(pkt[6:8], seq)
	binary.BigEndian.PutUint32(pkt[8:12], f.radioSID)
	binary.BigEndian.PutUint32(pkt[12:16], f.cliRemoteSIDCiv)
	pkt[16] = dataReplyFlag
	pkt[17] = byte(len(payload))
	binary.BigEndian.PutUint16(pkt[19:21], inner)
	copy(pkt[dataHeaderLen:], payload)
	f.civTx[seq] = pkt
	silent := f.silent
	to := f.cliCiv
	f.mu.Unlock()
	if !drop && !silent {
		_ = f.sendTo(f.civ, to, pkt)
	}
}
