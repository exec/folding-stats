package bot

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// The bot spent 11 August 2026 answering with figures nine and a half hours old, and
// its alert watcher had never fired at all. Both came from the same place: the cache
// stored whole envelopes but handed back only the data inside them, and the freshness
// probe was cached like everything else.
//
// These are the two checks that would have caught it. They are about the cache's
// contract, not about any particular route.
func TestFreshnessProbeIsNeverCached(t *testing.T) {
	var hits atomic.Int64
	at := "2026-08-11T06:51:11Z"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"snapshot":{"at":%q,"stale":false},"data":{"donors":7}}`, at)
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	ctx := context.Background()

	// Polling the probe must reach the origin every time. Answering it from the cache
	// is what froze the bot: it is the only route that can report the cache is stale,
	// so a cached copy means nothing ever refreshes again.
	for i := range 3 {
		env, err := c.GetEnvelope(ctx, "/v1/status")
		if err != nil {
			t.Fatalf("poll %d: %v", i, err)
		}
		want, _ := time.Parse(time.RFC3339, at)
		if !env.Snapshot.At.Equal(want) {
			t.Fatalf("poll %d: snapshot = %v, want %v — a zero time here is what stopped every alert firing",
				i, env.Snapshot.At, want)
		}
	}
	if n := hits.Load(); n != 3 {
		t.Errorf("origin saw %d requests for the probe, want 3; it is being served from cache", n)
	}

	// A new publish must invalidate everything, and the probe is what notices.
	before := hits.Load()
	at = "2026-08-11T07:51:17Z"
	if _, err := c.GetEnvelope(ctx, "/v1/status"); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Donors int `json:"donors"`
	}
	snap, err := c.Get(ctx, "/v1/summary", &got)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := time.Parse(time.RFC3339, at)
	if !snap.At.Equal(want) {
		t.Errorf("after a publish, snapshot = %v, want %v", snap.At, want)
	}
	if got.Donors != 7 {
		t.Errorf("data decoded as %+v; Get must unmarshal the data, not the envelope", got)
	}
	if hits.Load() <= before {
		t.Error("nothing was refetched after the publish")
	}
}

// Ordinary routes must still be cached, or the fix trades one bug for a busy origin.
func TestOrdinaryRoutesStayCached(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"snapshot":{"at":"2026-08-11T06:51:11Z"},"data":{"donors":7}}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	for range 4 {
		var got struct {
			Donors int `json:"donors"`
		}
		if _, err := c.Get(context.Background(), "/v1/summary", &got); err != nil {
			t.Fatal(err)
		}
		if got.Donors != 7 {
			t.Fatalf("donors = %d, want 7", got.Donors)
		}
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("origin saw %d requests, want 1 — the data is immutable between publishes", n)
	}
}

func TestAttackerChosenSearchesAreNeverCached(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, `{"snapshot":{"at":"2026-08-11T06:51:11Z"},"data":[]}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	for range 3 {
		var got []any
		if _, err := c.Get(context.Background(), "/v1/search?q=attacker-controlled", &got); err != nil {
			t.Fatal(err)
		}
	}
	if n := hits.Load(); n != 3 {
		t.Fatalf("search origin saw %d requests, want 3; attacker-controlled keys entered the cache", n)
	}
}
