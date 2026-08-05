package feed

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestBackoffOverridesTheSchedule(t *testing.T) {
	// Upstream asking us to slow down has to outrank our own prediction. Near an
	// expected publish the schedule polls once a minute, which is exactly the wrong
	// response to a 429.
	a := &Archiver{Log: slog.New(slog.DiscardHandler)}
	a.Delay = func(time.Time) time.Duration { return time.Minute }

	if got := a.nextDelay(10 * time.Minute); got != time.Minute {
		t.Fatalf("normal delay = %v, want the scheduled minute", got)
	}

	a.quietUntil = time.Now().Add(30 * time.Minute)
	got := a.nextDelay(10 * time.Minute)
	if got < 29*time.Minute {
		t.Errorf("delay while quiet = %v, want ~30m", got)
	}

	a.quietUntil = time.Now().Add(-time.Second)
	if got := a.nextDelay(10 * time.Minute); got != time.Minute {
		t.Errorf("delay after quiet expired = %v, want the schedule back", got)
	}
}

func TestRetryAfterParsing(t *testing.T) {
	if got := retryAfter("120"); got != 2*time.Minute {
		t.Errorf("seconds form = %v, want 2m", got)
	}
	// Absent or unparseable must err long: the point is to stop asking.
	for _, h := range []string{"", "soon", "-5"} {
		if got := retryAfter(h); got < time.Minute {
			t.Errorf("retryAfter(%q) = %v, want a conservative wait", h, got)
		}
	}
	if got := retryAfter(time.Now().Add(3 * time.Minute).UTC().Format(http.TimeFormat)); got < 2*time.Minute {
		t.Errorf("http-date form = %v, want ~3m", got)
	}
}

// TestStartupWaitRespectsTheAdaptiveSchedule covers the gap between the fetch a
// restart performs and the first scheduled one.
//
// Run polled once at startup and then slept the fixed interval, consulting Delay only
// from the second poll onward. So a restart blinded the adaptive schedule for a whole
// interval however close the next publish was — and because upstream publishes its
// two feeds about a minute apart, a fetch landing between them archives half a pair,
// forms no cycle, and then waits out that blind interval with the other half missing.
// Observed in production: a startup fetch eleven seconds early, then ten minutes late.
func TestStartupWaitRespectsTheAdaptiveSchedule(t *testing.T) {
	dir := t.TempDir()
	st, err := LoadState(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound) // the fetch is irrelevant; the schedule is not
	}))
	defer srv.Close()

	var mu sync.Mutex
	var delayCalls int
	a := &Archiver{
		Archive: &Archive{Root: dir},
		Fetcher: &Fetcher{Client: &http.Client{Transport: rewriteTransport{base: srv.URL}}},
		State:   st,
		Log:     slog.New(slog.DiscardHandler),
		Delay: func(time.Time) time.Duration {
			mu.Lock()
			delayCalls++
			mu.Unlock()
			return 20 * time.Millisecond
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// An interval far longer than the test. If the wait after the startup fetch
	// ignores Delay, it is never consulted at all and this fails deterministically
	// rather than by timing luck.
	go a.Run(ctx, time.Hour)

	// Assert on the startup wait alone. Waiting for a second poll would mean waiting
	// out minPollDelay, which nextDelay clamps every schedule up to — 15s of real
	// time to re-prove something the first call already settles. Before the fix Delay
	// was consulted zero times here, because the first wait was the fixed interval.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := delayCalls
		mu.Unlock()
		if n >= 1 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("Delay was never consulted with a 1h interval; the wait after the startup " +
		"fetch used the fixed interval instead of the adaptive schedule")
}
