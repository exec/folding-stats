package model

import (
	"math"
	"strings"
	"testing"

	"folding/internal/parse"
)

func TestReconcileOverflowIsolation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		teams []parse.TeamRow
		users []parse.UserRow
		want  []int32
	}{
		{"exact maximum", []parse.TeamRow{{ID: 1, Score: math.MaxInt64, WUs: math.MaxInt64}},
			[]parse.UserRow{{TeamID: 1, Score: math.MaxInt64, WUs: math.MaxInt64}}, nil},
		{"duplicate team authority", []parse.TeamRow{{ID: 1, Score: 10, WUs: 1}, {ID: 1, Score: 20, WUs: 2}},
			[]parse.UserRow{{TeamID: 1, Score: 30, WUs: 3}}, nil},
		{"duplicate overflow at maximum authority", []parse.TeamRow{{ID: 1, Score: math.MaxInt64}},
			[]parse.UserRow{{TeamID: 1, Score: math.MaxInt64}, {TeamID: 1, Score: 1}}, []int32{1}},
		{"duplicate work unit overflow", []parse.TeamRow{{ID: 1, WUs: math.MaxInt64}},
			[]parse.UserRow{{TeamID: 1, WUs: math.MaxInt64}, {TeamID: 1, WUs: 1}}, []int32{1}},
		{"global score overflow inside tolerance", []parse.TeamRow{{ID: 1, Score: math.MaxInt64 - 10}, {ID: 2, Score: 10}},
			[]parse.UserRow{{TeamID: 1, Score: math.MaxInt64 - 10}, {TeamID: 2, Score: 11}}, []int32{2}},
		{"global work unit overflow", []parse.TeamRow{{ID: 1, WUs: math.MaxInt64 - 10}, {ID: 2, WUs: 10}},
			[]parse.UserRow{{TeamID: 1, WUs: math.MaxInt64 - 10}, {TeamID: 2, WUs: 11}}, []int32{2}},
		{"normal tolerance", []parse.TeamRow{{ID: 1, Score: 100, WUs: 10}},
			[]parse.UserRow{{TeamID: 1, Score: 101, WUs: 11}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateSnapshot(tc.teams, nil); err != nil {
				t.Fatal(err)
			}
			over := Reconcile(tc.teams, tc.users, ReconcileWarnAt)
			if len(over) != len(tc.want) {
				t.Fatalf("quarantine = %+v, want %v", over, tc.want)
			}
			for i, id := range tc.want {
				if over[i].TeamID != id {
					t.Fatalf("quarantine = %+v, want %v", over, tc.want)
				}
			}
			if err := ValidateSnapshot(tc.teams, DropTeams(tc.users, over)); err != nil {
				t.Fatalf("unsafe rows survived reconciliation: %v", err)
			}
		})
	}
}

// The attack, driven through the real scanners rather than through hand-built rows.
// A transcription of what the parser is believed to produce would pass while the
// shipped one produced something else, and "what the parser does with a delimiter in a
// name" is the entire subject.
func TestForgedRecordIsCaughtByTheTeamFeed(t *testing.T) {
	const (
		teamFeed = "Tue Aug 04 12:34:06 UTC 2026\nname\tscore\twu\n" +
			"32\tReal Team\t900\t9\n"
		// One participant, whose chosen name is
		// "forged\t1000000000000\t1\t32\nreal". Upstream publishes it verbatim.
		userFeed = "Tue Aug 04 12:34:06 UTC 2026\nname\tnewcredit\tsum(total)\tteam\n" +
			"forged\t1000000000000\t1\t32\nreal\t500\t10\t32\n"
	)

	teams := scanTeams(t, teamFeed)
	users := scanUsers(t, userFeed)

	// Precondition: the parse really does split, or this test proves nothing.
	if len(users) != 2 {
		t.Fatalf("expected the name to split into 2 rows, got %d: %+v", len(users), users)
	}
	if users[0].Score != 1_000_000_000_000 {
		t.Fatalf("expected a forged trillion-point row, got %+v", users[0])
	}

	over := Reconcile(teams, users, ReconcileWarnAt)
	if len(over) != 1 || over[0].TeamID != 32 {
		t.Fatalf("reconciliation missed the forgery: %+v", over)
	}
	// 1,000,000,000,000 + 500 claimed against 900 held.
	if want := int64(1_000_000_000_000 + 500 - 900); over[0].Excess != want {
		t.Errorf("excess = %d, want %d", over[0].Excess, want)
	}
	if over[0].Excess <= ReconcileTolerance {
		t.Errorf("excess %d is within tolerance %d; the row would survive", over[0].Excess, ReconcileTolerance)
	}
	if got := DropTeams(users, over); len(got) != 0 {
		t.Errorf("dropping team 32 left %d rows: %+v", len(got), got)
	}
}

// An attacker-chosen team id the team feed has never published is the cheapest
// evasion, so an unmatched team reconciles against zero rather than being skipped.
func TestForgedRowOnAnUnknownTeamIsCaught(t *testing.T) {
	teams := []parse.TeamRow{{ID: 32, Name: "Real Team", Score: 900}}
	users := []parse.UserRow{{Name: "phantom", Score: 5_000_000_000, TeamID: 999999}}

	over := Reconcile(teams, users, ReconcileWarnAt)
	if len(over) != 1 || over[0].TeamID != 999999 {
		t.Fatalf("an unknown team was not reconciled: %+v", over)
	}
	if over[0].Excess <= ReconcileTolerance {
		t.Errorf("excess %d within tolerance", over[0].Excess)
	}
}

// The shape of the live corpus must pass untouched. Measured 2026-08-13: across
// 129,967 teams not one had members summing past its total, and the only rows without
// a team were 60 members holding at most 1.2M points between a team appearing in one
// feed before the other.
func TestRealisticFeedsReconcileCleanly(t *testing.T) {
	teams := []parse.TeamRow{
		{ID: 1, Name: "big", Score: 8_241_854_376_001},
		{ID: 2, Name: "small", Score: 1000},
		{ID: 3, Name: "empty", Score: 0},
	}
	users := []parse.UserRow{
		// Members summing to less than the team holds, which is the normal case: the
		// team feed leads the user feed and covers production not attributable to a
		// listed donor.
		{Name: "a", Score: 4_000_000_000_000, TeamID: 1},
		{Name: "b", Score: 4_000_000_000_000, TeamID: 1},
		{Name: "c", Score: 600, TeamID: 2},
		{Name: "d", Score: 400, TeamID: 2},
		// A member on a team the team feed has not published yet, at the largest
		// magnitude ever observed in that position.
		{Name: "newteam", Score: 1_196_338, TeamID: 424242},
	}

	over := Reconcile(teams, users, ReconcileWarnAt)
	for _, o := range over {
		if o.Excess > ReconcileTolerance {
			t.Errorf("legitimate data would have been dropped: team %d excess %d", o.TeamID, o.Excess)
		}
	}
	if got := DropTeams(users, nil); len(got) != len(users) {
		t.Errorf("DropTeams(nil) altered the rows: %d != %d", len(got), len(users))
	}
}

// Exact equality is the common case and must not register: a team whose members sum to
// precisely its total is 99.8% of the corpus.
func TestExactAgreementIsNotAnExcess(t *testing.T) {
	teams := []parse.TeamRow{{ID: 7, Score: 1000}}
	users := []parse.UserRow{{Name: "x", Score: 600, TeamID: 7}, {Name: "y", Score: 400, TeamID: 7}}
	if over := Reconcile(teams, users, ReconcileWarnAt); len(over) != 0 {
		t.Errorf("exact agreement reported as excess: %+v", over)
	}
}

func scanTeams(t *testing.T, feed string) []parse.TeamRow {
	t.Helper()
	sc := parse.NewTeamScanner(strings.NewReader(feed))
	var out []parse.TeamRow
	for sc.Scan() {
		out = append(out, sc.Row())
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func scanUsers(t *testing.T, feed string) []parse.UserRow {
	t.Helper()
	sc := parse.NewUserScanner(strings.NewReader(feed))
	var out []parse.UserRow
	for sc.Scan() {
		out = append(out, sc.Row())
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
