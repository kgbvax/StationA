package civ

// The fake radio's body lives in fakeradio.go (regular source — the radio
// session manager's and bridge slot's tests drive it over real loopback UDP
// through the exported surface). This file is only the testing.T adapter the
// package-internal tests have always used.

import "testing"

// fakeRadio is the historical name the package-internal tests use.
type fakeRadio = FakeRadio

// newFakeRadio builds a FakeRadio wired into t: bind failures are fatal and
// the fake closes at test cleanup.
func newFakeRadio(t *testing.T) *FakeRadio {
	t.Helper()
	fr, err := NewFakeRadio()
	if err != nil {
		t.Fatalf("fake radio bind: %v", err)
	}
	t.Cleanup(fr.Close)
	return fr
}
