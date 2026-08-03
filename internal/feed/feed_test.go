package feed

import (
	"log/slog"
	"net/http"
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
