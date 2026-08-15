package api

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func badgeGet(t *testing.T, srv *Server, path string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: status %d", path, rec.Code)
	}
	return rec.Body.String()
}

func segments(svg string) []string {
	var out []string
	for _, m := range regexp.MustCompile(`<text[^>]*>([^<]*)</text>`).FindAllStringSubmatch(svg, -1) {
		out = append(out, m[1])
	}
	return out
}

// A badge is read where nothing around it supplies context, so it has to say what is
// being measured and in what unit. It used to render "ppd | 31.0M", which names neither
// the project nor the quantity.
func TestBadgeNamesTheSourceAndTheUnit(t *testing.T) {
	srv := fixture(t)
	for _, tc := range []struct{ metric, wantSuffix string }{
		{"ppd", " PPD"},
		{"points", " points"},
		{"rank", ""},
	} {
		got := segments(badgeGet(t, srv, "/badge/team/32?metric="+tc.metric))
		if len(got) != 2 {
			t.Fatalf("metric %s: %d segments, want 2: %v", tc.metric, len(got), got)
		}
		if got[0] != badgeSource {
			t.Errorf("metric %s: left segment %q, want %q", tc.metric, got[0], badgeSource)
		}
		if tc.wantSuffix != "" && !strings.HasSuffix(got[1], tc.wantSuffix) {
			t.Errorf("metric %s: value %q carries no unit", tc.metric, got[1])
		}
		if tc.metric == "rank" && !strings.HasPrefix(got[1], "#") {
			t.Errorf("rank value %q should read as a position", got[1])
		}
	}
}

// ?name=1 adds the entity between the source and the figure, for somewhere with no
// surrounding page to identify it.
func TestBadgeNameSegmentIsOptional(t *testing.T) {
	srv := fixture(t)
	if got := segments(badgeGet(t, srv, "/badge/team/32?metric=ppd")); len(got) != 2 {
		t.Errorf("without ?name: %v", got)
	}
	got := segments(badgeGet(t, srv, "/badge/team/32?metric=ppd&name=1"))
	if len(got) != 3 {
		t.Fatalf("with ?name=1: %d segments, want 3: %v", len(got), got)
	}
	if got[0] != badgeSource || !strings.HasSuffix(got[2], "PPD") {
		t.Errorf("segments out of order: %v", got)
	}
}

// The name is chosen by the public and the badge is embedded on somebody else's page.
// Escaping stops it becoming markup; this stops it becoming a layout.
func TestBadgeTextNeutralisesHostileNames(t *testing.T) {
	for _, tc := range []struct{ name, in, wantNot string }{
		{"right-to-left override", "abc‮def", "‮"},
		{"zero width", "ab​cd", "​"},
		{"control character", "ab\x01cd", "\x01"},
	} {
		if got := badgeText(tc.in, 40); strings.Contains(got, tc.wantNot) {
			t.Errorf("%s: %q survived in %q", tc.name, tc.wantNot, got)
		}
	}
	// Newlines become spaces rather than vanishing, so the name still reads.
	if got := badgeText("a\nb", 40); got != "a b" {
		t.Errorf("newline handling = %q, want %q", got, "a b")
	}
	// Long names are cut, because a badge is not a banner.
	long := badgeText(strings.Repeat("x", 60), maxBadgeName)
	if !strings.HasSuffix(long, "…") || len([]rune(long)) > maxBadgeName+1 {
		t.Errorf("truncation = %q (%d runes)", long, len([]rune(long)))
	}
}

// Width is measured in runes, not bytes. Sized from len(), five Japanese characters
// measured as fifteen and produced a badge three times wider than its text.
func TestBadgeWidthIsRuneAware(t *testing.T) {
	latin := badgeWidth("abcde") // 5 narrow glyphs
	cjk := badgeWidth("チーム日本")   // 5 wide glyphs, 15 bytes
	if cjk <= latin {
		t.Errorf("wide glyphs measured %d, narrow %d — wide should be greater", cjk, latin)
	}
	if cjk > latin*3 {
		t.Errorf("wide glyphs measured %d against narrow %d — that is byte-shaped, not rune-shaped", cjk, latin)
	}
}

// The declared width has to match what is drawn, or the text sits off-centre or clipped
// wherever it is embedded.
func TestBadgeWidthMatchesItsSegments(t *testing.T) {
	svg := string(renderBadge("t", []string{"folding@home", "31.0M PPD"}))
	declared, err := strconv.Atoi(regexp.MustCompile(`width="(\d+)"`).FindStringSubmatch(svg)[1])
	if err != nil {
		t.Fatal(err)
	}
	sum := 0
	for _, m := range regexp.MustCompile(`<rect x="\d+" width="(\d+)" height="20" fill="#[0-9a-f]{6}"`).FindAllStringSubmatch(svg, -1) {
		n, _ := strconv.Atoi(m[1])
		sum += n
	}
	if declared != sum {
		t.Errorf("svg declares width %d but its segments total %d", declared, sum)
	}
}
