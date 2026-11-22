package tailnet

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const statusOut = `{"Peer":{
 "k1":{"HostName":"mac","Online":true,"TailscaleIPs":["100.85.205.27","fd7a::1"]},
 "k2":{"HostName":"scmino","Online":true,"TailscaleIPs":["100.82.232.82"]},
 "k3":{"HostName":"phone","Online":false,"TailscaleIPs":["100.1.1.1"]}}}`

func TestPingPeersPingsOnlineOnes(t *testing.T) {
	var calls []string
	run := func(_ context.Context, exe string, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		if args[0] == "status" {
			return []byte(statusOut), nil
		}
		if strings.HasSuffix(args[len(args)-1], "100.82.232.82") {
			return nil, errors.New("no reply")
		}
		return []byte("pong"), nil
	}
	res, err := PingPeers(context.Background(), run, "ts")
	if err != nil {
		t.Fatal(err)
	}
	if res.Peers != 2 || res.Pinged != 1 {
		t.Errorf("result = %+v, want 2 peers 1 pinged", res)
	}
	want := "ping --tsmp -c 1 --timeout 5s 100.85.205.27"
	if len(calls) != 3 || calls[2] != want {
		t.Errorf("calls = %q, want third call %q", calls, want)
	}
}

func TestPingPeersWithoutTailscale(t *testing.T) {
	run := func(context.Context, string, ...string) ([]byte, error) {
		return nil, &exec.Error{Name: "ts", Err: exec.ErrNotFound}
	}
	if _, err := PingPeers(context.Background(), run, "ts"); !errors.Is(err, ErrNoTailscale) {
		t.Errorf("err = %v, want ErrNoTailscale", err)
	}
}

func TestPingPeersBadStatus(t *testing.T) {
	run := func(context.Context, string, ...string) ([]byte, error) { return []byte("not json"), nil }
	if _, err := PingPeers(context.Background(), run, "ts"); err == nil {
		t.Error("want an error for unreadable status")
	}
}

func TestResumeDetector(t *testing.T) {
	t0 := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	d := NewResumeDetector(90*time.Second, t0)
	if d.Tick(t0.Add(30 * time.Second)) {
		t.Error("30 s is a normal tick")
	}
	if !d.Tick(t0.Add(30*time.Second + 20*time.Minute)) {
		t.Error("a 20 minute gap is a resume")
	}
	if d.Tick(t0.Add(31*time.Second + 20*time.Minute)) {
		t.Error("the tick after a resume is normal again")
	}
}
