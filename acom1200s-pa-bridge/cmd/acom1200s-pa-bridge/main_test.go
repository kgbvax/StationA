package main

import (
	"io"
	"log/slog"
	"testing"
)

type fakeMessage struct {
	topic    string
	payload  []byte
	retained bool
}

func (m fakeMessage) Duplicate() bool   { return false }
func (m fakeMessage) Qos() byte         { return 1 }
func (m fakeMessage) Retained() bool    { return m.retained }
func (m fakeMessage) Topic() string     { return m.topic }
func (m fakeMessage) MessageID() uint16 { return 0 }
func (m fakeMessage) Payload() []byte   { return m.payload }
func (m fakeMessage) Ack()              {}

func TestCmdMessageHandler(t *testing.T) {
	const topic = "muehle/hf/pa/cmd"
	live := []byte(`{"action":"set_mode","value":"standby"}`)
	tests := []struct {
		name string
		msg  fakeMessage
		want int
	}{
		{"live command dispatched", fakeMessage{topic: topic, payload: live}, 1},
		{"retained command dropped", fakeMessage{topic: topic, payload: live, retained: true}, 0},
		{"empty clear-echo ignored", fakeMessage{topic: topic}, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got [][]byte
			h := cmdMessageHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), func(p []byte) { got = append(got, p) })
			h(nil, tc.msg)
			if len(got) != tc.want {
				t.Fatalf("dispatched %d times, want %d", len(got), tc.want)
			}
		})
	}
}
