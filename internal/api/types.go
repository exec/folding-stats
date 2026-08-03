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

// SnapshotInfo describes the data's freshness. Carried on every response so a client
// can cache correctly without a second request.
type SnapshotInfo struct {
	// At is the upstream publish time this data reflects — not when we fetched it.
	At time.Time `json:"at"`
	// NextExpectedAt is when the next upstream publish is due. Upstream publishes
	// hourly, so polling faster than this only costs both sides bandwidth.
	NextExpectedAt time.Time `json:"next_expected_at"`
	// Stale means the expected update did not arrive and this data is older than
	// it should be. Upstream outages are routine; the flag makes them visible.
	Stale bool `json:"stale"`
	// AvgWindowComplete is false while less than 7 days of history has been
	// collected, during which points_per_day_7d_avg is averaged over a short window
	// and reads low.
	AvgWindowComplete bool `json:"avg_window_complete"`
	// ServerTime is this server's clock at the moment the response was built.
	//
	// A countdown computed as next_expected_at minus the browser's own clock is wrong
	// by however far that clock is off, and unsynced clocks are minutes out routinely.
	// Comparing both endpoints against this one instead makes the countdown depend on
	// elapsed time rather than on absolute agreement.
	ServerTime time.Time `json:"server_time"`
	// IntervalSec is the measured upstream publish cadence in seconds. Nominally an
	// hour; actually 3606-3613s and drifting later each cycle.
	IntervalSec int64 `json:"interval_sec"`
	// IntervalMeasured is false while IntervalSec is still the nominal fallback,
	// before enough cycles have been observed to measure it.
	IntervalMeasured bool `json:"interval_measured"`
	// HistorySpanSec is how much history the rate windows actually cover, capped at
	// the 7-day window. A caller that only reads AvgWindowComplete knows the average
	// is short but not by how much; this says. Zero on the very first snapshot, when
	// no interval has been observed yet.
	HistorySpanSec int64 `json:"history_span_sec"`
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
}

// Team is one team at collection or detail scope.
type Team struct {
	TeamID int32  `json:"team_id"`
	Name   string `json:"name"`
	Rank   int32  `json:"rank"`

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

	Production
}

// Donor is a name aggregated across every team it folds for.
//
// This has no equivalent on EOC, which treats the same name on two teams as two
// unrelated users. Totals here are the sum across Teams.
type Donor struct {
	Name string `json:"name"`
	Rank int32  `json:"rank"`

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
