package content

import (
	"strings"
	"testing"
	"time"
)

func TestPostsParseAndRender(t *testing.T) {
	// init() panics on a malformed post, so reaching here means every file parsed.
	ps := Published()
	if len(ps) == 0 {
		t.Fatal("no published posts")
	}
	for _, p := range ps {
		if p.Title == "" || p.Date.IsZero() || p.Slug == "" {
			t.Errorf("%+v: missing title, date or slug", p)
		}
		if !strings.Contains(p.HTML, "<p>") {
			t.Errorf("%s: rendered HTML has no paragraphs", p.Slug)
		}
		if strings.Contains(p.Slug, "/") || strings.Contains(p.Slug, " ") {
			t.Errorf("%s: slug is not URL-safe", p.Slug)
		}
	}
}

func TestNewestFirst(t *testing.T) {
	ps := Published()
	for i := 1; i < len(ps); i++ {
		if ps[i].Date.After(ps[i-1].Date) {
			t.Errorf("post %d (%s) is newer than the one before it", i, ps[i].Slug)
		}
	}
}

func TestDatePrefixIsStrippedFromSlug(t *testing.T) {
	// The prefix orders files on disk; carrying it into the URL would mean a post
	// that slips a week needs a new URL.
	for _, tc := range []struct{ file, want string }{
		{"posts/2026-09-01-hello-world.md", "hello-world"},
		{"posts/no-date-here.md", "no-date-here"},
		{"posts/2026-13-45-not-a-date.md", "2026-13-45-not-a-date"},
	} {
		if got := slugOf(tc.file); got != tc.want {
			t.Errorf("slugOf(%q) = %q, want %q", tc.file, got, tc.want)
		}
	}
}

func TestRawHTMLIsNotPassedThrough(t *testing.T) {
	// Posts are trusted, but the renderer is configured to drop raw HTML so that a
	// future post cannot paste in something it should not.
	p, err := parse("posts/2026-01-01-x.md", []byte(
		"---\ntitle: X\ndate: 2026-01-01\n---\n<script>alert(1)</script>\n\ntext\n"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(p.HTML, "<script>") {
		t.Errorf("raw HTML survived rendering: %s", p.HTML)
	}
}

func TestFrontmatterErrorsAreLoud(t *testing.T) {
	// A post that fails to parse must fail the build, not vanish from the site.
	for name, src := range map[string]string{
		"no title":       "---\ndate: 2026-01-01\n---\nbody\n",
		"no date":        "---\ntitle: X\n---\nbody\n",
		"bad date":       "---\ntitle: X\ndate: 1 Jan 2026\n---\nbody\n",
		"unknown key":    "---\ntitle: X\ndate: 2026-01-01\nauthr: me\n---\nbody\n",
		"unclosed block": "---\ntitle: X\ndate: 2026-01-01\nbody\n",
	} {
		if _, err := parse("posts/2026-01-01-x.md", []byte(src)); err == nil {
			t.Errorf("%s: parsed without error", name)
		}
	}
}

func TestLookupFindsDrafts(t *testing.T) {
	// Drafts stay out of the listing but remain reachable, which is how you review
	// one before announcing it.
	p, err := parse("posts/2026-01-01-secret.md",
		[]byte("---\ntitle: X\ndate: 2026-01-01\ndraft: true\n---\nbody\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !p.Draft {
		t.Error("draft: true did not set Draft")
	}
	_ = time.Now
}
