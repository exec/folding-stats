package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"folding/internal/store"
)

// These exercise nextDue from main.go rather than a copy of it. A transcription here
// would pass while the shipped scheduler stayed broken, which is precisely how the
// original bug survived: maintain built a 24-hour ticker at startup and waited on it,
// so the pass was always a full day away from the last restart. A service redeployed
// several times a day never reached one, and in the first nine days of this service's
// life it ran zero times while the archive it was meant to thin grew by 6.8 GB.

func TestMaintenanceScheduleSurvivesRestarts(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name string
		last string
		want time.Duration // how long after now the pass is due
	}{
		// The case that never happened before: a service that has never run one gets
		// a pass shortly after it starts, not a day later.
		{"never run", "", maintenanceDelay},

		// The bug itself. Eight restarts in a day used to mean eight fresh 24-hour
		// waits and no pass; the recorded run now carries across them.
		{"ran an hour ago", now.Add(-time.Hour).Format(time.RFC3339), 23 * time.Hour},
		{"ran 23h ago", now.Add(-23 * time.Hour).Format(time.RFC3339), time.Hour},

		// Overdue: due immediately, but still floored at the startup delay so a
		// restart cannot land maintenance on top of the initial ingest.
		{"ran 3 days ago", now.Add(-72 * time.Hour).Format(time.RFC3339), maintenanceDelay},

		// A corrupt marker must not wedge the schedule permanently.
		{"unparseable", "not a timestamp", maintenanceDelay},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := nextDue(tc.last, now).Sub(now)
			if got != tc.want {
				t.Errorf("due in %v, want %v", got, tc.want)
			}
		})
	}
}

// The marker has to outlive the process, which means it has to be in the database and
// not in a variable. A restart reads back what the last run wrote.
func TestMaintenanceMarkerRoundTrips(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "m.db")

	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// An unset marker is empty and not an error — the state every new install is in.
	if v, err := db.Meta(ctx, maintenanceKey); err != nil || v != "" {
		t.Fatalf("unset marker = %q, %v; want empty and no error", v, err)
	}

	stamp := time.Date(2026, 8, 12, 4, 17, 0, 0, time.UTC).Format(time.RFC3339)
	if err := db.SetMeta(ctx, maintenanceKey, stamp); err != nil {
		t.Fatal(err)
	}
	db.Close()

	// Reopened, as after a deploy.
	db2, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	got, err := db2.Meta(ctx, maintenanceKey)
	if err != nil {
		t.Fatal(err)
	}
	if got != stamp {
		t.Errorf("after reopen, marker = %q, want %q", got, stamp)
	}
	if due := nextDue(got, time.Date(2026, 8, 12, 5, 0, 0, 0, time.UTC)); !due.Equal(
		time.Date(2026, 8, 13, 4, 17, 0, 0, time.UTC)) {
		t.Errorf("a restart moved the next pass to %v, want the day after the last run", due)
	}
}
