package service

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeCDN records purge requests and answers however the test tells it to.
type fakeCDN struct {
	mu    sync.Mutex
	calls []map[string][]string
	body  string
	code  int
}

func (f *fakeCDN) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req map[string][]string
		json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.calls = append(f.calls, req)
		f.mu.Unlock()
		if f.code != 0 {
			w.WriteHeader(f.code)
		}
		body := f.body
		if body == "" {
			body = `{"success":true,"errors":[]}`
		}
		io.WriteString(w, body)
	}
}

func (f *fakeCDN) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// waitFor polls until cond holds, because Purge dispatches on its own goroutine —
// the whole point being that it does not block the publish that called it.
func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func testPurger(t *testing.T, cdn *fakeCDN) *purger {
	t.Helper()
	srv := httptest.NewServer(cdn.handler())
	t.Cleanup(srv.Close)
	p := newPurger("zone", "token", "https://foldingstats.org", quietLog())
	if p == nil {
		t.Fatal("purger is nil despite full configuration")
	}
	p.endpoint = srv.URL
	return p
}

func TestPurgeFiresOncePerSnapshot(t *testing.T) {
	cdn := &fakeCDN{}
	p := testPurger(t, cdn)

	at := time.Date(2026, 8, 4, 22, 35, 0, 0, time.UTC)
	p.Purge(at)
	waitFor(t, func() bool { return cdn.count() == 1 }, "the first purge")

	// publish() runs on restart and on republish, not only on new data. Those must
	// not each spend a purge on a cache that is already correct.
	p.Purge(at)
	p.Purge(at.Add(-time.Hour)) // an older snapshot is not news either
	time.Sleep(50 * time.Millisecond)
	if n := cdn.count(); n != 1 {
		t.Errorf("%d purges for one snapshot; repeat publishes should be ignored", n)
	}

	files := cdn.calls[0]["files"]
	if len(files) != len(purgePaths) {
		t.Fatalf("purged %d urls, expected %d", len(files), len(purgePaths))
	}
	for _, f := range files {
		if !strings.HasPrefix(f, "https://foldingstats.org/v1/") {
			t.Errorf("purge url %q is not an absolute url on the site", f)
		}
	}
	// Never purge_everything: the token's zone covers more than this service.
	if _, ok := cdn.calls[0]["purge_everything"]; ok {
		t.Error("request included purge_everything; it would evict the whole zone")
	}
}

func TestPurgeRateLimitBlocksARapidSecondSnapshot(t *testing.T) {
	cdn := &fakeCDN{}
	p := testPurger(t, cdn)

	base := time.Date(2026, 8, 4, 22, 35, 0, 0, time.UTC)
	p.Purge(base)
	waitFor(t, func() bool { return cdn.count() == 1 }, "the first purge")

	// A newer snapshot, but seconds later. Real cycles are an hour apart; anything
	// faster is a loop or a replay, and it must not be allowed to hammer the API.
	p.Purge(base.Add(time.Hour))
	time.Sleep(50 * time.Millisecond)
	if n := cdn.count(); n != 1 {
		t.Errorf("%d purges within the minimum interval, want 1", n)
	}
}

func TestPurgeFailureIsNotFatal(t *testing.T) {
	// Cloudflare answers 200 with success:false for an expired or unscoped token, so
	// a handler that only checked the status code would call this a success.
	cdn := &fakeCDN{body: `{"success":false,"errors":[{"code":10000,"message":"Authentication error"}]}`}
	p := testPurger(t, cdn)

	done := make(chan struct{})
	go func() {
		p.Purge(time.Now())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Purge blocked; it must never hold up a publish")
	}
	waitFor(t, func() bool { return cdn.count() == 1 }, "the rejected purge")
	// Nothing to assert beyond not panicking and not blocking: a rejected purge is
	// logged and the TTL takes over.
}

func TestPurgeDisabledWithoutFullConfiguration(t *testing.T) {
	// A nil purger has to be a working no-op, because that is the normal state for a
	// local run and for anybody else running this.
	for _, c := range []struct{ zone, token, site string }{
		{"", "token", "https://x"},
		{"zone", "", "https://x"},
		{"zone", "token", ""},
		{"", "", ""},
	} {
		p := newPurger(c.zone, c.token, c.site, quietLog())
		if p != nil {
			t.Errorf("newPurger(%q,%q,%q) returned a purger; want nil", c.zone, c.token, c.site)
			continue
		}
		p.Purge(time.Now()) // must not panic on the nil receiver
	}
}

// TestPurgePathsAreRealRoutes guards the one fragile part of purging by URL.
//
// The paths are written out by hand to match what the frontend requests, so a renamed
// route or a dropped parameter would leave this purging URLs that no longer exist —
// silently, since a purge of nothing still succeeds. This does not catch every kind
// of drift (a changed query string still matches a live route), but it does catch the
// list naming something the API no longer serves.
func TestPurgePathsAreRealRoutes(t *testing.T) {
	if len(purgePaths) > purgeMaxFiles {
		t.Fatalf("%d purge paths exceeds the %d per-call limit", len(purgePaths), purgeMaxFiles)
	}
	seen := map[string]bool{}
	for _, p := range purgePaths {
		if seen[p] {
			t.Errorf("duplicate purge path %q", p)
		}
		seen[p] = true

		u, err := url.Parse(p)
		if err != nil {
			t.Errorf("purge path %q does not parse: %v", p, err)
			continue
		}
		if !strings.HasPrefix(u.Path, "/v1/") {
			t.Errorf("purge path %q is not an API route", p)
		}
		// /v1/status is sent no-store precisely so it is never cached; purging it
		// would be a wasted slot and a sign the list was copied carelessly.
		if u.Path == "/v1/status" {
			t.Error("/v1/status is never cached; purging it does nothing")
		}
	}
}
