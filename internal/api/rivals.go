package api

import (
	"math"
	"net/http"
	"strconv"
	"time"
)

const (
	// overtakeHorizonDays bounds how far ahead a projection is reported.
	//
	// The input is a seven-day average. Extrapolating it across a decade is
	// arithmetic rather than information, and a gap of a trillion points closing at a
	// hundred a day yields a number with no meaning attached to it. Past the horizon
	// the answer is null, which says "not on any timescale worth naming" — the honest
	// reading — instead of dressing a rounding artefact up as a date.
	overtakeHorizonDays = 3650
)

// projectOvertake returns when two entities would swap places if both held their
// current daily rate, and the lifetime gap between them.
//
// Whoever is behind has to be gaining for this to have an answer at all, so the sign
// works out the same whichever side the subject is on: the trailing party's rate has
// to exceed the leading party's, and the gap closes at the difference.
//
// Nil means they never swap at current rates, which is the ordinary case — most
// neighbours are not converging, and most of those that are will not arrive this
// decade.
func projectOvertake(now time.Time, selfScore, selfRate, rivalScore, rivalRate int64) (int64, *float64, *time.Time) {
	gap := selfScore - rivalScore
	if gap < 0 {
		gap = -gap
	}
	// Already level: they are not going to swap so much as separate, and the answer
	// to "when" is "now".
	if gap == 0 {
		zero := 0.0
		at := now
		return 0, &zero, &at
	}

	// The one with fewer points is the one who has to do the catching.
	closing := selfRate - rivalRate
	if selfScore > rivalScore {
		closing = rivalRate - selfRate
	}
	if closing <= 0 {
		return gap, nil, nil
	}

	days := float64(gap) / float64(closing)
	if days > overtakeHorizonDays || math.IsInf(days, 0) || math.IsNaN(days) {
		return gap, nil, nil
	}
	// Two decimals: the input is a seven-day average, so any more precision than
	// "about this many days" is invented.
	days = math.Round(days*100) / 100
	at := now.Add(time.Duration(days * float64(24*time.Hour)))
	return gap, &days, &at
}

func (s *Server) teamRivals(snap *Snapshot, r *http.Request) (any, *PageInfo, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 32)
	if err != nil {
		return nil, nil, badRequest("team id must be an integer")
	}
	slot, ok := snap.State.TeamSlot(int32(id))
	if !ok {
		return nil, nil, notFound("no team with id %d", id)
	}
	self := snap.teamView(slot)
	order := snap.Ranks.TeamOrder
	lo, hi, page, err := paginateAround(r, len(order), int(self.Rank))
	if err != nil {
		return nil, nil, err
	}

	now := time.Now().UTC()
	out := Rivals{Rank: self.Rank, Name: self.Name, HorizonDays: overtakeHorizonDays}
	for _, near := range order[lo:hi] {
		// Built through the same view builder as every other team response, so a
		// rival's figures cannot drift from the ones on its own page.
		v := snap.teamView(near)
		teamID := v.TeamID
		gap, days, at := int64(0), (*float64)(nil), (*time.Time)(nil)
		// No projection against oneself. The gap is zero by definition, and the tie
		// branch would otherwise report "level now" on the reader's own row.
		if near != slot {
			gap, days, at = projectOvertake(now, self.PointsTotal, self.PointsPerDay7dAvg,
				v.PointsTotal, v.PointsPerDay7dAvg)
		}
		out.Rivals = append(out.Rivals, Rival{
			Rank: v.Rank, Name: v.Name, TeamID: &teamID, Self: near == slot,
			PointsTotal: v.PointsTotal, PointsPerDay7dAvg: v.PointsPerDay7dAvg,
			PointsGap: gap, OvertakeDays: days, OvertakeAt: at,
		})
	}
	return out, page, nil
}

func (s *Server) donorRivals(snap *Snapshot, r *http.Request) (any, *PageInfo, error) {
	idx, ok := snap.donorIndexByName(r.PathValue("name"))
	if !ok {
		return nil, nil, notFound("no donor named %q", r.PathValue("name"))
	}
	// Donors are stored in rank order, so a page of the ranking is a slice — there is
	// no separate order to look through as there is for teams.
	self := snap.donorView(idx, false)
	lo, hi, page, err := paginateAround(r, len(snap.Ranks.Donors), int(self.Rank))
	if err != nil {
		return nil, nil, err
	}

	now := time.Now().UTC()
	out := Rivals{Rank: self.Rank, Name: self.Name, HorizonDays: overtakeHorizonDays}
	for i := lo; i < hi; i++ {
		v := snap.donorView(int32(i), false)
		gap, days, at := int64(0), (*float64)(nil), (*time.Time)(nil)
		if int32(i) != idx { // see the note on the team path
			gap, days, at = projectOvertake(now, self.PointsTotal, self.PointsPerDay7dAvg,
				v.PointsTotal, v.PointsPerDay7dAvg)
		}
		out.Rivals = append(out.Rivals, Rival{
			Rank: v.Rank, Name: v.Name, Self: int32(i) == idx,
			PointsTotal: v.PointsTotal, PointsPerDay7dAvg: v.PointsPerDay7dAvg,
			PointsGap: gap, OvertakeDays: days, OvertakeAt: at,
		})
	}
	return out, page, nil
}
