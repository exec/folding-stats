package api

import "time"

// Envelope wraps every response.
//
// The shape is identical for every endpoint — snapshot metadata, then the payload,
// then pagination if the payload is a collection. Consumers (and models reading the
// API) can rely on one structure rather than learning a shape per route, and there is
// somewhere to add fields later without breaking anyone.
type Envelope struct {
	Snapshot SnapshotInfo `json:"snapshot"`
	Data     any          `json:"data"`
	Page     *PageInfo    `json:"page,omitempty"`
}

// SnapshotInfo describes the data's freshness. Carried on every response, not exposed
// as a separate endpoint, because it describes *this response's* data: a client that
// fetched data and then fetched freshness separately could straddle a publish and
// believe its data an hour newer than it is. That is a correctness property a side
// request cannot reconstruct.
//
// It is deliberately small, because it rides on every response and most responses are
// under a kilobyte. Anything derivable from another field is omitted rather than
// duplicated, and the warm-up qualifiers disappear once they would be permanently
// constant.
type SnapshotInfo struct {
	// At is the upstream publish time this data reflects — not when we fetched it.
	At time.Time `json:"at"`
	// NextExpectedAt is when the next upstream publish is due, from the measured
	// cadence rather than an assumed hour. Subtract At for the interval.
	NextExpectedAt time.Time `json:"next_expected_at"`
	// Stale means the expected update did not arrive and this data is older than it
	// should be. Not derivable from the timestamps: it allows a grace period for
	// routine upstream drift that only the server knows about.
	Stale bool `json:"stale"`
	// ServerTime is this server's clock at the moment the response was built.
	//
	// Comparing timestamps against this rather than the client's own clock makes any
	// derived figure depend on elapsed time instead of on two machines agreeing.
	// Unsynced clients are routinely minutes out.
	ServerTime time.Time `json:"server_time"`
	// WarmingUp is present only while some figure is not yet at full fidelity, and
	// absent otherwise.
	//
	// Its presence is the signal, which is why it is an object rather than a pair of
	// booleans: a client testing `if (!snapshot.avg_window_complete)` on an omitted
	// boolean would read "absent" as "incomplete" and get the opposite of the truth.
	// There is no such trap in testing whether an object exists.
	WarmingUp *WarmingUp `json:"warming_up,omitempty"`
}

// WarmingUp qualifies figures that are still converging after a cold start.
type WarmingUp struct {
	// HistorySpanSec is how much history the rate windows actually cover, while that
	// is less than the full seven days. During this period points_per_day_7d_avg is
	// divided by 7 over a shorter window and therefore reads low.
	HistorySpanSec int64 `json:"history_span_sec,omitempty"`
	// IntervalEstimated is true while NextExpectedAt comes from the nominal hour
	// rather than from observed publishes, before enough cycles exist to measure the
	// real cadence.
	IntervalEstimated bool `json:"interval_estimated,omitempty"`
	// RankChange24hUnavailable is true while no rank_change_24h can be computed for
	// anyone, because less than 24 hours of cycles have been observed. It
	// distinguishes "nobody has moved" from "we cannot say yet" for a client that
	// finds the field missing on every entity at once.
	RankChange24hUnavailable bool `json:"rank_change_24h_unavailable,omitempty"`
}

// PageInfo describes a paginated collection.
type PageInfo struct {
	Page       int `json:"page"`
	PerPage    int `json:"per_page"`
	TotalItems int `json:"total_items"`
	TotalPages int `json:"total_pages"`
}

// Production is the set of rate figures shared by every entity at every scope.
//
// Field names deliberately do not follow EOC's. Their "24hr Avg" is a 7-day moving
// average — their own FAQ says so — and inheriting that name would mislead every
// consumer of this API. Likewise the calendar buckets are explicitly UTC, because
// EOC's are US Central and that is invisible in their field names.
type Production struct {
	PointsTotal int64 `json:"points_total"`
	WUsTotal    int64 `json:"wus_total"`

	// PointsLastUpdate is production in the most recent hourly cycle.
	PointsLastUpdate int64 `json:"points_last_update"`

	// Rolling windows, measured back from the snapshot time.
	PointsLast24h int64 `json:"points_last_24h"`
	PointsLast7d  int64 `json:"points_last_7d"`

	// Calendar buckets, reset at 00:00 UTC and 00:00 UTC Monday. These differ from
	// the rolling windows: just after midnight points_today_utc is small while
	// points_last_24h is unchanged.
	PointsTodayUTC    int64 `json:"points_today_utc"`
	PointsThisWeekUTC int64 `json:"points_this_week_utc"`

	// PointsPerDay7dAvg is points_last_7d divided by 7, rounded to nearest. This is
	// the figure EOC labels "24hr Avg".
	PointsPerDay7dAvg int64 `json:"points_per_day_7d_avg"`

	// PointsThisMonthUTC is production since 00:00 UTC on the 1st. Unlike the other
	// figures here it is read from the monthly rollup rather than from the rolling
	// windows, which only span seven days.
	PointsThisMonthUTC int64 `json:"points_this_month_utc"`
}

// Team is one team at collection or detail scope.
type Team struct {
	TeamID int32  `json:"team_id"`
	Name   string `json:"name"`
	Rank   int32  `json:"rank"`

	// RankChange24h is places gained since 24 hours ago, negative for places lost.
	//
	// A pointer because absent and zero are different answers: zero means the rank
	// is genuinely unchanged, while absent means there is nothing to compare against
	// — the entity is newer than a day, or the service has not yet observed one.
	// Rendering "no change" for an entity we simply were not watching would be a
	// measurement we never made.
	RankChange24h *int32 `json:"rank_change_24h,omitempty"`

	MembersTotal  int32 `json:"members_total"`
	MembersActive int32 `json:"members_active"`

	Production
}

// Member is one (name, team) pair — the grain of the upstream feed.
//
// A member is not a person: the same donor name appears on many teams. Use Donor for
// the person-level view.
type Member struct {
	Name       string `json:"name"`
	TeamID     int32  `json:"team_id"`
	TeamName   string `json:"team_name,omitempty"`
	RankGlobal int32  `json:"rank_global"`
	RankInTeam int32  `json:"rank_in_team"`

	// RankChange24h tracks rank_global. In-team movement is not reported: it is
	// derived by walking the global order, so it would need a second historical
	// pass per team to answer a question nobody has asked for.
	RankChange24h *int32 `json:"rank_change_24h,omitempty"`

	Production
}

// Donor is a name aggregated across every team it folds for.
//
// This has no equivalent on EOC, which treats the same name on two teams as two
// unrelated users. Totals here are the sum across Teams.
type Donor struct {
	Name string `json:"name"`
	Rank int32  `json:"rank"`

	// RankChange24h is places gained since 24 hours ago; see Team.RankChange24h for
	// why it is a pointer.
	RankChange24h *int32 `json:"rank_change_24h,omitempty"`

	TeamCount int32 `json:"team_count"`

	// LikelyNotAPerson marks names shared by implausibly many teams — console
	// defaults and placeholders such as "PS3" (10,426 teams) or "Anonymous". The
	// points are real; the single identity is not. Such entries are flagged rather
	// than hidden so totals still reconcile.
	LikelyNotAPerson bool `json:"likely_not_a_person"`

	Production

	// Teams is the per-team breakdown, present on the detail endpoint and ordered
	// by points. It ships with the donor so assembling a donor page never costs one
	// request per team.
	//
	// It is capped: a shared placeholder name such as "PS3" spans 10,426 teams, and
	// embedding all of them would make the response megabytes. When TeamsTruncated
	// is set, the full list is available from /v1/donors/{name}/teams.
	Teams          []Member `json:"teams,omitempty"`
	TeamsTruncated bool     `json:"teams_truncated,omitempty"`
}

// Summary is the whole project as one entity — the top of the same hierarchy that
// Team and Donor sit in.
type Summary struct {
	TeamsTotal   int `json:"teams_total"`
	TeamsActive  int `json:"teams_active"`
	DonorsTotal  int `json:"donors_total"`
	DonorsActive int `json:"donors_active"`
	// MembersTotal counts (name, team) pairs, which exceeds donors_total because
	// donors on several teams contribute one member row per team.
	MembersTotal int `json:"members_total"`

	Production
}

// HistoryPoint is one bucket of a time series.
type HistoryPoint struct {
	At     time.Time `json:"at"`
	Points int64     `json:"points"`
	WUs    int64     `json:"wus"`
}

// History is a time series for one entity.
type History struct {
	Metric      string         `json:"metric"`
	Granularity string         `json:"granularity"`
	From        time.Time      `json:"from"`
	To          time.Time      `json:"to"`
	Points      []HistoryPoint `json:"points"`

	// For a donor series merged across teams: how many contributed, and whether
	// that was capped. Only shared placeholder names reach the cap, and the teams
	// included are the highest-scoring ones.
	TeamsIncluded  int  `json:"teams_included,omitempty"`
	TeamsTruncated bool `json:"teams_truncated,omitempty"`
}

// SearchResults holds lookups, best match first.
//
// An exact case-sensitive hit always leads and is flagged, followed by
// case-insensitive prefix matches in rank order. There is no minimum query length:
// a three-character floor makes short real names such as "DH" unfindable, which is
// exactly the gap this endpoint exists to close.
type SearchResults struct {
	Query  string  `json:"query"`
	Teams  []Team  `json:"teams"`
	Donors []Donor `json:"donors"`

	// ExactDonor and ExactTeam report whether the first entry of each list is an
	// exact case-sensitive match rather than a prefix one — the difference between
	// "this is your donor" and "these look similar".
	ExactDonor bool `json:"exact_donor"`
	ExactTeam  bool `json:"exact_team"`
}

// APIError is the error body. One shape for every failure.
type APIError struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// PostSummary is a blog post without its body, for listings.
type PostSummary struct {
	Slug    string    `json:"slug"`
	Title   string    `json:"title"`
	Date    time.Time `json:"date"`
	Summary string    `json:"summary"`
}
