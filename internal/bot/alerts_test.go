package bot

import (
	"strings"
	"testing"
	"time"
)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// newAlert builds one seeded from a starting reading, the way /alert add does.
func newAlert(t AlertType, threshold int64, start entity, now time.Time) *Alert {
	a := &Alert{Type: t, Kind: "donor", Target: start.Name, Label: start.Name, Threshold: threshold}
	seed(a, start, now)
	return a
}

// step runs one evaluation and commits the resulting state, as the watcher does.
func step(a *Alert, e entity, now time.Time) (bool, string) {
	fire, headline, _, next := evaluate(a, e, now)
	a.Seen = next
	return fire, headline
}

// TestANewAlertDoesNotAnnounceThePast is the property the whole design rests on.
//
// Every rule fires on a transition, and without seeding at creation the first
// evaluation compares against a zero value — so a donor who passed a billion points
// last year, is already inside the target rank, and has been idle for a month would
// set off all three at once, the moment somebody subscribed.
func TestANewAlertDoesNotAnnounceThePast(t *testing.T) {
	now := at("2026-08-06T12:00:00Z")
	past := entity{Name: "DH", Rank: 400, PointsTotal: 6_211_286_931, Last24h: 0}

	for _, tc := range []struct {
		typ       AlertType
		threshold int64
	}{
		{AlertMilestone, 0},
		{AlertRank, 1000},
		{AlertIdle, 24},
		{AlertResumed, 0},
	} {
		a := newAlert(tc.typ, tc.threshold, past, now)
		if fire, headline := step(a, past, now.Add(time.Hour)); fire {
			t.Errorf("%s fired on an unchanged reading: %q", tc.typ, headline)
		}
	}
}

func TestMilestoneFiresOncePerStep(t *testing.T) {
	now := at("2026-08-06T12:00:00Z")
	start := entity{Name: "DH", PointsTotal: 940_000_000}
	a := newAlert(AlertMilestone, 0, start, now)

	// Still short of a billion.
	if fire, _ := step(a, entity{Name: "DH", PointsTotal: 999_999_999}, now); fire {
		t.Error("fired before the milestone")
	}
	fire, headline := step(a, entity{Name: "DH", PointsTotal: 1_000_000_001}, now)
	if !fire {
		t.Fatal("did not fire on crossing 1B")
	}
	if !strings.Contains(headline, "1B") {
		t.Errorf("headline does not name the milestone: %q", headline)
	}
	// Growing past it must stay quiet until the next step at 2B.
	if fire, headline := step(a, entity{Name: "DH", PointsTotal: 1_500_000_000}, now); fire {
		t.Errorf("fired twice for the same milestone: %q", headline)
	}
	if fire, _ := step(a, entity{Name: "DH", PointsTotal: 2_000_000_000}, now); !fire {
		t.Error("did not fire on the next step")
	}
}

// A single evaluation must not skip steps silently, but nor should it fire five times:
// a donor who jumps several milestones between two publishes gets the one they reached.
func TestMilestoneSkippingSeveralAnnouncesTheHighest(t *testing.T) {
	now := at("2026-08-06T12:00:00Z")
	a := newAlert(AlertMilestone, 0, entity{Name: "T", PointsTotal: 1_000_000}, now)
	fire, headline := step(a, entity{Name: "T", PointsTotal: 60_000_000}, now)
	if !fire || !strings.Contains(headline, "50M") {
		t.Fatalf("fire=%v headline=%q, want the 50M step", fire, headline)
	}
	if fire, _ := step(a, entity{Name: "T", PointsTotal: 60_000_000}, now); fire {
		t.Error("fired again on an unchanged total")
	}
}

func TestRankFiresOnEntryAndRearmsOnLeaving(t *testing.T) {
	now := at("2026-08-06T12:00:00Z")
	a := newAlert(AlertRank, 1000, entity{Name: "DH", Rank: 1500}, now)

	if fire, _ := step(a, entity{Name: "DH", Rank: 1200}, now); fire {
		t.Error("fired while still outside the target")
	}
	fire, headline := step(a, entity{Name: "DH", Rank: 998}, now)
	if !fire {
		t.Fatal("did not fire on reaching the rank")
	}
	if !strings.Contains(headline, "#998") {
		t.Errorf("headline does not carry the rank: %q", headline)
	}
	// Holding the rank is not news every hour.
	if fire, _ := step(a, entity{Name: "DH", Rank: 990}, now); fire {
		t.Error("fired again while still inside the target")
	}
	// Slipping out re-arms it, so regaining the place is worth hearing.
	step(a, entity{Name: "DH", Rank: 1100}, now)
	if fire, _ := step(a, entity{Name: "DH", Rank: 999}, now); !fire {
		t.Error("did not fire on re-entering the rank")
	}
}

// An alert created while the target is already inside the rank must wait for a real
// crossing rather than congratulating them on standing still.
func TestRankCreatedInsideWaitsForARealCrossing(t *testing.T) {
	now := at("2026-08-06T12:00:00Z")
	a := newAlert(AlertRank, 1000, entity{Name: "DH", Rank: 400}, now)
	if fire, _ := step(a, entity{Name: "DH", Rank: 380}, now); fire {
		t.Error("fired for a rank it already held when created")
	}
	step(a, entity{Name: "DH", Rank: 1400}, now)
	if fire, _ := step(a, entity{Name: "DH", Rank: 900}, now); !fire {
		t.Error("did not fire after leaving and returning")
	}
}

func TestIdleWaitsOutTheThreshold(t *testing.T) {
	start := at("2026-08-06T00:00:00Z")
	a := newAlert(AlertIdle, 24, entity{Name: "rig", Last24h: 5_000_000}, start)

	// Production stops. The clock starts here, not at creation.
	if fire, _ := step(a, entity{Name: "rig", Last24h: 0}, start.Add(1*time.Hour)); fire {
		t.Error("fired the moment production stopped, before the threshold")
	}
	if fire, _ := step(a, entity{Name: "rig", Last24h: 0}, start.Add(20*time.Hour)); fire {
		t.Error("fired before 24 hours of silence")
	}
	fire, headline := step(a, entity{Name: "rig", Last24h: 0}, start.Add(26*time.Hour))
	if !fire {
		t.Fatal("did not fire after the threshold")
	}
	if !strings.Contains(headline, "gone quiet") {
		t.Errorf("unexpected headline: %q", headline)
	}
	// Still quiet is not news again.
	if fire, _ := step(a, entity{Name: "rig", Last24h: 0}, start.Add(50*time.Hour)); fire {
		t.Error("repeated the idle alert while still idle")
	}
	// Coming back and dying again is.
	step(a, entity{Name: "rig", Last24h: 1_000_000}, start.Add(60*time.Hour))
	step(a, entity{Name: "rig", Last24h: 0}, start.Add(61*time.Hour))
	if fire, _ := step(a, entity{Name: "rig", Last24h: 0}, start.Add(90*time.Hour)); !fire {
		t.Error("did not fire for a second outage")
	}
}

func TestResumedNeedsAGapFirst(t *testing.T) {
	now := at("2026-08-06T12:00:00Z")
	// Created while already producing: resuming means nothing yet.
	a := newAlert(AlertResumed, 0, entity{Name: "rig", Last24h: 9_000_000}, now)
	if fire, _ := step(a, entity{Name: "rig", Last24h: 9_500_000}, now); fire {
		t.Error("fired without a gap to resume from")
	}
	step(a, entity{Name: "rig", Last24h: 0}, now.Add(time.Hour))
	fire, headline := step(a, entity{Name: "rig", Last24h: 3_000_000}, now.Add(2*time.Hour))
	if !fire {
		t.Fatal("did not fire on resuming")
	}
	if !strings.Contains(headline, "folding again") {
		t.Errorf("unexpected headline: %q", headline)
	}
	if fire, _ := step(a, entity{Name: "rig", Last24h: 4_000_000}, now.Add(3*time.Hour)); fire {
		t.Error("repeated while still producing")
	}
}

// The daily summary is the one rule driven by the clock, and upstream publishes drift
// about ten seconds later each hour — so a 24-hour interval would walk the alert
// forward through the day until it crossed midnight and skipped one entirely.
func TestDailyFiresOncePerUTCDay(t *testing.T) {
	e := entity{Name: "DH", Rank: 1008, PointsTotal: 6e9, Last24h: 45e6, PerDay: 77e6}
	a := newAlert(AlertDaily, 12, e, at("2026-08-06T09:00:00Z"))

	if fire, _ := step(a, e, at("2026-08-06T11:00:00Z")); fire {
		t.Error("fired before the hour")
	}
	if fire, _ := step(a, e, at("2026-08-06T12:00:04Z")); !fire {
		t.Fatal("did not fire at the hour")
	}
	for _, when := range []string{"2026-08-06T13:00:00Z", "2026-08-06T23:59:00Z"} {
		if fire, _ := step(a, e, at(when)); fire {
			t.Errorf("fired twice on the same day (%s)", when)
		}
	}
	// Next day, and slightly later than yesterday, as a drifting publish would be.
	if fire, _ := step(a, e, at("2026-08-07T12:00:31Z")); !fire {
		t.Error("did not fire the next day")
	}
}

func TestMilestoneLadder(t *testing.T) {
	for _, tc := range []struct{ points, want int64 }{
		{999_999, 0},
		{1_000_000, 1_000_000},
		{1_999_999, 1_000_000},
		{2_000_000, 2_000_000},
		{6_211_286_931, 5_000_000_000},
		{52_243_537_611_990, 50_000_000_000_000},
	} {
		if got := milestoneAtOrBelow(tc.points); got != tc.want {
			t.Errorf("milestoneAtOrBelow(%d) = %d, want %d", tc.points, got, tc.want)
		}
	}
	if got := nextMilestone(5_000_000_000); got != 10_000_000_000 {
		t.Errorf("nextMilestone(5B) = %d, want 10B", got)
	}
}

/* ---------------------------------------------------------------- store --- */

func TestAlertStoreRoundTrip(t *testing.T) {
	path := t.TempDir() + "/alerts.json"
	s, err := OpenAlerts(path)
	if err != nil {
		t.Fatal(err)
	}
	a := &Alert{GuildID: "g1", ChannelID: "c1", Type: AlertIdle, Kind: "donor",
		Target: "DH", Label: "DH", Threshold: 24, CreatedAt: time.Now().UTC()}
	if err := s.Add(a); err != nil {
		t.Fatal(err)
	}
	if a.ID == "" {
		t.Fatal("Add did not assign an id")
	}

	// The same alert twice would double every message it ever sends.
	dupe := *a
	dupe.ID = ""
	if err := s.Add(&dupe); err == nil {
		t.Error("added a duplicate alert")
	}

	// State written by the watcher has to survive a restart, or a bot that restarts
	// hourly re-announces everything it has ever announced.
	a.Seen.Milestone = 5_000_000_000
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	again, err := OpenAlerts(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := again.Get(a.ID)
	if !ok {
		t.Fatal("alert did not survive a reopen")
	}
	if got.Seen.Milestone != 5_000_000_000 {
		t.Errorf("state lost across reopen: %+v", got.Seen)
	}
	if got.Target != "DH" || got.Threshold != 24 {
		t.Errorf("configuration lost across reopen: %+v", got)
	}

	if _, err := again.Remove(a.ID); err != nil {
		t.Fatal(err)
	}
	if again.Count() != 0 {
		t.Error("remove left it behind")
	}
}

func TestAlertScopeIsPerGuild(t *testing.T) {
	s, _ := OpenAlerts(t.TempDir() + "/a.json")
	mine := &Alert{GuildID: "g1", ChannelID: "c1", Type: AlertDaily, Kind: "team", Target: "0", Label: "Ours"}
	theirs := &Alert{GuildID: "g2", ChannelID: "c9", Type: AlertDaily, Kind: "team", Target: "0", Label: "Theirs"}
	if err := s.Add(mine); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(theirs); err != nil {
		t.Fatal(err)
	}
	got := s.InScope("g1", "c1")
	if len(got) != 1 || got[0].Label != "Ours" {
		t.Fatalf("guild scope leaked: %+v", got)
	}
	// And a DM sees only its own channel, having no guild to belong to.
	dm := &Alert{ChannelID: "dm1", Type: AlertDaily, Kind: "team", Target: "0", Label: "Mine"}
	if err := s.Add(dm); err != nil {
		t.Fatal(err)
	}
	if got := s.InScope("", "dm1"); len(got) != 1 || got[0].Label != "Mine" {
		t.Fatalf("DM scope wrong: %+v", got)
	}
}

func TestAlertChannelCap(t *testing.T) {
	s, _ := OpenAlerts(t.TempDir() + "/a.json")
	for i := 0; i < MaxPerChannel; i++ {
		if err := s.Add(&Alert{GuildID: "g", ChannelID: "c", Type: AlertRank, Kind: "donor",
			Target: "DH", Label: "DH", Threshold: int64(i + 1)}); err != nil {
			t.Fatalf("add %d: %v", i, err)
		}
	}
	err := s.Add(&Alert{GuildID: "g", ChannelID: "c", Type: AlertRank, Kind: "donor",
		Target: "DH", Label: "DH", Threshold: 9999})
	if err == nil {
		t.Fatal("channel cap not enforced")
	}
	// A different channel is unaffected.
	if err := s.Add(&Alert{GuildID: "g", ChannelID: "other", Type: AlertRank, Kind: "donor",
		Target: "DH", Label: "DH", Threshold: 1}); err != nil {
		t.Errorf("cap leaked across channels: %v", err)
	}
}

func TestParseTarget(t *testing.T) {
	for _, tc := range []struct {
		in           string
		kind, target string
		tagged       bool
	}{
		{"t:51", "team", "51", true},
		{"d:Anonymous", "donor", "Anonymous", true},
		// A donor whose name is a number is the case the tagging exists for.
		{"d:51", "donor", "51", true},
		{"Anonymous", "", "Anonymous", false},
		{"  51  ", "", "51", false},
	} {
		k, tg, ok := parseTarget(tc.in)
		if k != tc.kind || tg != tc.target || ok != tc.tagged {
			t.Errorf("parseTarget(%q) = (%q,%q,%v), want (%q,%q,%v)",
				tc.in, k, tg, ok, tc.kind, tc.target, tc.tagged)
		}
	}
}

func TestDescribeReadsAsASentence(t *testing.T) {
	for _, tc := range []struct {
		a    Alert
		want string
	}{
		{Alert{Type: AlertRank, Label: "DH", Threshold: 1000}, "DH — reaching rank #1,000"},
		{Alert{Type: AlertIdle, Label: "rig", Threshold: 1}, "rig — no points for 1 hour"},
		{Alert{Type: AlertDaily, Label: "USA", Threshold: 9}, "USA — once a day at 09:00 UTC"},
		{Alert{Type: AlertMilestone, Label: "DH"}, "DH — every 1M/2M/5M step"},
	} {
		if got := tc.a.Describe(); got != tc.want {
			t.Errorf("Describe() = %q, want %q", got, tc.want)
		}
	}
}

func TestThresholdValidation(t *testing.T) {
	if checkThreshold(AlertRank, 0) == "" {
		t.Error("accepted rank 0")
	}
	if checkThreshold(AlertRank, 1) != "" {
		t.Error("rejected rank 1")
	}
	// Below an hour would fire on the gap between two publishes rather than on an outage.
	if checkThreshold(AlertIdle, 0) == "" {
		t.Error("accepted a zero-hour idle window")
	}
	if checkThreshold(AlertDaily, 24) == "" {
		t.Error("accepted hour 24")
	}
	if checkThreshold(AlertDaily, 0) != "" {
		t.Error("rejected midnight")
	}
	if checkThreshold(AlertMilestone, 0) != "" {
		t.Error("milestone takes no threshold and should not be validated")
	}
}
