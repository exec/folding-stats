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

// donorTeamRivals is the neighbourhood inside one team rather than across the site.
//
// The two are different competitions and people care about the second one more. A
// donor is rank 48,213 of two million, which moves on a timescale nobody watches; they
// are also 7th of 340 on their own team, where the person two places up is somebody
// they know and can actually catch this month. The global board cannot show that — a
// teammate is thousands of rows away in it — so the same question needs its own field.
//
// The roster is already stored best-first, and in-team rank is a position in it, so
// this is a slice of an existing order and costs no more than the global one.
func (s *Server) donorTeamRivals(snap *Snapshot, r *http.Request, name string, teamID int32) (any, *PageInfo, error) {
	slot, ok := snap.memberSlot(name, teamID)
	if !ok {
		return nil, nil, notFound("%q does not fold for team %d", name, teamID)
	}
	teamSlot, ok := snap.State.TeamSlot(teamID)
	if !ok {
		return nil, nil, notFound("no team with id %d", teamID)
	}
	roster := snap.Ranks.TeamMembers(teamID)
	self := snap.memberView(slot, false)

	lo, hi, page, err := paginateAround(r, len(roster), int(self.RankInTeam))
	if err != nil {
		return nil, nil, err
	}

	now := time.Now().UTC()
	out := Rivals{
		Rank: self.RankInTeam, Name: self.Name, HorizonDays: overtakeHorizonDays,
		TeamID: &teamID, TeamName: snap.State.Names.Name(snap.State.Teams[teamSlot].NameID),
	}
	for _, near := range roster[lo:hi] {
		v := snap.memberView(near, false)
		gap, days, at := int64(0), (*float64)(nil), (*time.Time)(nil)
		if near != slot { // see the note on the team path
			gap, days, at = projectOvertake(now, self.PointsTotal, self.PointsPerDay7dAvg,
				v.PointsTotal, v.PointsPerDay7dAvg)
		}
		out.Rivals = append(out.Rivals, Rival{
			// The in-team position, not the global one: a list ranked 1..n against
			// ranks in the hundred-thousands would read as a different table.
			Rank: v.RankInTeam, Name: v.Name, Self: near == slot,
			PointsTotal: v.PointsTotal, PointsPerDay7dAvg: v.PointsPerDay7dAvg,
			PointsGap: gap, OvertakeDays: days, OvertakeAt: at,
		})
	}
	return out, page, nil
}

func (s *Server) donorRivals(snap *Snapshot, r *http.Request) (any, *PageInfo, error) {
	name := r.PathValue("name")
	// ?team_id= narrows the field to one team's roster, the same way it narrows a
	// donor's history to one team's production.
	if v := r.URL.Query().Get("team_id"); v != "" {
		id, err := strconv.ParseInt(v, 10, 32)
		if err != nil {
			return nil, nil, badRequest("team_id must be an integer")
		}
		return s.donorTeamRivals(snap, r, name, int32(id))
	}
	idx, ok := snap.donorIndexByName(name)
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
