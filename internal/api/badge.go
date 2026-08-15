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
func (s *Server) badge(w http.ResponseWriter, r *http.Request) {
	if err := checkQuery(r, map[string]bool{"metric": true}, []string{"metric"}); err != nil {
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
	var label, value string
	switch metric {
	case "ppd":
		label, value = "PPD", compactBadgeInt(entity.PointsPerDay24hAvg)
	case "points":
		label, value = "points", compactBadgeInt(entity.PointsTotal)
	case "rank":
		label, value = "rank", "#"+strconv.FormatInt(int64(entity.Rank), 10)
	default:
		writeAPIErrorFor(w, r, badRequest("metric must be ppd, points or rank"))
		return
	}

	etag := `"` + buildID() + "-badge-" + snap.ETag + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Last-Modified", snap.At.UTC().Format(http.TimeFormat))
	w.Header().Set("Cache-Control", cacheControl(snap, r.URL.Path))
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	title := entity.Name + " — " + label + " " + value
	left, right := 54, max(56, len(value)*8+18)
	width := left + right
	svg := fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" role="img" aria-label="%s" width="%d" height="20" viewBox="0 0 %d 20">`+
		`<title>%s</title><linearGradient id="s" x2="0" y2="100%%"><stop stop-color="#fff" stop-opacity=".12"/><stop offset="1" stop-opacity=".12"/></linearGradient>`+
		`<clipPath id="r"><rect width="%d" height="20" rx="3"/></clipPath><g clip-path="url(#r)">`+
		`<rect width="%d" height="20" fill="#30363d"/><rect x="%d" width="%d" height="20" fill="#2878d6"/><rect width="%d" height="20" fill="url(#s)"/></g>`+
		`<g fill="#fff" text-anchor="middle" font-family="Verdana,DejaVu Sans,sans-serif" font-size="11"><text x="%d" y="14">%s</text><text x="%d" y="14" font-weight="bold">%s</text></g></svg>`,
		html.EscapeString(title), width, width, html.EscapeString(title), width,
		left, left, right, width, left/2, html.EscapeString(strings.ToLower(label)),
		left+right/2, html.EscapeString(value))
	writeBody(w, r, http.StatusOK, "image/svg+xml; charset=utf-8", []byte(svg))
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
