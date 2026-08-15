package api

import (
	"fmt"
	"html"
	"net/http"
	"strconv"
	"strings"
)

// badge is intentionally outside /v1: it is an image resource rather than a JSON
// API response, suitable for a README, forum signature or team site.
//
// The shape is source, then value with its unit — "folding@home | 31.0M PPD". A badge
// is read where nothing around it supplies context: it said "ppd | 31.0M", which names
// neither what is being measured nor whose it is, and credits nobody for the data. The
// entity's name stays in the title and aria-label, where it has always been, and
// ?name=1 adds a middle segment for somewhere like a signature that has no surrounding
// page to identify it.
func (s *Server) badge(w http.ResponseWriter, r *http.Request) {
	allowed := map[string]bool{"metric": true, "name": true}
	if err := checkQuery(r, allowed, []string{"metric", "name"}); err != nil {
		writeAPIErrorFor(w, r, err)
		return
	}
	snap := s.snap.Load()
	if snap == nil {
		writeErrorFor(w, r, http.StatusServiceUnavailable, "no_data", "no snapshot has been ingested yet")
		return
	}
	kind, err := insightKind(r.PathValue("kind"))
	if err != nil {
		writeAPIErrorFor(w, r, err)
		return
	}

	var entity InsightEntity
	func() {
		if snap.Guard != nil {
			snap.Guard.RLock()
			defer snap.Guard.RUnlock()
		}
		entity, err = snap.insightEntity(kind, r.PathValue("ref"))
	}()
	if err != nil {
		writeAPIErrorFor(w, r, err)
		return
	}

	metric := r.URL.Query().Get("metric")
	if metric == "" {
		metric = "ppd"
	}
	var value, spoken string
	switch metric {
	case "ppd":
		value = compactBadgeInt(entity.PointsPerDay24hAvg) + " PPD"
		spoken = "PPD " + compactBadgeInt(entity.PointsPerDay24hAvg)
	case "points":
		value = compactBadgeInt(entity.PointsTotal) + " points"
		spoken = compactBadgeInt(entity.PointsTotal) + " points"
	case "rank":
		value = "#" + strconv.FormatInt(int64(entity.Rank), 10)
		spoken = "rank " + value
	default:
		writeAPIErrorFor(w, r, badRequest("metric must be ppd, points or rank"))
		return
	}

	segments := []string{badgeSource}
	if r.URL.Query().Get("name") != "" {
		segments = append(segments, badgeText(entity.Name, maxBadgeName))
	}
	segments = append(segments, value)

	etag := `"` + buildID() + "-badge-" + snap.ETag + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Last-Modified", snap.At.UTC().Format(http.TimeFormat))
	w.Header().Set("Cache-Control", cacheControl(snap, r.URL.Path))
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	title := badgeText(entity.Name, maxBadgeName) + " — " + spoken + " on Folding@home"
	writeBody(w, r, http.StatusOK, "image/svg+xml; charset=utf-8", renderBadge(title, segments))
}

const (
	// badgeSource names what is being measured rather than who rendered it. The number
	// is meaningless without it, and the data is Folding@home's.
	badgeSource = "folding@home"
	// maxBadgeName is where a name is cut. "University of Wisconsin-Madison" is 31
	// characters; at full length the badge is a banner rather than a badge.
	maxBadgeName = 20
)

// badgeText makes a participant-chosen name safe to render and bounded in width.
//
// Escaping alone is not enough here. The name is arbitrary text chosen by the public
// and the result is embedded on somebody else's page, so a right-to-left override
// reorders the segments around it and a zero-width run pads the width invisibly.
// Escaping stops it becoming markup; this stops it becoming a layout.
func badgeText(s string, maxRunes int) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteRune(' ')
		case r < 0x20 || r == 0x7f: // other C0 controls
			continue
		case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069: // bidi overrides
			continue
		case r == 0x200b, r == 0x200c, r == 0x200d, r == 0xfeff: // zero width
			continue
		default:
			b.WriteRune(r)
		}
		n++
		if n >= maxRunes {
			return strings.TrimSpace(b.String()) + "…"
		}
	}
	return strings.TrimSpace(b.String())
}

// badgeWidth approximates the rendered width of a string at 11px Verdana.
//
// Rune-aware rather than byte-aware, which the previous version was: it sized from
// len(s), so a name of five Japanese characters measured as fifteen and produced a
// badge three times wider than its text. Latin glyphs run about seven pixels and the
// East Asian wide ranges about eleven; the estimate only has to be close enough that
// the text is not clipped or adrift in its segment.
func badgeWidth(s string) int {
	w := 0
	for _, r := range s {
		switch {
		case r >= 0x1100 && r <= 0x115f, // Hangul Jamo
			r >= 0x2e80 && r <= 0xa4cf, // CJK radicals through Yi
			r >= 0xac00 && r <= 0xd7a3, // Hangul syllables
			r >= 0xf900 && r <= 0xfaff, // CJK compatibility
			r >= 0xfe30 && r <= 0xfe6f, // CJK compatibility forms
			r >= 0xff00 && r <= 0xff60, // fullwidth forms
			r >= 0xffe0 && r <= 0xffe6,
			r >= 0x20000 && r <= 0x3fffd: // CJK extensions
			w += 11
		default:
			w += 7
		}
	}
	return w
}

// renderBadge draws the segments left to right, the last one accented.
func renderBadge(title string, segments []string) []byte {
	const pad = 9
	widths := make([]int, len(segments))
	total := 0
	for i, seg := range segments {
		widths[i] = badgeWidth(seg) + pad*2
		total += widths[i]
	}

	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" role="img" aria-label="%s" width="%d" height="20" viewBox="0 0 %d 20">`,
		html.EscapeString(title), total, total)
	fmt.Fprintf(&b, `<title>%s</title>`, html.EscapeString(title))
	b.WriteString(`<linearGradient id="s" x2="0" y2="100%"><stop stop-color="#fff" stop-opacity=".12"/><stop offset="1" stop-opacity=".12"/></linearGradient>`)
	fmt.Fprintf(&b, `<clipPath id="r"><rect width="%d" height="20" rx="3"/></clipPath><g clip-path="url(#r)">`, total)

	// Greys for the context, the accent for the figure — so the number is what the eye
	// lands on when a row of these sits in a README.
	fills := []string{"#30363d", "#444c56"}
	x := 0
	for i, wSeg := range widths {
		fill := "#2878d6"
		if i < len(widths)-1 {
			fill = fills[min(i, len(fills)-1)]
		}
		fmt.Fprintf(&b, `<rect x="%d" width="%d" height="20" fill="%s"/>`, x, wSeg, fill)
		x += wSeg
	}
	fmt.Fprintf(&b, `<rect width="%d" height="20" fill="url(#s)"/></g>`, total)

	b.WriteString(`<g fill="#fff" text-anchor="middle" font-family="Verdana,DejaVu Sans,sans-serif" font-size="11">`)
	x = 0
	for i, seg := range segments {
		weight := ""
		if i == len(segments)-1 {
			weight = ` font-weight="bold"`
		}
		fmt.Fprintf(&b, `<text x="%d" y="14"%s>%s</text>`, x+widths[i]/2, weight, html.EscapeString(seg))
		x += widths[i]
	}
	b.WriteString(`</g></svg>`)
	return []byte(b.String())
}

func compactBadgeInt(v int64) string {
	abs := v
	if abs < 0 {
		abs = -abs
	}
	for _, unit := range []struct {
		n int64
		s string
	}{{1_000_000_000_000_000, "Q"}, {1_000_000_000_000, "T"}, {1_000_000_000, "B"}, {1_000_000, "M"}, {1_000, "k"}} {
		if abs >= unit.n {
			return strconv.FormatFloat(float64(v)/float64(unit.n), 'f', 1, 64) + unit.s
		}
	}
	return strconv.FormatInt(v, 10)
}
