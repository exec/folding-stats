package service

import (
	"testing"
	"time"

	"folding/internal/feed"
)

func snap(kind feed.Kind, t time.Time) feed.Snapshot {
	return feed.Snapshot{Meta: feed.Meta{Feed: kind, SnapshotAt: t}}
}

func hour(h, m int) time.Time {
	return time.Date(2026, 8, 2, h, m, 0, 0, time.UTC)
}

func TestPairsUserWithPrecedingTeamSnapshot(t *testing.T) {
	// Upstream publishes the team feed at :29 and the user feed at :30, so pairing
	// has to be by proximity — the timestamps are never equal.
	teams := []feed.Snapshot{snap(feed.Teams, hour(21, 29)), snap(feed.Teams, hour(22, 29))}
	users := []feed.Snapshot{snap(feed.Users, hour(21, 30)), snap(feed.Users, hour(22, 30))}

	got := pairSnapshots(teams, users)
	if len(got) != 2 {
		t.Fatalf("got %d pairs, want 2", len(got))
	}
	for i, want := range []struct{ team, user time.Time }{
		{hour(21, 29), hour(21, 30)},
		{hour(22, 29), hour(22, 30)},
	} {
		if !got[i].team.Meta.SnapshotAt.Equal(want.team) {
			t.Errorf("pair %d team = %v, want %v", i, got[i].team.Meta.SnapshotAt, want.team)
		}
		if !got[i].user.Meta.SnapshotAt.Equal(want.user) {
			t.Errorf("pair %d user = %v, want %v", i, got[i].user.Meta.SnapshotAt, want.user)
		}
		// The cycle is stamped with the user snapshot, the later of the two.
		if !got[i].at.Equal(want.user) {
			t.Errorf("pair %d at = %v, want %v", i, got[i].at, want.user)
		}
	}
}

func TestUserSnapshotWithNoPrecedingTeamIsSkipped(t *testing.T) {
	// Happens on a cold archive: collection can begin between the team and user
	// publishes, leaving a user snapshot with nothing to pair against.
	teams := []feed.Snapshot{snap(feed.Teams, hour(22, 29))}
	users := []feed.Snapshot{snap(feed.Users, hour(21, 30)), snap(feed.Users, hour(22, 30))}

	got := pairSnapshots(teams, users)
	if len(got) != 1 {
		t.Fatalf("got %d pairs, want 1", len(got))
	}
	if !got[0].user.Meta.SnapshotAt.Equal(hour(22, 30)) {
		t.Errorf("paired user = %v, want 22:30", got[0].user.Meta.SnapshotAt)
	}
}

func TestMissingTeamPublishReusesPreviousTeamSnapshot(t *testing.T) {
	// Upstream feed failures are, per EOC's FAQ, "a common occurrence". If the team
	// feed misses a publish but the user feed does not, dropping the cycle would
	// throw away an hour of donor production. Reusing the last team snapshot costs
	// only a zero team delta, which the next cycle absorbs.
	teams := []feed.Snapshot{snap(feed.Teams, hour(21, 29)), snap(feed.Teams, hour(23, 29))}
	users := []feed.Snapshot{
		snap(feed.Users, hour(21, 30)),
		snap(feed.Users, hour(22, 30)), // no team publish this hour
		snap(feed.Users, hour(23, 30)),
	}

	got := pairSnapshots(teams, users)
	if len(got) != 3 {
		t.Fatalf("got %d pairs, want 3 (the gap hour must not be dropped)", len(got))
	}
	if !got[1].user.Meta.SnapshotAt.Equal(hour(22, 30)) {
		t.Errorf("pair 1 user = %v, want 22:30", got[1].user.Meta.SnapshotAt)
	}
	if !got[1].team.Meta.SnapshotAt.Equal(hour(21, 29)) {
		t.Errorf("pair 1 reused team = %v, want the 21:29 snapshot",
			got[1].team.Meta.SnapshotAt)
	}
	if !got[2].team.Meta.SnapshotAt.Equal(hour(23, 29)) {
		t.Errorf("pair 2 team = %v, want 23:29", got[2].team.Meta.SnapshotAt)
	}
}

func TestExtraTeamSnapshotsDoNotShiftPairing(t *testing.T) {
	// If the team feed publishes twice in one interval, the newest one at or before
	// the user snapshot wins.
	teams := []feed.Snapshot{
		snap(feed.Teams, hour(21, 29)),
		snap(feed.Teams, hour(21, 45)),
		snap(feed.Teams, hour(22, 29)),
	}
	users := []feed.Snapshot{snap(feed.Users, hour(21, 50)), snap(feed.Users, hour(22, 30))}

	got := pairSnapshots(teams, users)
	if len(got) != 2 {
		t.Fatalf("got %d pairs, want 2", len(got))
	}
	if !got[0].team.Meta.SnapshotAt.Equal(hour(21, 45)) {
		t.Errorf("pair 0 team = %v, want the newer 21:45", got[0].team.Meta.SnapshotAt)
	}
	if !got[1].team.Meta.SnapshotAt.Equal(hour(22, 29)) {
		t.Errorf("pair 1 team = %v, want 22:29", got[1].team.Meta.SnapshotAt)
	}
}

func TestEmptyInputs(t *testing.T) {
	if got := pairSnapshots(nil, nil); len(got) != 0 {
		t.Errorf("nil inputs produced %d pairs", len(got))
	}
	if got := pairSnapshots([]feed.Snapshot{snap(feed.Teams, hour(1, 0))}, nil); len(got) != 0 {
		t.Errorf("no users produced %d pairs", len(got))
	}
	if got := pairSnapshots(nil, []feed.Snapshot{snap(feed.Users, hour(1, 0))}); len(got) != 0 {
		t.Errorf("no teams produced %d pairs", len(got))
	}
}

func TestPairsAreChronological(t *testing.T) {
	// The metrics windows reject out-of-order cycles outright, so pairing must
	// never emit them.
	var teams, users []feed.Snapshot
	for h := 0; h < 12; h++ {
		teams = append(teams, snap(feed.Teams, hour(h, 29)))
		users = append(users, snap(feed.Users, hour(h, 30)))
	}
	got := pairSnapshots(teams, users)
	if len(got) != 12 {
		t.Fatalf("got %d pairs, want 12", len(got))
	}
	for i := 1; i < len(got); i++ {
		if !got[i].at.After(got[i-1].at) {
			t.Fatalf("pair %d at %v is not after %v", i, got[i].at, got[i-1].at)
		}
	}
}
