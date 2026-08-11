package api

import (
	"strings"
	"testing"
)

// TestPageMeta covers the head a crawler reads.
//
// Every URL on this site used to carry the same title and the same description,
// because the page is drawn client-side and the shell is one file. The failure was
// invisible from a browser and total from outside it: two million donor pages that a
// search engine could only read as duplicates of each other.
func TestPageMeta(t *testing.T) {
	s := fixture(t)

	seen := map[string]bool{}
	check := func(path string, wantIn ...string) PageMeta {
		t.Helper()
		m, ok := s.PageMeta(path)
		if !ok {
			t.Fatalf("%s: no metadata", path)
		}
		if m.Title == "" || m.Description == "" {
			t.Fatalf("%s: empty title or description: %+v", path, m)
		}
		// The whole point is that pages differ. A repeated title is the bug.
		if seen[m.Title] {
			t.Errorf("%s: title %q already used by another page", path, m.Title)
		}
		seen[m.Title] = true
		for _, w := range wantIn {
			if !strings.Contains(m.Title+" "+m.Description, w) {
				t.Errorf("%s: %q missing from\n  title: %s\n  desc:  %s", path, w, m.Title, m.Description)
			}
		}
		// Descriptions are cut around 155 characters in a result; the distinguishing
		// fact has to survive that.
		if len(m.Description) > 320 {
			t.Errorf("%s: description is %d chars, far past any useful truncation", path, len(m.Description))
		}
		return m
	}

	// A donor's own numbers, not the site's boilerplate.
	d := check("/donors/DH", "DH", "ranked #", "points")
	if d.NoIndex {
		t.Error("a real donor page is marked noindex")
	}
	check("/donors/toTOW", "toTOW")
	check("/donors/DH/rivals", "DH", "rivals")

	// A team is identified by its name, which is what people search for.
	check("/teams/32", "overclockers", "ranked #")
	check("/teams/51", "Alliance")
	check("/teams/32/rivals", "overclockers")

	check("/teams", "teams")
	check("/donors", "donors")
	check("/api", "API")
	check("/agents")
	check("/fold")

	// A name nobody holds still renders, but must not be indexed — this is the URL a
	// sitemap keeps pointing at after someone disappears upstream.
	gone, ok := s.PageMeta("/donors/nobody-by-that-name")
	if !ok {
		t.Fatal("an unknown donor produced no metadata at all")
	}
	if !gone.NoIndex {
		t.Error("an unknown donor is indexable; that is a soft 404")
	}
	if missing, ok := s.PageMeta("/teams/999999"); !ok || !missing.NoIndex {
		t.Errorf("unknown team: ok=%v meta=%+v, want noindex", ok, missing)
	}
	// A search result page is a view of pages that each already exist.
	if sr, ok := s.PageMeta("/search"); !ok || !sr.NoIndex {
		t.Errorf("/search: ok=%v meta=%+v, want noindex", ok, sr)
	}

	// Before the first ingest there is nothing true to say, and saying nothing leaves
	// the shell's own defaults in place rather than publishing an invented number.
	var empty Server
	if _, ok := empty.PageMeta("/donors/DH"); ok {
		t.Error("described a donor with no snapshot loaded")
	}
}

func TestCompact(t *testing.T) {
	for in, want := range map[int]string{
		999: "999", 12000: "12k", 129964: "129k", 2123908: "2.1M",
	} {
		if got := compact(in); got != want {
			t.Errorf("compact(%d) = %q, want %q", in, got, want)
		}
	}
}
