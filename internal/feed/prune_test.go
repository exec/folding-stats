package feed

import (
	"testing"
	"time"
)

func TestPruneThinsOldSnapshotsToDaily(t *testing.T) {
	a := &Archive{Root: t.TempDir()}
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)

	// Recent day (inside the window) and an old day (outside), 4 snapshots each.
	recent := now.Add(-24 * time.Hour)
	old := now.Add(-200 * 24 * time.Hour)
	for i := 0; i < 4; i++ {
		writeStub(t, a, Teams, recent.Add(time.Duration(i)*time.Hour))
		writeStub(t, a, Teams, old.Add(time.Duration(i)*time.Hour))
	}

	res, err := a.Prune(DefaultRetention, now)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	// All 4 recent survive; the old day thins to exactly 1.
	if res.Kept != 5 || res.Deleted != 3 {
		t.Errorf("Kept=%d Deleted=%d, want 5 and 3", res.Kept, res.Deleted)
	}

	snaps, _ := a.List(Teams)
	if len(snaps) != 5 {
		t.Fatalf("got %d snapshots after prune, want 5", len(snaps))
	}
	// The survivor from the old day must be the earliest of that day, so repeated
	// prunes are idempotent rather than walking the kept snapshot forward.
	if !snaps[0].Meta.SnapshotAt.Equal(old) {
		t.Errorf("kept %v from old day, want the first (%v)", snaps[0].Meta.SnapshotAt, old)
	}
}

func TestPruneIsIdempotent(t *testing.T) {
	a := &Archive{Root: t.TempDir()}
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	old := now.Add(-200 * 24 * time.Hour)
	for i := 0; i < 4; i++ {
		writeStub(t, a, Teams, old.Add(time.Duration(i)*time.Hour))
	}
	if _, err := a.Prune(DefaultRetention, now); err != nil {
		t.Fatal(err)
	}
	second, err := a.Prune(DefaultRetention, now)
	if err != nil {
		t.Fatal(err)
	}
	if second.Deleted != 0 {
		t.Errorf("second prune deleted %d, want 0", second.Deleted)
	}
}

func TestPruneKeepsEverythingWhenDisabled(t *testing.T) {
	a := &Archive{Root: t.TempDir()}
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		writeStub(t, a, Teams, now.Add(-time.Duration(i*400)*24*time.Hour))
	}
	res, err := a.Prune(RetentionPolicy{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted != 0 {
		t.Errorf("Deleted=%d with zero policy, want 0", res.Deleted)
	}
}

func TestPruneFeedsIndependently(t *testing.T) {
	// Team and user feeds publish a minute apart; thinning must not let one feed's
	// day-boundary decision delete the other's only surviving snapshot.
	a := &Archive{Root: t.TempDir()}
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	old := now.Add(-200 * 24 * time.Hour)
	for i := 0; i < 3; i++ {
		writeStub(t, a, Teams, old.Add(time.Duration(i)*time.Hour))
		writeStub(t, a, Users, old.Add(time.Duration(i)*time.Hour+time.Minute))
	}
	if _, err := a.Prune(DefaultRetention, now); err != nil {
		t.Fatal(err)
	}
	for _, k := range All() {
		snaps, _ := a.List(k)
		if len(snaps) != 1 {
			t.Errorf("feed %s: %d snapshots after prune, want 1", k, len(snaps))
		}
	}
}

func TestListSincePrunesByDate(t *testing.T) {
	// The archive grows by ~17k snapshots a year and ingest runs hourly, so listing
	// must not read every sidecar ever written.
	a := &Archive{Root: t.TempDir()}
	base := time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 40; i++ { // spans several months
		writeStub(t, a, Teams, base.AddDate(0, 0, -i*7))
	}

	all, err := a.List(Teams)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 40 {
		t.Fatalf("List returned %d, want 40", len(all))
	}

	cutoff := base.AddDate(0, 0, -20)
	recent, err := a.ListSince(Teams, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) == 0 || len(recent) >= len(all) {
		t.Fatalf("ListSince returned %d of %d; expected a strict subset", len(recent), len(all))
	}
	for _, s := range recent {
		if s.Meta.SnapshotAt.Before(cutoff) {
			t.Errorf("ListSince returned %v, before the %v cutoff", s.Meta.SnapshotAt, cutoff)
		}
	}
	// Nothing at or after the cutoff may be missed.
	want := 0
	for _, s := range all {
		if !s.Meta.SnapshotAt.Before(cutoff) {
			want++
		}
	}
	if len(recent) != want {
		t.Errorf("ListSince returned %d snapshots, want %d", len(recent), want)
	}
}

func TestListSinceZeroReturnsEverything(t *testing.T) {
	a := &Archive{Root: t.TempDir()}
	base := time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		writeStub(t, a, Users, base.AddDate(0, -i, 0))
	}
	got, err := a.ListSince(Users, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Errorf("ListSince(zero) returned %d, want 5", len(got))
	}
}
