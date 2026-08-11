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
	// ServerTime is this server's clock at the moment the response was built —
	// which is not necessarily now.
	//
	// Comparing timestamps against this rather than the client's own clock makes any
	// derived figure depend on elapsed time instead of on two machines agreeing.
	// Unsynced clients are routinely minutes out.
	//
	// But every route except /v1/status is cacheable, and this rides inside the
	// cached body, so a stored response replays the clock reading it was built with.
	// A caller taking it as "now" reports whatever freshness its own cached copy
	// happened to capture. HTTP already carries the correction: Age says how long the
	// response has been held, so now is ServerTime plus Age. A caller that would
	// rather not do the arithmetic can read /v1/status, which is no-store and always
	// freshly built.
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
	// is less than the full seven days.
	//
	// points_per_day_7d_avg is not understated during this period — it divides by the
	// span actually observed — but it is averaged over less, so a single busy day
	// moves it much further than it will once a full week is in view.
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

// Rivals is the neighbourhood around one entity in the rankings, with the moment
// each order would swap if both sides held their current rate.
type Rivals struct {
	Rank int32  `json:"rank"`
	Name string `json:"name"`

	// TeamID and TeamName are present only when the field is one team's roster rather
	// than the whole site. They are what tells a reader which competition the ranks
	// below belong to: "7th" means something entirely different among 340 teammates
	// than among two million donors, and the numbers alone cannot say which.
	TeamID   *int32 `json:"team_id,omitempty"`
	TeamName string `json:"team_name,omitempty"`

	// HorizonDays is how far ahead a projection is reported at all. Past it,
	// overtake_days is null rather than a number: a daily rate extrapolated over
	// decades is arithmetic, not information, and printing "412 years" would dress a
	// rounding artefact up as a finding.
	HorizonDays int `json:"horizon_days"`

	Rivals []Rival `json:"rivals"`
}

// Rival is one entity near the subject, ordered best-first alongside it.
//
// Every figure here except the projection is measured. The projection is not: it
// assumes both parties keep producing at exactly their current rolling-day rate
// forever, which nobody does — and over a day rather than a week that assumption is
// livelier and correspondingly more fragile. It is reported because "how far away is that" is the
// question people actually have, and a gap in points alone does not answer it.
type Rival struct {
	Rank int32  `json:"rank"`
	Name string `json:"name"`
	// TeamID is present on team rivals and absent on donors.
	TeamID *int32 `json:"team_id,omitempty"`
	// Self marks the subject's own row, which is included so the list reads as a
	// neighbourhood rather than as two lists the client has to splice.
	Self bool `json:"self,omitempty"`

	PointsTotal int64 `json:"points_total"`
	// PointsPerDay24hAvg is the rate the projection below was built on. Both are
	// reported because they routinely disagree, and the gap between them is the
	// warning: a rival whose day is far above their week is having a good day, not
	// necessarily arriving when overtake_at says.
	PointsPerDay24hAvg int64 `json:"points_per_day_24h_avg"`
	PointsPerDay7dAvg  int64 `json:"points_per_day_7d_avg"`

	// PointsGap is the lifetime difference to the subject, always positive. Zero on
	// the subject's own row, and for anyone exactly tied with it.
	PointsGap int64 `json:"points_gap"`

	// OvertakeDays is when the two would swap places, null when they never would at
	// current rates — the one behind is not gaining, or is not gaining fast enough to
	// arrive inside the horizon. Null is the common case and is not a failure.
	OvertakeDays *float64   `json:"overtake_days"`
	OvertakeAt   *time.Time `json:"overtake_at"`
}

// Standing is where an entity sits within one ordering, as a share rather than as a
// position.
//
// Rank #48,213 tells a person almost nothing; "top 2.3%" tells them where they are.
// Both are reported, because the two answer different questions and a rank alone has
// no scale attached to it.
type Standing struct {
	// TopPercent is the share of the field at or above this entity, so smaller is
	// better and the leader is as close to zero as the field size allows. Named for
	// what it is rather than "percentile", which conventionally counts the other way
	// and would invert every reading of it.
	TopPercent float64 `json:"top_percent"`
	// Of is the size of the field this share is taken against, which differs by basis
	// and is the whole reason the number means anything. Lifetime counts everyone
	// tracked; this_month counts only those who produced this month, because a share
	// of a field that mostly did nothing flatters everyone who did anything.
	Of int `json:"of"`
}

// Standings is one entity's position on each basis, present on detail responses only.
//
// Collections omit it deliberately: a percentile within a page of the top fifty is
// the same number fifty times over, and computing it per row would put a search over
// two million donors on the listing path to say so.
type Standings struct {
	Lifetime *Standing `json:"lifetime,omitempty"`
	// ThisMonth is absent for an entity that produced nothing this month. That is not
	// last place — it is not being in the field at all, and reporting "bottom" for it
	// would be a measurement of something that did not happen.
	ThisMonth *Standing `json:"this_month,omitempty"`
}

// Recent is production over a trailing window of whole UTC days.
//
// It exists for one field. The lifetime points_per_wu turned out to be dominated by
// how long an entity has been folding rather than by what it folds with — the same
// ratio over the last 30 days runs 3x to 27x higher for everybody, because the points
// a work unit earns have inflated enormously since the project began. This window is
// the version that can be compared between entities, since they are all facing the
// same work units now.
// PerDayWindow is one averaging window over the same underlying rate.
//
// The list exists so a reader can switch between them without the service having to
// guess which one they wanted, and so adding a window later is a new element rather
// than a new field every consumer has to learn about.
//
// CoversSec is the load-bearing part. A window is a request, not a promise: this
// service began collecting on 2 August 2026, so for its first month a thirty-day
// average is a ten-day average wearing a thirty-day label, and that is precisely the
// misnaming this project refuses to repeat — see the note on PointsPerDay7dAvg about
// EOC's "24hr Avg". Read CoversSec, not the name, when the number has to be trusted.
type PerDayWindow struct {
	// Window is the nominal window: "24h", "7d" or "30d".
	Window string `json:"window"`
	// PointsPerDay over the period actually covered, not over the nominal window —
	// dividing by days an entity did not exist for reports a fraction of a real rate.
	PointsPerDay int64 `json:"points_per_day"`
	// CoversSec is what was really averaged over.
	CoversSec int64 `json:"covers_sec"`
	// Partial marks a window the record is not yet long enough to fill.
	Partial bool `json:"partial,omitempty"`
}

type Recent struct {
	// Days is the window covered: thirty, or the whole record while it is shorter.
	// Reported rather than assumed, because a ratio over four days and a ratio over
	// thirty deserve different amounts of trust.
	Days int `json:"days"`

	Points int64 `json:"points"`
	WUs    int64 `json:"wus"`
	// PointsPerWU over this window. Absent — the whole block is — when nothing was
	// produced in it, which is the honest answer rather than falling back to lifetime
	// and quietly changing what the field means.
	PointsPerWU int64 `json:"points_per_wu"`
}

// Streak is consecutive days with production.
//
// A day counts if anything at all was produced on it, because the question is whether
// somebody kept going, not how hard. For a donor that means any of their teams: folding
// for two teams on one day is one day of folding.
type Streak struct {
	// Current is the run ending today, or ending yesterday while today is still open.
	// A day that has not finished cannot have been missed, so somebody who folded
	// yesterday and has not yet folded in the hours since midnight has broken nothing.
	Current int `json:"current"`
	// Longest is the best run anywhere in the retained record.
	Longest int `json:"longest"`
	// ActiveDays is how many days had production at all, which is the denominator
	// consistency is measured against — 30 days of a 30-day record is a very different
	// story from 30 days of a 400-day one.
	ActiveDays int `json:"active_days"`
	// Since is the first day of the current run, absent when there is no current run.
	Since *time.Time `json:"since,omitempty"`
	// AtCollectionFloor marks a current run that reaches back to the first day this
	// service recorded anything. Then the figure is a lower bound and not a fact about
	// the entity: somebody who has folded daily for a decade reports the age of this
	// site. Saying so is the difference between a statistic and a wrong one.
	AtCollectionFloor bool `json:"at_collection_floor,omitempty"`
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

	// PointsPerDay24hAvg is production in the rolling day as a daily rate.
	//
	// The same question as the seven-day figure over a window a seventh as long, and
	// the livelier of the two: a machine switched on this morning appears here today
	// and takes most of a week to move its neighbour. The cost is the other side of
	// that — one good night reads as a permanent rate. Both are published so they can
	// be read against each other, and every projection on this service is built on
	// this one.
	PointsPerDay24hAvg int64 `json:"points_per_day_24h_avg"`

	// PointsPerDay7dAvg is production over the last seven days as a daily rate,
	// rounded to nearest. This is the figure EOC labels "24hr Avg".
	//
	// The divisor is seven days once seven days of this entity have been watched, and
	// the period actually observed before that — so a team first seen yesterday
	// reports the rate it is really producing at, rather than a seventh of it
	// climbing to the truth over its first week. Days it existed for and produced
	// nothing on still divide; only days before it existed are excluded.
	PointsPerDay7dAvg int64 `json:"points_per_day_7d_avg"`

	// PointsThisMonthUTC is production since 00:00 UTC on the 1st. Unlike the other
	// figures here it is read from the monthly rollup rather than from the rolling
	// windows, which only span seven days.
	PointsThisMonthUTC int64 `json:"points_this_month_utc"`

	// PointsPerWU is lifetime points divided by lifetime work units, rounded.
	//
	// Do not compare this between entities. Measured across the top teams it runs 3x to
	// 27x below the same ratio over the last 30 days — for every one of them, not for a
	// few that changed hardware. Points per work unit have inflated enormously over the
	// project's twenty years, so this figure tracks how long an entity has been folding
	// far more than what it folds with.
	//
	// Recent.PointsPerWU on detail responses is the comparable one: every entity is
	// facing the same work units now, so differences there are differences in hardware
	// and project mix rather than in tenure.
	//
	// Zero when no work units are recorded, which is the honest answer to a division
	// with nothing to divide by.
	PointsPerWU int64 `json:"points_per_wu"`
}

// perWU is lifetime points per work unit, rounded to nearest, and zero when there is
// nothing to divide by.
func perWU(points, wus int64) int64 {
	if wus <= 0 {
		return 0
	}
	return (points + wus/2) / wus
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

	// Standing, Streak, Recent and PerDay are present on the detail endpoint only.
	//
	// PerDay is absent from listings on purpose: the thirty-day window is read from
	// the daily rollup, which is one query per entity, and a page of fifty rows would
	// turn a single response into fifty round trips for a figure nobody sorted by.
	Standing *Standings     `json:"standing,omitempty"`
	Streak   *Streak        `json:"streak,omitempty"`
	Recent   *Recent        `json:"recent,omitempty"`
	PerDay   []PerDayWindow `json:"points_per_day,omitempty"`

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

	// Standing, Streak, Recent and PerDay are present on the detail endpoint only.
	//
	// PerDay is absent from listings on purpose: the thirty-day window is read from
	// the daily rollup, which is one query per entity, and a page of fifty rows would
	// turn a single response into fifty round trips for a figure nobody sorted by.
	Standing *Standings     `json:"standing,omitempty"`
	Streak   *Streak        `json:"streak,omitempty"`
	Recent   *Recent        `json:"recent,omitempty"`
	PerDay   []PerDayWindow `json:"points_per_day,omitempty"`

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

	// NewDonors24h, NewTeams24h and NewMembers24h count arrivals over the last 24
	// hours — the same baseline rank_change_24h is measured against.
	//
	// New means first seen in the feed, which is not quite the same as newly created:
	// a donor who renames themselves arrives here as a new name and departs as an old
	// one. Memberships exceed donors because an existing donor joining another team
	// creates a member row without creating a donor.
	//
	// Pointers, because absent and zero differ: absent means less than a day has been
	// observed and nobody can be called new yet.
	NewDonors24h  *int `json:"new_donors_24h,omitempty"`
	NewTeams24h   *int `json:"new_teams_24h,omitempty"`
	NewMembers24h *int `json:"new_members_24h,omitempty"`

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
