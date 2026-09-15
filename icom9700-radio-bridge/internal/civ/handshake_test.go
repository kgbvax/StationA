package civ

// Handshake FSM tests: the connect sequence runs against the fake radio and
// the tests assert the exact packet sequence, the timeout bounds and the
// error paths (R1: the full RS-BA1 handshake; R3: failures surface as facts).

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"log/slog"
	"testing"
	"time"
)

// testOpts builds fast-shrunk options around the fake radio (production
// cadences come from the protocol brief; tests shrink every period).
func testOpts(fr *fakeRadio, log *slog.Logger) Opts {
	o := Opts{
		Host:       "127.0.0.1",
		Username:   "operator1",
		Password:   "s3cret!",
		ClientName: "icom9700-radio-bridge",
		Log:        log,
	}
	o.ControlPort = fr.ctrlPort()
	o.AreYouTherePeriod = 40 * time.Millisecond
	o.HandshakeTimeout = 5 * time.Second
	o.IdlePeriod = 25 * time.Millisecond
	o.PingPeriod = 60 * time.Millisecond
	o.RetransmitPeriod = 25 * time.Millisecond
	o.TokenRenewal = 150 * time.Millisecond
	o.RenewalTimeout = 600 * time.Millisecond
	o.CivSilence = 200 * time.Millisecond
	o.StartDataPeriod = 60 * time.Millisecond
	o.SessionTimeout = 400 * time.Millisecond
	return o
}

// waitFor polls cond until it holds or the deadline passes (test helper for
// asynchronous radio/client exchanges).
func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// sockTypes returns the packet type byte of every datagram the fake received
// on a socket, in arrival order — the packet-sequence assertion primitive.
func sockTypes(t *testing.T, fr *fakeRadio, sock string) []byte {
	t.Helper()
	pkts := fr.recvOn(sock)
	types := make([]byte, 0, len(pkts))
	for _, p := range pkts {
		if len(p.data) < typeOff+2 {
			types = append(types, 0xff)
			continue
		}
		types = append(types, p.data[typeOff])
	}
	return types
}

func TestDialHappyPathExactSequence(t *testing.T) {
	fr := newFakeRadio(t)
	tr, err := Dial(context.Background(), testOpts(fr, testLogger(t)))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer tr.Close()
	if !tr.Live() {
		t.Fatal("transport must be live after a full handshake")
	}

	// Exact control-stream packet sequence the client must produce:
	// are-you-there, are-you-ready, then login (0x80), token confirm (0x40,
	// requesttype 0x02), token renewal (0x40, requesttype 0x05) and the
	// stream request (0x90) — the auth packets all ride as type-0x00 tracked
	// packets, so their lengths pin the family.
	wantSeq := []struct {
		typ byte
		len int
	}{
		{ptAreYouThere, ctrlLen},
		{ptAreYouReady, ctrlLen},
		{ptIdle, loginLen},
		{ptIdle, tokenLen},
		{ptIdle, tokenLen},
		{ptIdle, streamReqLen},
	}
	pkts := fr.recvOn(sockCtrl)
	if len(pkts) < len(wantSeq) {
		t.Fatalf("control packets = %d, want at least %d", len(pkts), len(wantSeq))
	}
	for i, w := range wantSeq {
		p := pkts[i].data
		if len(p) != w.len {
			t.Fatalf("control packet %d len = 0x%x, want 0x%x (types seen: % x)", i, len(p), w.len, sockTypes(t, fr, sockCtrl))
		}
		if typ := p[typeOff]; typ != w.typ {
			t.Fatalf("control packet %d type = 0x%02x, want 0x%02x", i, typ, w.typ)
		}
	}

	// Login payload: substitution-table-obfuscated credentials at 0x40/0x50,
	// a nonzero token request, and the client name.
	login := pkts[2].data
	wantUser, wantPass := passcode("operator1"), passcode("s3cret!")
	if !bytes.Equal(login[userOff:userOff+16], wantUser[:]) {
		t.Errorf("login username field = % x, want substituted %q", login[userOff:userOff+16], "operator1")
	}
	if !bytes.Equal(login[passOff:passOff+16], wantPass[:]) {
		t.Errorf("login password field = % x, want substituted %q", login[passOff:passOff+16], "s3cret!")
	}
	if binary.LittleEndian.Uint16(login[tokReqOff:]) == 0 {
		t.Error("login tokrequest must be nonzero")
	}
	wantName := field16("icom9700-radio-bridge")
	if !bytes.Equal(login[nameOff:nameOff+16], wantName[:]) {
		t.Errorf("login client name = % x, want the (16-byte-capped) client name", login[nameOff:nameOff+16])
	}

	// Token packets: requestreply 0x01, requesttype 0x02 then 0x05 (the
	// wfview/kappanhang renewal type), carrying the token from the login.
	if pkts[3].data[reqTypeOff] != 0x02 {
		t.Errorf("first token packet requesttype = 0x%02x, want 0x02 (confirm)", pkts[3].data[reqTypeOff])
	}
	if pkts[4].data[reqTypeOff] != 0x05 {
		t.Errorf("second token packet requesttype = 0x%02x, want 0x05 (renewal)", pkts[4].data[reqTypeOff])
	}
	for _, i := range []int{3, 4} {
		if pkts[i].data[reqReplyOff] != 0x01 {
			t.Errorf("token packet %d requestreply = 0x%02x, want 0x01 (request)", i, pkts[i].data[reqReplyOff])
		}
		if got := binary.LittleEndian.Uint32(pkts[i].data[tokenOff:]); got != fakeToken {
			t.Errorf("token packet %d carries token %#x, want %#x", i, got, fakeToken)
		}
	}

	// Stream request carries the client's chosen local CI-V port big-endian
	// at 0x7c; the radio answered with its own CI-V port and the client
	// opened the data socket to exactly that port.
	streamReq := pkts[5].data
	if streamReq[reqTypeOff] != 0x03 {
		t.Errorf("stream request requesttype = 0x%02x, want 0x03", streamReq[reqTypeOff])
	}
	chosen := binary.BigEndian.Uint32(streamReq[civLocalPortOff:])
	if int(chosen) != tr.civStream().localPort() {
		t.Errorf("stream request civ port = %d, want the bound local port %d", chosen, tr.civStream().localPort())
	}
	// The CI-V socket is bound to the chosen port and connected to the
	// radio's data port from the status packet.
	if tr.civStream().radioAddr.Port != fr.civPort() {
		t.Errorf("civ stream remote port = %d, want %d (status packet)", tr.civStream().radioAddr.Port, fr.civPort())
	}

	// Exact CI-V socket sequence: are-you-there, are-you-ready, open. The
	// open datagram races the fake's serve goroutine, so poll for it.
	waitFor(t, time.Second, "open packet at the fake", func() bool {
		return len(fr.recvOn(sockCiv)) >= 3
	})
	civPkts := fr.recvOn(sockCiv)
	wantCiv := []struct {
		typ byte
		len int
	}{
		{ptAreYouThere, ctrlLen},
		{ptAreYouReady, ctrlLen},
		{ptIdle, openCloseLen},
	}
	for i, w := range wantCiv {
		p := civPkts[i].data
		if len(p) != w.len {
			t.Fatalf("civ packet %d len = 0x%x, want 0x%x", i, len(p), w.len)
		}
		if typ := p[typeOff]; typ != w.typ {
			t.Fatalf("civ packet %d type = 0x%02x, want 0x%02x", i, typ, w.typ)
		}
	}
	open := civPkts[2].data
	if got := binary.LittleEndian.Uint16(open[dataFieldOff:]); got != 0x01c0 {
		t.Errorf("civ open packet data field = 0x%04x, want 0x01c0", got)
	}
	if open[magicOff] != 0x04 {
		t.Errorf("civ open packet magic = 0x%02x, want 0x04", open[magicOff])
	}
}

func TestDialAreYouThereSilentTimesOut(t *testing.T) {
	fr := newFakeRadio(t)
	fr.setSilent()
	o := testOpts(fr, testLogger(t))
	o.HandshakeTimeout = 300 * time.Millisecond

	start := time.Now()
	_, err := Dial(context.Background(), o)
	if err == nil {
		t.Fatal("dial against a silent radio must fail")
	}
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("error = %v, want ErrTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("dial took %s, want the configured ~300ms bound", elapsed)
	}
	// The are-you-there probe repeated at its configured period while waiting.
	probes := 0
	for _, p := range fr.recvOn(sockCtrl) {
		if len(p.data) == ctrlLen && p.data[typeOff] == ptAreYouThere {
			probes++
		}
	}
	if probes < 3 {
		t.Errorf("are-you-there probes = %d, want several at the configured 40ms period", probes)
	}
}

func TestDialIamHereButNoReadyTimesOut(t *testing.T) {
	fr := newFakeRadio(t)
	fr.setNoReady()
	o := testOpts(fr, testLogger(t))
	o.HandshakeTimeout = 300 * time.Millisecond

	_, err := Dial(context.Background(), o)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("error = %v, want ErrTimeout when the radio never answers are-you-ready", err)
	}
}

func TestDialBadCredentials(t *testing.T) {
	fr := newFakeRadio(t)
	fr.setLoginErr(0xfffffffe) // login response error: invalid username/password
	_, err := Dial(context.Background(), testOpts(fr, testLogger(t)))
	if !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("error = %v, want ErrAuthFailed for login response 0xfffffffe", err)
	}
}

func TestDialStreamRefused(t *testing.T) {
	fr := newFakeRadio(t)
	fr.setRefused() // status error 0xffffffff: connection refused (stale/other session)
	_, err := Dial(context.Background(), testOpts(fr, testLogger(t)))
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("error = %v, want ErrRefused for status error 0xffffffff", err)
	}
}

func TestDialContextCancel(t *testing.T) {
	fr := newFakeRadio(t)
	fr.setSilent()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	_, err := Dial(ctx, testOpts(fr, testLogger(t)))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled when the dial ctx dies mid-handshake", err)
	}
}

func TestDialRequiresCredentials(t *testing.T) {
	fr := newFakeRadio(t)
	o := testOpts(fr, testLogger(t))
	o.Username = ""
	o.Password = ""
	if _, err := Dial(context.Background(), o); err == nil {
		t.Fatal("dial without credentials must fail — the login packet would authenticate with empty secrets")
	}
}

func TestPasscodeSubstitutionTable(t *testing.T) {
	// Hand-computed vectors from the public wfview/kappanhang table:
	// p = s[i] + i (wrapped into 32..126), then table lookup; padded to 16.
	got := passcode("ab")
	if got[0] != 0x38 || got[1] != 0x2e {
		t.Errorf("passcode(\"ab\") = % x, want 38 2e prefix", got[:2])
	}
	got = passcode("~") // 126 + 0 -> table[126] = 0x52
	if got[0] != 0x52 {
		t.Errorf("passcode(\"~\") = % x, want 52 prefix", got[:1])
	}
	got = passcode("\xff") // 255 > 126 -> 32 + 255%127 = 33 -> table[33] = 0x5d
	if got[0] != 0x5d {
		t.Errorf("passcode(0xff) = % x, want 5d prefix", got[:1])
	}
	for i := 2; i < 16; i++ {
		if got[i] != 0 {
			t.Errorf("passcode padding byte %d = %#x, want 0", i, got[i])
		}
	}
	// Only the first 16 bytes of a long secret are used (protocol field size).
	long := passcode("0123456789abcdefg")
	short := passcode("0123456789abcdef")
	if !bytes.Equal(long[:], short[:]) {
		t.Error("passcode must truncate to the 16-byte protocol field")
	}
}
