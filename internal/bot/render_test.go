package bot

import (
	"strings"
	"testing"
	"time"
)

// TestTeamListNamesTheTeam guards the trap in the membership rows.
//
// The service stores at the grain (name, team), so "name" on those rows is the donor
// and the team is "team_name". Reading the first produced a "Folding for" list that
// was the donor's own name repeated once per team — valid JSON, valid embed, wrong
// answer. The fixture keeps the two obviously different so the mistake cannot pass.
func TestTeamListNamesTheTeam(t *testing.T) {
	got := teamList([]Membership{
		{TeamID: 52552, TeamName: "University of Wisconsin-Madison", Donor: "DH", RankInTeam: 1, PointsPerDay: 60298676, PointsTotal: 2537325639},
		{TeamID: 1066966, TeamName: "Team Pewds", Donor: "DH", RankInTeam: 7, PointsTotal: 2381783463},
		{TeamID: 33147, TeamName: "USA", Donor: "DH", RankInTeam: 5, PointsTotal: 745645066},
	})
	if strings.Contains(got, "DH") {
		t.Errorf("the donor's name leaked into the team list:\n%s", got)
	}
	if !strings.Contains(got, "University of Wisconsin-Madison") {
		t.Errorf("team name missing:\n%s", got)
	}
	// The dormant memberships are a count, not four rows of `0/day`.
	if strings.Contains(got, "0/day") {
		t.Errorf("dormant teams listed with a zero rate:\n%s", got)
	}
	if !strings.Contains(got, "2 with nothing recent") {
		t.Errorf("dormant teams not summarised:\n%s", got)
	}
	if !strings.Contains(got, "#1 on team") {
		t.Errorf("rank within the team missing:\n%s", got)
	}
}

// A donor who has stopped folding everywhere still has a story; an empty field would
// make Discord reject the whole message.
func TestTeamListFallsBackToLifetime(t *testing.T) {
	got := teamList([]Membership{
		{TeamName: "Retired Crew", PointsTotal: 1500000},
		{TeamName: "Old Guard", PointsTotal: 900},
	})
	if strings.TrimSpace(got) == "" {
		t.Fatal("empty field value; Discord rejects the message")
	}
	if strings.Contains(got, "/day") {
		t.Errorf("showed a rate for teams producing nothing:\n%s", got)
	}
	if !strings.Contains(got, "1.5M") || !strings.Contains(got, "Retired Crew") {
		t.Errorf("lifetime totals missing:\n%s", got)
	}
}

func TestTeamListEscapesMarkdown(t *testing.T) {
	got := teamList([]Membership{{TeamName: "**WINNERS** `x`", PointsPerDay: 10}})
	if strings.Contains(got, "**WINNERS**") {
		t.Errorf("markdown in a team name left live:\n%s", got)
	}
	// An unescaped backtick would close the code span around the rate.
	if strings.Count(got, "`")%2 != 0 {
		t.Errorf("unbalanced code span:\n%s", got)
	}
}

// TestStreakAtFloorIsNotStatedAsFact: a run reaching the first day on record is a
// lower bound. Someone folding daily for a decade would otherwise be told they have a
// five-day streak, which is the site's own age wearing their name.
func TestStreakAtFloorIsNotStatedAsFact(t *testing.T) {
	got := streakText(&Streak{Current: 5, Longest: 5, ActiveDays: 5, AtCollectionFloor: true})
	if !strings.Contains(got, "on record") {
		t.Errorf("a floor was presented as a measured streak: %q", got)
	}
	if strings.Contains(got, "best") {
		t.Errorf("a best-run figure is meaningless at the floor: %q", got)
	}
	if got := streakText(&Streak{Current: 12, Longest: 40}); !strings.Contains(got, "best 40 days") {
		t.Errorf("a real streak lost its best run: %q", got)
	}
	if got := streakText(&Streak{Current: 1, Longest: 1}); strings.Contains(got, "1 days") {
		t.Errorf("plural on a single day: %q", got)
	}
	if streakText(nil) != "" || streakText(&Streak{}) != "" {
		t.Error("an absent streak should render no field at all")
	}
}

// Durations reach people, so they must not read like log output.
func TestHumanDur(t *testing.T) {
	for _, c := range []struct {
		in   time.Duration
		want string
	}{
		{30 * time.Second, "under a minute"},
		{32 * time.Minute, "32m"},
		{time.Hour, "1 hour"},
		{100 * time.Minute, "1h 40m"},
		{2 * time.Hour, "2 hours"},
		{91 * time.Hour, "3.8 days"},
		{48 * time.Hour, "2 days"},
		{40 * 24 * time.Hour, "40 days"},
	} {
		if got := humanDur(c.in); got != c.want {
			t.Errorf("humanDur(%s) = %q, want %q", c.in, got, c.want)
		}
	}
}
