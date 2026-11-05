// SPDX-License-Identifier: AGPL-3.0-or-later

package qrz

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testRecord(call string) Record {
	return Record{Call: call, Grid: "JO60AB", Lat: 48.13, Lon: 11.57, Country: "Germany", Qth: "München", Name: "Jürgen Müller"}
}

func TestCachePutGetRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "qrz.json")
	c := OpenCache(path, DefaultCacheDays, DefaultNegativeMinutes, 10)

	if err := c.Put(testRecord("DL1ABC")); err != nil {
		t.Fatalf("put: %v", err)
	}
	rec, ok, err := c.Get(" dl1abc ")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v, want hit", ok, err)
	}
	if rec.Call != "DL1ABC" || rec.Grid != "JO60AB" {
		t.Errorf("record = %+v", rec)
	}
}

func TestCachePersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "qrz.json")
	c := OpenCache(path, DefaultCacheDays, DefaultNegativeMinutes, 10)
	if err := c.Put(testRecord("DL1ABC")); err != nil {
		t.Fatalf("put: %v", err)
	}

	reopened := OpenCache(path, DefaultCacheDays, DefaultNegativeMinutes, 10)
	if _, ok, _ := reopened.Get("DL1ABC"); !ok {
		t.Fatal("reopened cache lost the entry")
	}
}

func TestCacheMiss(t *testing.T) {
	c := OpenCache(filepath.Join(t.TempDir(), "qrz.json"), DefaultCacheDays, DefaultNegativeMinutes, 10)
	rec, ok, err := c.Get("DL1ABC")
	if ok || err != nil || rec != (Record{}) {
		t.Errorf("get = (%+v, %v, %v), want a clean miss", rec, ok, err)
	}
}

func TestCachePositiveTTLExpiry(t *testing.T) {
	c := OpenCache(filepath.Join(t.TempDir(), "qrz.json"), time.Hour, DefaultNegativeMinutes, 10)
	now := time.Unix(1_000_000, 0)
	c.now = func() time.Time { return now }

	if err := c.Put(testRecord("DL1ABC")); err != nil {
		t.Fatalf("put: %v", err)
	}
	now = now.Add(time.Hour + time.Minute) // past the positive TTL
	if _, ok, _ := c.Get("DL1ABC"); ok {
		t.Error("entry survived the positive TTL")
	}
}

func TestCacheNegativeTTL(t *testing.T) {
	c := OpenCache(filepath.Join(t.TempDir(), "qrz.json"), DefaultCacheDays, 10*time.Minute, 10)
	now := time.Unix(1_000_000, 0)
	c.now = func() time.Time { return now }

	if err := c.PutNotFound("XX9XX"); err != nil {
		t.Fatalf("put not-found: %v", err)
	}

	// Inside the negative TTL: a typed not-found, never a hit.
	rec, ok, err := c.Get("XX9XX")
	if ok || !errors.Is(err, ErrNotFound) || rec != (Record{}) {
		t.Errorf("get = (%+v, %v, %v), want negative hit", rec, ok, err)
	}

	// Past it: a clean miss so the next lookup goes back to QRZ.
	now = now.Add(11 * time.Minute)
	if _, ok, err := c.Get("XX9XX"); ok || err != nil {
		t.Errorf("get after negative TTL = (%v, %v), want clean miss", ok, err)
	}
}

func TestCacheCapEvictsOldest(t *testing.T) {
	c := OpenCache(filepath.Join(t.TempDir(), "qrz.json"), DefaultCacheDays, DefaultNegativeMinutes, 3)
	base := time.Unix(1_000_000, 0)
	tick := base
	c.now = func() time.Time { return tick }

	for _, call := range []string{"DL1AAA", "DL1BBB", "DL1CCC"} {
		tick = tick.Add(time.Second)
		if err := c.Put(testRecord(call)); err != nil {
			t.Fatalf("put %s: %v", call, err)
		}
	}
	tick = tick.Add(time.Second)
	if err := c.Put(testRecord("DL1DDD")); err != nil {
		t.Fatalf("put DL1DDD: %v", err)
	}

	if _, ok, _ := c.Get("DL1AAA"); ok {
		t.Error("oldest entry (DL1AAA) survived eviction")
	}
	for _, call := range []string{"DL1BBB", "DL1CCC", "DL1DDD"} {
		if _, ok, _ := c.Get(call); !ok {
			t.Errorf("%s evicted though newer entries remain", call)
		}
	}
}

func TestCacheCorruptFileIsColdStart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "qrz.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	c := OpenCache(path, DefaultCacheDays, DefaultNegativeMinutes, 10)
	if _, ok, _ := c.Get("DL1ABC"); ok {
		t.Error("corrupt file yielded a hit")
	}
	// And the cache still works going forward.
	if err := c.Put(testRecord("DL1ABC")); err != nil {
		t.Fatalf("put after cold start: %v", err)
	}
	if _, ok, _ := c.Get("DL1ABC"); !ok {
		t.Error("put after cold start did not stick")
	}
}
