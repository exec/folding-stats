package parse

import (
	"strconv"
	"strings"
	"testing"
)

const preamble = "Sun Aug 02 21:29:05 GMT 2026\nteam\tteamname\tscore\twu\n"

func teamFeed(rows ...string) string { return preamble + strings.Join(rows, "\n") + "\n" }

func userFeed(rows ...string) string {
	return "Sun Aug 02 21:29:05 GMT 2026\nname\tscore\twu\tteam\n" + strings.Join(rows, "\n") + "\n"
}

func TestTeamWellFormed(t *testing.T) {
	in := teamFeed(
		"0\tDefault (No team specified)\t8213749748944\t364747804",
		"182116\tAtheists, Skeptics, & Humanists  -  ASH Folding\t251359529740\t2884335",
	)
	s := NewTeamScanner(strings.NewReader(in))
	var got []TeamRow
	for s.Scan() {
		got = append(got, s.Row())
	}
	if err := s.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2", len(got))
	}
	if got[0].ID != 0 || got[0].Score != 8213749748944 || got[0].WUs != 364747804 {
		t.Errorf("row 0 = %+v", got[0])
	}
	// Runs of double-spaces inside names must survive: splitting on whitespace
	// instead of tabs would mangle this one.
	if want := "Atheists, Skeptics, & Humanists  -  ASH Folding"; got[1].Name != want {
		t.Errorf("name = %q, want %q", got[1].Name, want)
	}
}

func TestTeamNameSpanningLines(t *testing.T) {
	// Real records from the 2026-08-02 feed.
	in := teamFeed(
		"231018\tThe Orion Initiative\t3449738\t74",
		"151775\tdiscworld",
		"\t3448577\t434",
		"2614\tjan_from_",
		"jan from md\t1476019\t19",
		"245999\tUNICA\t1475933\t857",
	)
	s := NewTeamScanner(strings.NewReader(in))
	var got []TeamRow
	for s.Scan() {
		got = append(got, s.Row())
	}
	if len(got) != 4 {
		t.Fatalf("got %d rows, want 4: %+v", len(got), got)
	}
	if got[1].ID != 151775 || got[1].Name != "discworld\n" || got[1].Score != 3448577 {
		t.Errorf("embedded-newline row = %+v", got[1])
	}
	if got[2].ID != 2614 || got[2].Name != "jan_from_\njan from md" || got[2].WUs != 19 {
		t.Errorf("multi-line name row = %+v", got[2])
	}
	// The record following a repaired one must still parse — a desynchronised
	// reader would lose it.
	if got[3].ID != 245999 || got[3].Name != "UNICA" {
		t.Errorf("row after repair = %+v", got[3])
	}
	if s.Stats().Malformed != 0 {
		t.Errorf("Malformed = %d, want 0 (all rows recoverable)", s.Stats().Malformed)
	}
}

func TestTeamNameContainingTab(t *testing.T) {
	in := teamFeed(
		"87792\t\tGreater Hartford Academy of Mathematics and Science\t15553\t11",
		"68207\t772\t15553\t61",
	)
	s := NewTeamScanner(strings.NewReader(in))
	var got []TeamRow
	for s.Scan() {
		got = append(got, s.Row())
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2", len(got))
	}
	if want := "\tGreater Hartford Academy of Mathematics and Science"; got[0].Name != want {
		t.Errorf("tabbed name = %q, want %q", got[0].Name, want)
	}
	// A team legitimately named "772" must not be mistaken for a numeric column.
	if got[1].ID != 68207 || got[1].Name != "772" || got[1].Score != 15553 {
		t.Errorf("numeric-looking name row = %+v", got[1])
	}
}

func TestUserWellFormed(t *testing.T) {
	in := userFeed(
		"Anonymous\t3850901836542\t153536384\t0",
		"84036980\t226150637550\t656911\t31403",
	)
	s := NewUserScanner(strings.NewReader(in))
	var got []UserRow
	for s.Scan() {
		got = append(got, s.Row())
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2", len(got))
	}
	if got[0].Name != "Anonymous" || got[0].TeamID != 0 {
		t.Errorf("row 0 = %+v", got[0])
	}
	// A purely numeric donor name must stay a name.
	if got[1].Name != "84036980" || got[1].Score != 226150637550 {
		t.Errorf("numeric name row = %+v", got[1])
	}
}

func TestUserPathologicalNames(t *testing.T) {
	// Every one of these is a real record from the 2026-08-02 feed.
	in := userFeed(
		"zmd\t1760\t1\t31457",
		"\terrabyte\t1760\t1\t24", // leading tab inside the name
		"oslo\t60p\t186\t1\t0",    // tab inside the name
		"dono",                    // name ends with a newline
		"\t186\t1\t49078",
		"/", // name begins with "/\n"
		"dy-Houston\t180\t1\t0",
		"", // name begins with a newline
		"oam\t11473\t58\t49078",
		"i/-",
		"\t17\t5\t93",
		"Osmow\t186\t1\t34479",
	)
	s := NewUserScanner(strings.NewReader(in))
	var got []UserRow
	for s.Scan() {
		got = append(got, s.Row())
	}
	if err := s.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}

	want := []UserRow{
		{"zmd", 1760, 1, 31457},
		{"\terrabyte", 1760, 1, 24},
		{"oslo\t60p", 186, 1, 0},
		{"dono\n", 186, 1, 49078},
		{"/\ndy-Houston", 180, 1, 0},
		{"\noam", 11473, 58, 49078},
		{"i/-\n", 17, 5, 93},
		{"Osmow", 186, 1, 34479},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d rows, want %d:\n%+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d:\n got %+v\nwant %+v", i, got[i], want[i])
		}
	}
	if s.Stats().Malformed != 0 {
		t.Errorf("Malformed = %d, want 0", s.Stats().Malformed)
	}
}

func TestGarbageResyncsWithinOneRecord(t *testing.T) {
	// A corrupt region must not desynchronise the rest of the feed.
	//
	// The line immediately after junk may be absorbed into a garbage "name" —
	// that case is genuinely indistinguishable from a legitimate name containing a
	// newline (see "/\ndy-Houston" in TestUserPathologicalNames), so it cannot be
	// fixed without losing real data. The guarantee is that absorption stops
	// there: the *second* row after the junk must parse cleanly.
	junk := strings.Repeat(strings.Repeat("nonsense ", 10)+"\n", maxAccumulate+3)
	in := userFeed("before\t100\t1\t5") + junk + "adjacent\t200\t2\t6\nclean\t300\t3\t7\n"

	s := NewUserScanner(strings.NewReader(in))
	var got []UserRow
	for s.Scan() {
		got = append(got, s.Row())
	}
	if err := s.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	if len(got) < 2 {
		t.Fatalf("got %d rows, want at least 2: %+v", len(got), got)
	}
	if got[0].Name != "before" || got[0].Score != 100 {
		t.Errorf("row before junk = %+v, want 'before'", got[0])
	}
	last := got[len(got)-1]
	if last.Name != "clean" || last.Score != 300 || last.WUs != 3 || last.TeamID != 7 {
		t.Errorf("second row after junk = %+v, want clean/300/3/7", last)
	}
	if s.Stats().Malformed == 0 {
		t.Error("Malformed = 0, want the junk lines counted")
	}
	// Memory safety: the accumulator must never grow without bound.
	if len(got) > 4 {
		t.Errorf("got %d rows, want the junk collapsed into few records", len(got))
	}
}

func TestStatsCaptureHeader(t *testing.T) {
	s := NewTeamScanner(strings.NewReader(teamFeed("1\tx\t2\t3")))
	for s.Scan() {
	}
	st := s.Stats()
	if st.Timestamp != "Sun Aug 02 21:29:05 GMT 2026" {
		t.Errorf("Timestamp = %q", st.Timestamp)
	}
	if st.Header != "team\tteamname\tscore\twu" {
		t.Errorf("Header = %q", st.Header)
	}
	if st.Rows != 1 {
		t.Errorf("Rows = %d, want 1", st.Rows)
	}
}

func TestEmptyFeedIsAnError(t *testing.T) {
	s := NewUserScanner(strings.NewReader(""))
	if s.Scan() {
		t.Error("Scan succeeded on empty input")
	}
	if s.Err() == nil {
		t.Error("want an error for an empty feed, got nil")
	}
}

func TestNegativeAndOversizedNumbersRejected(t *testing.T) {
	// Scores are cumulative and never negative; a "-" would indicate corruption,
	// and treating it as a name fragment is safer than accepting it.
	for _, bad := range []string{"-5", "99999999999999999999", "1.5", " 12"} {
		if _, ok := atoi64(bad); ok {
			t.Errorf("atoi64(%q) accepted, want rejected", bad)
		}
	}
	if v, ok := atoi64("8213749748944"); !ok || v != 8213749748944 {
		t.Errorf("atoi64 rejected a valid score")
	}
}

func TestSparseTeamIDIsBounded(t *testing.T) {
	if _, ok := atoi32(strconv.Itoa(MaxTeamID + 1)); ok {
		t.Fatal("team ID beyond the allocation bound was accepted")
	}
}

func TestOverlongPhysicalLineIsDiscardedAndScannerResynchronizes(t *testing.T) {
	in := "ts\nteam\tteamname\tscore\twu\n" + strings.Repeat("x", 2048) + "\n32\toc\t100\t1\n"
	s := NewTeamScanner(strings.NewReader(in))
	if !s.Scan() {
		t.Fatalf("valid row after overlong line was lost: %v", s.Err())
	}
	if got := s.Row(); got.ID != 32 || got.Score != 100 {
		t.Fatalf("row = %+v", got)
	}
	if s.Stats().Malformed == 0 {
		t.Fatal("overlong line was not counted malformed")
	}
}
