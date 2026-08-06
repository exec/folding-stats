package api

// What has moved since a given moment.
//
// Every other collection here answers "what is the state of everything", and a client
// that wants to stay current with all of it has no choice but to ask that repeatedly:
// 129,958 teams or 2.1M donors an hour, almost all of it identical to the hour before.
// That is the traffic this endpoint exists to not receive.
//
// About 1,100 members produce in any given cycle, out of 2.7M — so the changed set is
// roughly a twentieth of one percent of the corpus. Someone mirroring the whole thing
// hourly can do it with that instead of the crawl, which is better for them and
// dramatically cheaper here. It is the same data through the same view builders; only
// the selection differs.
//
// The cursor is the snapshot time a client already holds. Every response carries
// snapshot.at, so "give me what changed since the data I have" needs no separate state
// on either side, and no cursor to expire.

import (
	"math"
	"net/http"
	"sort"
	"strconv"
	"time"
)

// maxChangesWindow bounds how far back a client may ask.
//
// Raw deltas are retained for ninety days, so a longer window is answerable — but past
// about a week the changed set stops being a saving. A caller that far behind is better
// served by the full collections, and telling them so beats quietly serving them a
// multi-megabyte diff that costs more than the thing it replaces.
const maxChangesWindow = 7 * 24 * time.Hour

// parseSince reads the ?since= cursor, accepting either RFC 3339 or unix seconds.
//
// Both, because the two obvious things to paste are the snapshot.at from a previous
// response and whatever a client's own clock produces, and rejecting either one is a
// trap that costs somebody an afternoon.
func parseSince(r *http.Request, now time.Time) (time.Time, error) {
	v := r.URL.Query().Get("since")
	if v == "" {
		return time.Time{}, badRequest(
			"since is required: pass the snapshot.at of the data you already hold, " +
				"as RFC 3339 or unix seconds")
	}
	var at time.Time
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
		at = time.Unix(secs, 0).UTC()
	} else if at, err = time.Parse(time.RFC3339, v); err != nil {
		return time.Time{}, badRequest("since must be RFC 3339 or unix seconds, not %q", v)
	}
	if oldest := now.Add(-maxChangesWindow); at.Before(oldest) {
		return time.Time{}, badRequest(
			"since is further back than %s; a caller that far behind should read the full "+
				"collections at /v1/teams and /v1/donors rather than a diff, which by then "+
				"costs more than the crawl it replaces. The oldest accepted value is %s",
			maxChangesWindow, oldest.Format(time.RFC3339))
	}
	return at, nil
}

// changes lists the entities that produced since a moment, in rank order.
//
// Rank order rather than id order so paging is stable and the first page is the part
// most callers care about — but the guarantee that matters is that the set is complete
// as of snapshot.at, which the envelope already reports.
func (s *Server) changes(snap *Snapshot, r *http.Request) (any, *PageInfo, error) {
	if snap.Store == nil {
		return nil, nil, badRequest("this instance has no history store, so it cannot say what changed")
	}
	since, err := parseSince(r, snap.At)
	if err != nil {
		return nil, nil, err
	}
	kind := r.URL.Query().Get("kind")
	if kind == "" {
		kind = "teams"
	}
	ctx := r.Context()

	switch kind {
	case "teams":
		slots, err := snap.Store.ChangedTeams(ctx, since)
		if err != nil {
			return nil, nil, err
		}
		order := rankOrder(slots, snap.Ranks.TeamRankOf)
		lo, hi, page, err := paginate(r, len(order))
		if err != nil {
			return nil, nil, err
		}
		out := make([]Team, 0, hi-lo)
		for _, slot := range order[lo:hi] {
			out = append(out, snap.teamView(slot))
		}
		return out, page, nil

	case "members":
		slots, err := snap.Store.ChangedMembers(ctx, since)
		if err != nil {
			return nil, nil, err
		}
		order := rankOrder(slots, snap.Ranks.MemberRankOf)
		lo, hi, page, err := paginate(r, len(order))
		if err != nil {
			return nil, nil, err
		}
		out := make([]Member, 0, hi-lo)
		for _, slot := range order[lo:hi] {
			out = append(out, snap.memberView(slot, true))
		}
		return out, page, nil

	case "donors":
		// A donor changed when any of its memberships did, so the member set collapses
		// onto fewer donors — which is the point of asking at this grain rather than
		// reconstructing the aggregate from member rows on the client.
		slots, err := snap.Store.ChangedMembers(ctx, since)
		if err != nil {
			return nil, nil, err
		}
		seen := make(map[int32]bool, len(slots))
		idxs := make([]int32, 0, len(slots))
		for _, slot := range slots {
			if int(slot) >= len(snap.State.Members) {
				continue
			}
			d := snap.Ranks.DonorIndexOf(snap.State.Members[slot].NameID)
			if d < 0 || seen[d] {
				continue
			}
			seen[d] = true
			idxs = append(idxs, d)
		}
		// Donors are stored in rank order, so the index is the rank.
		order := rankOrder(idxs, func(i int32) int32 { return i + 1 })
		lo, hi, page, err := paginate(r, len(order))
		if err != nil {
			return nil, nil, err
		}
		out := make([]Donor, 0, hi-lo)
		for _, idx := range order[lo:hi] {
			out = append(out, snap.donorView(idx, false))
		}
		return out, page, nil
	}
	return nil, nil, badRequest(`kind must be "teams", "members" or "donors"`)
}

// rankOrder sorts ids best-first by the rank each one holds.
//
// The changed set arrives in whatever order the delta index yields, which is stable but
// arbitrary. Sorting it means page 2 of a request is the same rows however the storage
// engine felt about the range scan, and that the first page is the part a reader
// actually wants.
func rankOrder(ids []int32, rankOf func(int32) int32) []int32 {
	type ranked struct{ id, rank int32 }
	rs := make([]ranked, len(ids))
	for i, id := range ids {
		r := rankOf(id)
		// Unranked ids sort last rather than first: zero would put anything the table
		// does not know about at the top of every page. The sentinel has to be past
		// every real rank, not past the length of this set — ranks run to the size of
		// the corpus, and a changed set is a small fraction of it.
		if r <= 0 {
			r = math.MaxInt32
		}
		rs[i] = ranked{id, r}
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].rank < rs[j].rank })
	out := make([]int32, len(rs))
	for i, r := range rs {
		out[i] = r.id
	}
	return out
}
