// Package content holds the site's written posts and renders them to HTML.
//
// Posts are Markdown files in this directory, compiled into the binary. That is a
// deliberate choice over a database and an admin UI:
//
//   - The site is already a single binary with no build step. Posts add no
//     infrastructure — no schema, no migrations, no upload path.
//   - A post is versioned with the code that renders it, so history, diffs and
//     rollback are already solved by git.
//   - The whole premise of this API is that it is read-only and unauthenticated.
//     A CMS would mean adding an authenticated write path to it, which is real
//     attack surface in exchange for a handful of posts a year.
//
// The cost, stated honestly: publishing needs a rebuild and a deploy. For
// announcements that is the right trade. It would not be for a daily journal.
//
// Rendering happens once at startup rather than per request. Posts cannot change
// while the process runs, so anything else would be work repeated for no reason.
package content

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	ghtml "github.com/yuin/goldmark/renderer/html"
)

//go:embed posts/*.md
var files embed.FS

// Post is one rendered article.
type Post struct {
	Slug    string    `json:"slug"`
	Title   string    `json:"title"`
	Date    time.Time `json:"date"`
	Summary string    `json:"summary"`
	// HTML is the rendered body. Raw HTML in the source is dropped rather than
	// passed through: nothing here needs it, and leaving the door open invites a
	// future post to paste something it should not.
	HTML string `json:"html,omitempty"`
	// Draft keeps a post out of the listing while still reachable by its URL, which
	// is how you review one before it is announced.
	Draft bool `json:"draft,omitempty"`
}

var (
	posts  []Post
	bySlug map[string]Post
	// fingerprint identifies this build's post content. Posts change only when the
	// binary does, so it is their cache identity — the data snapshot's timestamp
	// says nothing about whether an article was edited.
	fingerprint string
)

// Fingerprint is a short hash of every post, stable for a given binary.
func Fingerprint() string { return fingerprint }

func init() {
	var err error
	posts, err = parseAll()
	if err != nil {
		// A malformed post is a build-time mistake in a file we wrote, and a site
		// that silently drops an announcement is worse than one that refuses to
		// start with the reason on stderr.
		panic("content: " + err.Error())
	}
	bySlug = make(map[string]Post, len(posts))
	h := sha256.New()
	for _, p := range posts {
		bySlug[p.Slug] = p
		fmt.Fprintf(h, "%s\x00%s\x00%s\x00", p.Slug, p.Title, p.HTML)
	}
	fingerprint = hex.EncodeToString(h.Sum(nil))[:12]
}

// Published returns every non-draft post, newest first.
func Published() []Post {
	out := make([]Post, 0, len(posts))
	for _, p := range posts {
		if !p.Draft {
			out = append(out, p)
		}
	}
	return out
}

// Lookup returns one post by slug, drafts included.
func Lookup(slug string) (Post, bool) {
	p, ok := bySlug[slug]
	return p, ok
}

var md = goldmark.New(
	goldmark.WithExtensions(extension.GFM, extension.Typographer),
	goldmark.WithParserOptions(parser.WithAutoHeadingID()),
	goldmark.WithRendererOptions(ghtml.WithHardWraps()),
)

func parseAll() ([]Post, error) {
	entries, err := fs.Glob(files, "posts/*.md")
	if err != nil {
		return nil, err
	}

	out := make([]Post, 0, len(entries))
	seen := map[string]string{}
	for _, name := range entries {
		raw, err := files.ReadFile(name)
		if err != nil {
			return nil, err
		}
		p, err := parse(name, raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if prev, dup := seen[p.Slug]; dup {
			return nil, fmt.Errorf("%s: slug %q already used by %s", name, p.Slug, prev)
		}
		seen[p.Slug] = name
		out = append(out, p)
	}

	// Newest first. Ties break on slug so the order is stable across builds rather
	// than dependent on directory iteration.
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Date.Equal(out[j].Date) {
			return out[i].Date.After(out[j].Date)
		}
		return out[i].Slug < out[j].Slug
	})
	return out, nil
}

// parse splits frontmatter from body and renders the body.
//
// The frontmatter reader is deliberately a few lines of key: value rather than a
// YAML dependency. It handles exactly what a post needs, and a parser that rejects
// anything it does not understand cannot silently misread a field.
func parse(name string, raw []byte) (Post, error) {
	body := raw
	p := Post{Slug: slugOf(name)}

	if rest, ok := strings.CutPrefix(string(raw), "---\n"); ok {
		front, after, found := strings.Cut(rest, "\n---\n")
		if !found {
			return p, fmt.Errorf("frontmatter is not closed by ---")
		}
		body = []byte(after)
		for i, line := range strings.Split(front, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			key, val, ok := strings.Cut(line, ":")
			if !ok {
				return p, fmt.Errorf("frontmatter line %d: expected key: value", i+1)
			}
			key = strings.TrimSpace(key)
			val = strings.Trim(strings.TrimSpace(val), `"`)
			switch key {
			case "title":
				p.Title = val
			case "summary":
				p.Summary = val
			case "slug":
				p.Slug = val
			case "draft":
				p.Draft = val == "true"
			case "date":
				t, err := time.Parse("2006-01-02", val)
				if err != nil {
					return p, fmt.Errorf("date %q: want YYYY-MM-DD", val)
				}
				p.Date = t.UTC()
			default:
				return p, fmt.Errorf("unknown frontmatter key %q", key)
			}
		}
	}

	if p.Title == "" {
		return p, fmt.Errorf("missing title")
	}
	if p.Date.IsZero() {
		return p, fmt.Errorf("missing date")
	}

	var buf bytes.Buffer
	if err := md.Convert(body, &buf); err != nil {
		return p, err
	}
	p.HTML = buf.String()
	return p, nil
}

// slugOf turns "posts/2026-09-01-hello.md" into "hello".
//
// The date prefix orders the files on disk, where it is useful, and is stripped from
// the URL, where it is noise that would have to change if a post slipped a week.
func slugOf(name string) string {
	base := strings.TrimSuffix(name[strings.LastIndexByte(name, '/')+1:], ".md")
	if len(base) > 11 && base[4] == '-' && base[7] == '-' && base[10] == '-' {
		if _, err := time.Parse("2006-01-02", base[:10]); err == nil {
			return base[11:]
		}
	}
	return base
}
