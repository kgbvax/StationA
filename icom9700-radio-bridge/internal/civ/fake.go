package civ

import (
	"encoding/binary"
	"net"
	"sync"
	"testing"
	"time"
)

// FakeRadio is an in-process UDP server speaking the same packet grammar as
// the IC-9700 (kappanhang's controlstream/serialstream from the radio side),
// scripted per test: drop datagrams, refuse logins, stop answering, flood.
// It records every received packet for byte-exact wire assertions.
//
// It lives in the production package (not a _test file) so the radio
// session's tests (internal/radio) drive the same grammar — the spid
// package's in-package Mock is the stationa precedent. Nothing in the
// bridge references it; the linker drops it from release binaries.
type FakeRadio struct {
	t testing.TB

	ctrl *net.UDPConn
	civ  *net.UDPConn

	mu          sync.Mutex
	cliCtrl     *net.UDPAddr
	cliCiv      *net.UDPAddr
	radioSID    uint32
	authID      [6]byte
	loginCount  int
	loginTimes  []time.Time
	auth05Count int
	requested   bool
	civOpened   int
	civClosed   int
	renewed     int

	dropNext    int  // drop the next N control datagrams from the client
	refuseLogin bool // answer 0x60 with ff ff ff fe
	busyLogin   bool // answer the login with the 20-byte 81 ff ff ff busy reject
	refuseSess  bool // answer the stream request with 0x50 ff ff ff
	silent      bool // stop answering entirely (session-loss rehearsal)

	// tx log of CI-V data packets sent to the client, by outer seq — the
	// retransmit-serve path for rx-gap tests.
	civTx      map[uint16][]byte
	civSendSeq uint16
	civInner   uint16

	// the client's civ-stream localSID, learned from its datagrams [8:12]
	cliRemoteSIDCiv uint32

	// CI-V device state the bridge's reads poll (U5): per-VFO frequency and
	// mode, selected VFO, satellite mode, PTT, and the meters. SetFoo lets
	// tests move the "front panel"; the CI-V command handler mutates it.
	radioMu     sync.Mutex
	freq        map[string]uint64 // "main"/"sub" -> Hz
	mode        map[string]string
	selectedVFO string
	satMode     bool
	ptt         bool
	sMeterVal   byte
	swrVal      byte
	alcVal      byte

	ctrlPackets [][]byte // every control datagram the client sent
	civPackets  [][]byte // every civ datagram the client sent

	civAddrKnown chan struct{}
}

func NewFakeRadio(t testing.TB) *FakeRadio {
	t.Helper()
	f := &FakeRadio{
		t:            t,
		radioSID:     0x11223344,
		civTx:        map[uint16][]byte{},
		civAddrKnown: make(chan struct{}, 1),
		freq:         map[string]uint64{"main": 432_100_000, "sub": 145_800_000},
		mode:         map[string]string{"main": "usb", "sub": "fm"},
		selectedVFO:  "sub",
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
func (f *FakeRadio) Addr() *net.UDPAddr { return f.ctrl.LocalAddr().(*net.UDPAddr) }

// civAddr returns the fake's CI-V port.
func (f *FakeRadio) CIVPort() int { return f.civ.LocalAddr().(*net.UDPAddr).Port }

func (f *FakeRadio) serve(conn *net.UDPConn, stream string) {
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

func (f *FakeRadio) record(stream string, pkt []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if stream == "control" {
		f.ctrlPackets = append(f.ctrlPackets, pkt)
	} else {
		f.civPackets = append(f.civPackets, pkt)
	}
}

func (f *FakeRadio) counts() (logins, auth05, opens, closes, renewals int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loginCount, f.auth05Count, f.civOpened, f.civClosed, f.renewed
}

func (f *FakeRadio) ctrlLog() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]byte, len(f.ctrlPackets))
	copy(out, f.ctrlPackets)
	return out
}

func (f *FakeRadio) civLog() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]byte, len(f.civPackets))
	copy(out, f.civPackets)
	return out
}

func (f *FakeRadio) SetSilent(v bool) {
	f.mu.Lock()
	f.silent = v
	f.mu.Unlock()
}

// setRadioSID changes the fake's session ID — the "radio rebooted" script
// (a fresh full re-login must succeed against the new identity).
func (f *FakeRadio) SetRadioSID(sid uint32) {
	f.mu.Lock()
	f.radioSID = sid
	f.mu.Unlock()
}

// loginSpacings returns the gaps between successive login attempts.
func (f *FakeRadio) LoginSpacings() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]time.Duration, 0, len(f.loginTimes))
	for i := 1; i < len(f.loginTimes); i++ {
		out = append(out, f.loginTimes[i].Sub(f.loginTimes[i-1]))
	}
	return out
}

func (f *FakeRadio) handle(stream string, conn *net.UDPConn, from *net.UDPAddr, pkt []byte) {
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
		f.loginTimes = append(f.loginTimes, time.Now())
		if f.busyLogin {
			// The real radio's held-session reject (bench 2026-09-20):
			// 20 bytes, type 0x0001, body 81 ff ff ff — then the radio
			// keeps the control connection and pings.
			busy := make([]byte, 20)
			copy(busy, sigBusyReject)
			binary.BigEndian.PutUint32(busy[8:12], radioSID)
			binary.BigEndian.PutUint32(busy[12:16], binary.BigEndian.Uint32(pkt[12:16]))
			copy(busy[16:20], sigBusyBody)
			reply = busy
			break
		}
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
		// Tracked client CI-V data — recorded, then answered like the radio:
		// the fake carries a small device state (per-VFO freq/mode, selected
		// VFO, satellite mode, PTT, meters) so the bridge's reads observe
		// the sets.
		f.mu.Unlock()
		f.handleCIV(pkt)
		return
	}
	f.mu.Unlock()

	if reply != nil {
		_ = f.sendTo(conn, from, reply)
	}
}

func (f *FakeRadio) sendTo(conn *net.UDPConn, to *net.UDPAddr, pkt []byte) error {
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
func (f *FakeRadio) SendCIVFrame(payload []byte, drop bool) {
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

// handleCIV executes one CI-V frame against the fake's device state and
// answers like the radio (FB ack, or a data reply for reads).
func (f *FakeRadio) handleCIV(pkt []byte) {
	frame := dataPayload(pkt)
	if len(frame) < 6 || frame[0] != 0xFE || frame[1] != 0xFE {
		return
	}
	cmd := frame[4]
	sub := frame[5 : len(frame)-1] // between cmd and FD

	radioMu := &f.radioMu
	reply := func(data ...byte) {
		ans := append([]byte{0xFE, 0xFE, 0xE0, 0xA2, cmd}, data...)
		ans = append(ans, 0xFB, 0xFD)
		f.SendCIVFrame(ans, false)
	}
	ng := func() {
		ans := []byte{0xFE, 0xFE, 0xE0, 0xA2, cmd, 0xFA, 0xFD}
		f.SendCIVFrame(ans, false)
	}

	radioMu.Lock()
	defer radioMu.Unlock()
	switch {
	case cmd == 0x03: // read freq (selected VFO)
		bcd, err := BCD10Encode(f.freq[f.selectedVFO])
		if err != nil {
			ng()
			return
		}
		reply(bcd...)
	case cmd == 0x05 && len(sub) == 5: // set freq (selected VFO)
		hz, err := BCD10Decode(sub)
		if err != nil {
			ng()
			return
		}
		f.freq[f.selectedVFO] = hz
		reply()
	case cmd == 0x04 && len(sub) == 0: // read mode (selected VFO)
		reply(modeByte(f.mode[f.selectedVFO]), 0x01)
	case cmd == 0x06 && len(sub) == 2: // set mode+filter (or data-mode modifier)
		if m, ok := modeFromByteKnown(sub[0]); ok {
			f.mode[f.selectedVFO] = m
		}
		reply()
	case cmd == 0x07 && len(sub) == 1 && (sub[0] == 0xD0 || sub[0] == 0xD1):
		if sub[0] == 0xD0 {
			f.selectedVFO = "main"
		} else {
			f.selectedVFO = "sub"
		}
		reply()
	case cmd == 0x07 && len(sub) == 2 && sub[0] == 0xD2:
		if f.selectedVFO == "main" {
			reply(0x00)
		} else {
			reply(0x01)
		}
	case cmd == 0x16 && len(sub) >= 1 && sub[0] == 0x5A:
		if len(sub) == 2 {
			f.satMode = sub[1] == 0x01
			reply()
		} else {
			if f.satMode {
				reply(0x01)
			} else {
				reply(0x00)
			}
		}
	case cmd == 0x1C && len(sub) == 2 && sub[0] == 0x00: // PTT set
		f.ptt = sub[1] == 0x01
		reply()
	case cmd == 0x1C && len(sub) == 1 && sub[0] == 0x00: // PTT read
		if f.ptt {
			reply(0x01)
		} else {
			reply(0x00)
		}
	case cmd == 0x15 && len(sub) == 1 && sub[0] == 0x02:
		reply(f.sMeterVal)
	case cmd == 0x15 && len(sub) == 1 && sub[0] == 0x12:
		reply(f.swrVal)
	case cmd == 0x15 && len(sub) == 1 && sub[0] == 0x13:
		reply(f.alcVal)
	case cmd == 0x19: // transceiver ID
		reply(0x98) // the 9700's CI-V address
	case cmd == 0x14 || cmd == 0x06 || cmd == 0x1A:
		reply()
	default:
		ng()
	}
}

// Front-panel setters for tests: move the radio under the bridge's feet.
func (f *FakeRadio) SetSMeter(v byte) { f.radioMu.Lock(); f.sMeterVal = v; f.radioMu.Unlock() }
func (f *FakeRadio) SetSWR(v byte)    { f.radioMu.Lock(); f.swrVal = v; f.radioMu.Unlock() }
func (f *FakeRadio) SetALC(v byte)    { f.radioMu.Lock(); f.alcVal = v; f.radioMu.Unlock() }

// PTT reads the fake's keyed state.
func (f *FakeRadio) PTT() bool { f.radioMu.Lock(); defer f.radioMu.Unlock(); return f.ptt }

// SelectedVFO reads the fake's selected VFO.
func (f *FakeRadio) SelectedVFO() string {
	f.radioMu.Lock()
	defer f.radioMu.Unlock()
	return f.selectedVFO
}

func modeByte(m string) byte {
	switch m {
	case "lsb":
		return 0x00
	case "usb":
		return 0x01
	case "am":
		return 0x02
	case "cw":
		return 0x03
	case "fm":
		return 0x05
	default:
		return 0x01
	}
}

func modeFromByteKnown(b byte) (string, bool) {
	m, ok := ModeFromByte(b)
	return m, ok
}

// SetRefuseLogin scripts the credential rejection (ff ff ff fe login
// answer) — the wfview-holds / wrong-password rehearsal.
func (f *FakeRadio) SetRefuseLogin(v bool) {
	f.mu.Lock()
	f.refuseLogin = v
	f.mu.Unlock()
}

// SetBusyLogin scripts the 20-byte 81 ff ff ff login rejection real
// IC-9700 firmware sends while its single LAN session is held (bench
// 2026-09-20; undocumented in wfview/kappanhang).
func (f *FakeRadio) SetBusyLogin(v bool) {
	f.mu.Lock()
	f.busyLogin = v
	f.mu.Unlock()
}

// SetRefuseSess scripts the 0x50 ff ff ff stream-request refusal — the
// another-client-holds-the-session rehearsal.
func (f *FakeRadio) SetRefuseSess(v bool) {
	f.mu.Lock()
	f.refuseSess = v
	f.mu.Unlock()
}

// Counts returns the scripted-traffic tallies (logins, 0x05 auths, civ
// opens/closes, renewals).
func (f *FakeRadio) Counts() (logins, auth05, opens, closes, renewals int) {
	return f.counts()
}

// CtrlLog returns every control datagram the client sent, in order.
func (f *FakeRadio) CtrlLog() [][]byte { return f.ctrlLog() }

// CivLog returns every civ datagram the client sent, in order.
func (f *FakeRadio) CivLog() [][]byte { return f.civLog() }
