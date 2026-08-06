package bot

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Alerts are the one thing here that speaks first.
//
// Every other command answers a question somebody just asked, which makes being wrong
// cheap: they are looking at the reply and can run it again. An alert arrives in a
// channel at three in the morning, possibly pinging a role, with nobody having asked
// for it — so the bar is not "is this figure right" but "is this worth interrupting
// people for". That shapes the whole design: every type below fires on a transition
// rather than on a state, each one re-arms only after the condition clears, and an
// alert created now is seeded with the current reading so it cannot immediately
// announce something that happened last week.

type AlertType string

const (
	// AlertMilestone fires when lifetime points cross a round number.
	AlertMilestone AlertType = "milestone"
	// AlertRank fires when the target reaches a rank at least as good as the threshold.
	AlertRank AlertType = "rank"
	// AlertIdle fires when a producer stops producing — the one people actually want,
	// because a rig that quietly died is otherwise invisible until someone checks.
	AlertIdle AlertType = "idle"
	// AlertResumed is its counterpart, so a fixed machine closes the loop.
	AlertResumed AlertType = "resumed"
	// AlertDaily is a scheduled summary rather than an event.
	AlertDaily AlertType = "daily"
)

// alertKind describes one type for the command surface and the rules.
type alertKind struct {
	Type      AlertType
	Label     string // shown in the /alert add picker
	Blurb     string // shown in /alert list
	Threshold string // what the threshold option means here, empty if unused
	Default   int64
}

// AlertKinds is the single source of truth for the types: the command choices, the
// listing, the help text and the rules all read this rather than restating it.
var AlertKinds = []alertKind{
	{AlertMilestone, "Milestone — crosses a round points total", "every 1M/2M/5M step", "", 0},
	{AlertRank, "Rank — reaches a place on the leaderboard", "reaching rank", "the rank to reach", 1000},
	{AlertIdle, "Went quiet — stopped producing", "no points for", "hours of silence before alerting", 24},
	{AlertResumed, "Started again — producing after a gap", "production resumes", "", 0},
	{AlertDaily, "Daily summary", "once a day at", "hour of day, UTC (0-23)", 12},
}

func kindOf(t AlertType) (alertKind, bool) {
	for _, k := range AlertKinds {
		if k.Type == t {
			return k, true
		}
	}
	return alertKind{}, false
}

// Alert is one subscription.
type Alert struct {
	ID        string    `json:"id"`
	GuildID   string    `json:"guild_id,omitempty"`
	ChannelID string    `json:"channel_id"`
	Type      AlertType `json:"type"`
	Kind      string    `json:"kind"`   // "donor" or "team"
	Target    string    `json:"target"` // donor name, or team id as text
	Label     string    `json:"label"`  // the display name when it was created
	Threshold int64     `json:"threshold,omitempty"`
	// Tag is a ready-made mention — "<@123>" or "<@&456>". Stored rendered rather than
	// as an id because the alert has to work without knowing which of the two it is.
	Tag       string    `json:"tag,omitempty"`
	CreatedBy string    `json:"created_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`

	// Seen is the reading this alert last acted on. Its presence is what stops a new
	// alert firing for history: it is seeded at creation from a live fetch.
	Seen AlertState `json:"seen"`
	// Failures counts consecutive delivery errors; see deliver.
	Failures int `json:"failures,omitempty"`
}

// AlertState is everything a rule needs to tell a transition from a state.
type AlertState struct {
	Rank        int64     `json:"rank,omitempty"`
	PointsTotal int64     `json:"points_total,omitempty"`
	Last24h     int64     `json:"points_last_24h,omitempty"`
	Idle        bool      `json:"idle,omitempty"`
	Armed       bool      `json:"armed,omitempty"`      // rank/idle rules re-arm on clearing
	Milestone   int64     `json:"milestone,omitempty"`  // highest already announced
	QuietSince  time.Time `json:"quiet_since,omitzero"` // when production stopped
	LastFired   time.Time `json:"last_fired,omitzero"`
}

// Describe is the one-line rendering used by /alert list and the remove picker.
func (a *Alert) Describe() string {
	k, _ := kindOf(a.Type)
	var what string
	switch a.Type {
	case AlertRank:
		what = fmt.Sprintf("%s #%s", k.Blurb, n(a.Threshold))
	case AlertIdle:
		what = fmt.Sprintf("%s %s", k.Blurb, plural(int(a.Threshold), "hour"))
	case AlertDaily:
		what = fmt.Sprintf("%s %02d:00 UTC", k.Blurb, a.Threshold)
	default:
		what = k.Blurb
	}
	return fmt.Sprintf("%s — %s", a.Label, what)
}

// TargetURL points at the page the alert is about.
func (a *Alert) TargetURL() string {
	if a.Kind == "team" {
		return SiteURL + "/teams/" + a.Target
	}
	return SiteURL + "/donors/" + esc(a.Target)
}

/* ---------------------------------------------------------------- store --- */

// Alerts is the persisted set.
//
// Same shape as Links and for the same reasons — a JSON file, rewritten whole, read
// far more than written. It carries per-alert state as well as configuration, which
// means the file is written every time an alert's reading changes rather than only on
// edits. At a few hundred alerts that is one small write an hour.
type Alerts struct {
	path string

	mu sync.RWMutex
	m  map[string]*Alert
}

func OpenAlerts(path string) (*Alerts, error) {
	a := &Alerts{path: path, m: map[string]*Alert{}}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) || (err == nil && len(b) == 0) {
		return a, nil
	}
	if err != nil {
		return nil, err
	}
	var list []*Alert
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	for _, al := range list {
		a.m[al.ID] = al
	}
	return a, nil
}

// MaxPerChannel caps how many alerts one channel can carry.
//
// Not a resource limit — the cost of an alert is one shared fetch an hour. It is a
// blast-radius limit: the failure mode of this feature is a channel nobody can read
// any more, and that is far easier to create by accident than to notice.
const MaxPerChannel = 25

func (a *Alerts) Add(al *Alert) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	var inChannel int
	for _, e := range a.m {
		if e.ChannelID == al.ChannelID {
			inChannel++
		}
		// The same alert twice would double every message it ever sends.
		if e.ChannelID == al.ChannelID && e.Type == al.Type &&
			e.Kind == al.Kind && e.Target == al.Target && e.Threshold == al.Threshold {
			return fmt.Errorf("this channel already has that alert for %s", al.Label)
		}
	}
	if inChannel >= MaxPerChannel {
		return fmt.Errorf("this channel already has %d alerts, which is the limit", MaxPerChannel)
	}
	al.ID = newAlertID()
	a.m[al.ID] = al
	return a.saveLocked()
}

func (a *Alerts) Remove(id string) (*Alert, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	al, ok := a.m[id]
	if !ok {
		return nil, fmt.Errorf("no alert with that id — it may already have been removed")
	}
	delete(a.m, id)
	return al, a.saveLocked()
}

func (a *Alerts) Get(id string) (*Alert, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	al, ok := a.m[id]
	return al, ok
}

// InScope lists the alerts a given interaction is allowed to see and change.
//
// Scoped to the guild rather than the channel: somebody setting alerts up is usually
// looking at the whole server, and an alert in a channel they have since left is
// exactly the one they need to find in order to delete it. In a DM the scope is that
// one channel, because there is no guild to belong to.
func (a *Alerts) InScope(guildID, channelID string) []*Alert {
	a.mu.RLock()
	defer a.mu.RUnlock()
	var out []*Alert
	for _, al := range a.m {
		if (guildID != "" && al.GuildID == guildID) || (guildID == "" && al.ChannelID == channelID) {
			out = append(out, al)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// All returns a snapshot for the watcher to work through.
func (a *Alerts) All() []*Alert {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]*Alert, 0, len(a.m))
	for _, al := range a.m {
		out = append(out, al)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (a *Alerts) Count() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.m)
}

// Save persists whatever the watcher has changed in place.
func (a *Alerts) Save() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.saveLocked()
}

func (a *Alerts) saveLocked() error {
	list := make([]*Alert, 0, len(a.m))
	for _, al := range a.m {
		list = append(list, al)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(a.path), 0o755); err != nil {
		return err
	}
	tmp := a.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.path)
}

func newAlertID() string {
	var b [5]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Only reachable if the kernel's entropy source is broken, and an alert with a
		// time-based id is better than a bot that refuses to start.
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b[:])
}

/* ------------------------------------------------------------ milestones --- */

// Milestones are 1, 2 and 5 per decade from a million upward.
//
// Powers of ten alone are too sparse to be worth subscribing to — between 1B and 10B a
// good folder can spend a year — and every-half-decade is close enough to arbitrary
// that nobody would recognise the number as an achievement. 1/2/5 is the same
// progression an axis uses, for the same reason.
func milestoneAtOrBelow(points int64) int64 {
	var best int64
	for d := int64(1_000_000); d <= 1_000_000_000_000_000; d *= 10 {
		for _, m := range []int64{1, 2, 5} {
			if v := m * d; v <= points {
				best = v
			}
		}
	}
	return best
}

func nextMilestone(after int64) int64 {
	for d := int64(1_000_000); d <= 1_000_000_000_000_000; d *= 10 {
		for _, m := range []int64{1, 2, 5} {
			if v := m * d; v > after {
				return v
			}
		}
	}
	return 0
}

/* ---------------------------------------------------------------- rules --- */

// entity is the shared shape a rule reads, so one rule set covers donors and teams.
type entity struct {
	Name        string
	Rank        int64
	PointsTotal int64
	Last24h     int64
	PerDay      int64
	WUs         int64
}

// evaluate decides whether an alert should speak, and returns the new state to store.
//
// It is a pure function of (alert, reading, clock) so the rules can be tested without a
// Discord session, an HTTP server or a wall clock — which matters more here than
// anywhere else in the bot, because these are the code paths nobody watches run.
func evaluate(a *Alert, e entity, now time.Time) (fire bool, headline string, detail string, next AlertState) {
	next = a.Seen

	switch a.Type {
	case AlertMilestone:
		next.PointsTotal = e.PointsTotal
		reached := milestoneAtOrBelow(e.PointsTotal)
		// Seen.Milestone is seeded at creation, so only a crossing from here counts.
		if reached > a.Seen.Milestone {
			next.Milestone = reached
			next.LastFired = now
			return true, fmt.Sprintf("%s passed %s points", e.Name, short(reached)),
				fmt.Sprintf("Now on **%s** — next up %s.", n(e.PointsTotal), short(nextMilestone(reached))), next
		}

	case AlertRank:
		next.Rank = e.Rank
		if e.Rank > 0 && e.Rank <= a.Threshold {
			if a.Seen.Armed {
				next.Armed = false
				next.LastFired = now
				return true, fmt.Sprintf("%s reached #%s", e.Name, n(e.Rank)),
					fmt.Sprintf("Target was #%s. Now **#%s** on **%s** points.",
						n(a.Threshold), n(e.Rank), n(e.PointsTotal)), next
			}
		} else if e.Rank > a.Threshold {
			// Slipped back out, so the next crossing is worth hearing about again.
			next.Armed = true
		}

	case AlertIdle:
		next.Last24h = e.Last24h
		if e.Last24h > 0 {
			// Producing: reset the clock and re-arm.
			next.Idle, next.Armed, next.QuietSince = false, true, time.Time{}
			break
		}
		if next.QuietSince.IsZero() {
			next.QuietSince = now
		}
		quiet := now.Sub(next.QuietSince)
		if a.Seen.Armed && quiet >= time.Duration(a.Threshold)*time.Hour {
			next.Idle, next.Armed = true, false
			next.LastFired = now
			return true, fmt.Sprintf("%s has gone quiet", e.Name),
				fmt.Sprintf("No points for **%s**. Last seen on %s lifetime.",
					humanDur(quiet), n(e.PointsTotal)), next
		}

	case AlertResumed:
		was := a.Seen.Last24h == 0 && a.Seen.Armed
		next.Last24h = e.Last24h
		if e.Last24h > 0 {
			next.Armed = false
			if was {
				next.LastFired = now
				return true, fmt.Sprintf("%s is folding again", e.Name),
					fmt.Sprintf("**%s** in the last 24 hours, after a gap.", short(e.Last24h)), next
			}
		} else {
			next.Armed = true
		}

	case AlertDaily:
		// Evaluated on each new snapshot, so hour granularity is the finest that means
		// anything — and a date comparison rather than a 24-hour interval, or the hour
		// would drift later every day as publishes do.
		if now.UTC().Hour() >= int(a.Threshold) && !sameUTCDay(a.Seen.LastFired, now) {
			next.LastFired = now
			next.PointsTotal, next.Rank, next.Last24h = e.PointsTotal, e.Rank, e.Last24h
			return true, fmt.Sprintf("%s — daily summary", e.Name),
				fmt.Sprintf("**%s** in the last 24 hours · **%s** per day average\n"+
					"Rank **#%s** · **%s** points · **%s** work units",
					short(e.Last24h), short(e.PerDay), n(e.Rank), n(e.PointsTotal), n(e.WUs)), next
		}
	}
	return false, "", "", next
}

func sameUTCDay(a, b time.Time) bool {
	if a.IsZero() {
		return false
	}
	ay, am, ad := a.UTC().Date()
	by, bm, bd := b.UTC().Date()
	return ay == by && am == bm && ad == bd
}

// seed records the current reading so a new alert starts from now.
//
// Without this every alert fires on its first evaluation: a donor is already past a
// milestone, already inside a rank, already idle. The alert would be correct and
// useless — it would be announcing the past.
func seed(a *Alert, e entity, now time.Time) {
	a.Seen = AlertState{
		Rank:        e.Rank,
		PointsTotal: e.PointsTotal,
		Last24h:     e.Last24h,
		Milestone:   milestoneAtOrBelow(e.PointsTotal),
	}
	switch a.Type {
	case AlertRank:
		// Armed only if they are not already there; otherwise wait for a re-entry.
		a.Seen.Armed = e.Rank == 0 || e.Rank > a.Threshold
	case AlertIdle:
		a.Seen.Armed = e.Last24h > 0
		if e.Last24h == 0 {
			a.Seen.QuietSince = now
		}
	case AlertResumed:
		a.Seen.Armed = e.Last24h == 0
	}
}

// parseTarget reads what the target option carried.
//
// Autocomplete sends a tagged value — "t:51", "d:Anonymous" — because one option has to
// carry both kinds and a team's id is otherwise indistinguishable from a donor whose
// name is a number. Untagged text is somebody who typed rather than picked, and is
// resolved the way /team does it.
func parseTarget(v string) (kind, target string, tagged bool) {
	v = strings.TrimSpace(v)
	switch {
	case strings.HasPrefix(v, "t:"):
		return "team", strings.TrimPrefix(v, "t:"), true
	case strings.HasPrefix(v, "d:"):
		return "donor", strings.TrimPrefix(v, "d:"), true
	}
	return "", v, false
}
