package api

import (
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// insightKind normalises the two spellings of an entity kind to the singular.
//
// REST says team/donor and MCP says teams/donors, because each reads naturally in its
// own place — ?kind=team beside a=32, against a tool argument describing a collection.
// Both resolve here so that one resolver serves both surfaces rather than each keeping
// its own and drifting.
func insightKind(raw string) (string, error) {
	switch strings.TrimSuffix(strings.TrimSpace(raw), "s") {
	case "team":
		return "team", nil
	case "donor":
		return "donor", nil
	default:
		return "", badRequest("kind must be team or donor")
	}
}

func (s *Snapshot) insightEntity(kind, ref string) (InsightEntity, error) {
	switch kind {
	case "team":
		id, err := strconv.ParseInt(strings.TrimSpace(ref), 10, 32)
		if err != nil {
			return InsightEntity{}, badRequest("%q is not a team number", ref)
		}
		slot, ok := s.State.TeamSlot(int32(id))
		if !ok {
			return InsightEntity{}, notFound("no team with id %d", id)
		}
		v := s.teamView(slot)
		teamID := v.TeamID
		return InsightEntity{Kind: kind, Name: v.Name, TeamID: &teamID, Rank: v.Rank,
			PointsTotal: v.PointsTotal, PointsPerDay24hAvg: v.PointsPerDay24hAvg,
			PointsPerDay7dAvg: v.PointsPerDay7dAvg}, nil
	case "donor":
		idx, ok := s.donorIndexByName(ref)
		if !ok {
			return InsightEntity{}, notFound("no donor named %q", ref)
		}
		v := s.donorView(idx, false)
		return InsightEntity{Kind: kind, Name: v.Name, Rank: v.Rank,
			PointsTotal: v.PointsTotal, PointsPerDay24hAvg: v.PointsPerDay24hAvg,
			PointsPerDay7dAvg: v.PointsPerDay7dAvg}, nil
	}
	return InsightEntity{}, badRequest("kind must be team or donor")
}

func (s *Snapshot) insightAtRank(kind string, rank int) (InsightEntity, error) {
	if rank < 1 {
		return InsightEntity{}, badRequest("rank %d is outside the rankings; the first is 1", rank)
	}
	switch kind {
	case "team":
		if rank > len(s.Ranks.TeamOrder) {
			return InsightEntity{}, badRequest("rank %d is outside the %s teams there are", rank, fmtInt(int64(len(s.Ranks.TeamOrder))))
		}
		v := s.teamView(s.Ranks.TeamOrder[rank-1])
		id := v.TeamID
		return InsightEntity{Kind: kind, Name: v.Name, TeamID: &id, Rank: v.Rank,
			PointsTotal: v.PointsTotal, PointsPerDay24hAvg: v.PointsPerDay24hAvg,
			PointsPerDay7dAvg: v.PointsPerDay7dAvg}, nil
	case "donor":
		if rank > len(s.Ranks.Donors) {
			return InsightEntity{}, badRequest("rank %d is outside the %s donors there are", rank, fmtInt(int64(len(s.Ranks.Donors))))
		}
		v := s.donorView(int32(rank-1), false)
		return InsightEntity{Kind: kind, Name: v.Name, Rank: v.Rank,
			PointsTotal: v.PointsTotal, PointsPerDay24hAvg: v.PointsPerDay24hAvg,
			PointsPerDay7dAvg: v.PointsPerDay7dAvg}, nil
	}
	return InsightEntity{}, badRequest("kind must be team or donor")
}

func (s *Server) compare(snap *Snapshot, r *http.Request) (any, *PageInfo, error) {
	kind, err := insightKind(r.URL.Query().Get("kind"))
	if err != nil {
		return nil, nil, err
	}
	aRef, bRef := r.URL.Query().Get("a"), r.URL.Query().Get("b")
	if aRef == "" || bRef == "" {
		return nil, nil, badRequest("a and b are required")
	}
	a, err := snap.insightEntity(kind, aRef)
	if err != nil {
		return nil, nil, err
	}
	b, err := snap.insightEntity(kind, bRef)
	if err != nil {
		return nil, nil, err
	}
	gap, days, at := projectOvertake(snap.At, a.PointsTotal, a.PointsPerDay24hAvg,
		b.PointsTotal, b.PointsPerDay24hAvg)
	leader := "tie"
	if a.PointsTotal > b.PointsTotal {
		leader = "a"
	} else if b.PointsTotal > a.PointsTotal {
		leader = "b"
	}
	return Comparison{Kind: kind, A: a, B: b, Leader: leader, PointsGap: gap,
		OvertakeDays: days, OvertakeAt: at, HorizonDays: overtakeHorizonDays}, nil, nil
}

// requiredRate is what the subject must average to arrive at the target in days.
//
// The moving-target correction is the part worth having, and the reason this is shared
// rather than written twice: overtaking somebody who is also producing costs the gap
// plus everything they add in the meantime, so a naive gap ÷ days understates it,
// sometimes by more than the gap itself.
//
// Not clamped. A non-positive result means the target needs no new production to be
// reached, which REST reports as zero and MCP says in words — the same arithmetic
// reaching two audiences, which is exactly the split worth keeping.
func requiredRate(self, target InsightEntity, days float64) int64 {
	finish := float64(target.PointsTotal) + float64(target.PointsPerDay24hAvg)*days
	return int64(math.Ceil((finish - float64(self.PointsTotal)) / days))
}

func (s *Server) goal(snap *Snapshot, r *http.Request) (any, *PageInfo, error) {
	kind, err := insightKind(r.URL.Query().Get("kind"))
	if err != nil {
		return nil, nil, err
	}
	who := r.URL.Query().Get("who")
	if who == "" {
		return nil, nil, badRequest("who is required")
	}
	self, err := snap.insightEntity(kind, who)
	if err != nil {
		return nil, nil, err
	}

	q := r.URL.Query()
	targets := 0
	for _, k := range []string{"target_rank", "target_points", "overtake"} {
		if q.Get(k) != "" {
			targets++
		}
	}
	if targets != 1 {
		return nil, nil, badRequest("give exactly one of target_rank, target_points or overtake")
	}

	var target InsightEntity
	targetType := ""
	targetRank := 0
	switch {
	case q.Get("target_rank") != "":
		targetRank, err = strconv.Atoi(q.Get("target_rank"))
		if err != nil {
			return nil, nil, badRequest("target_rank must be an integer")
		}
		target, err = snap.insightAtRank(kind, targetRank)
		targetType = "rank"
	case q.Get("target_points") != "":
		points, parseErr := strconv.ParseInt(q.Get("target_points"), 10, 64)
		if parseErr != nil || points <= 0 {
			return nil, nil, badRequest("target_points must be a positive integer")
		}
		target = InsightEntity{Kind: kind, Name: fmt.Sprintf("%d points", points), PointsTotal: points}
		targetType = "points"
	default:
		target, err = snap.insightEntity(kind, q.Get("overtake"))
		targetType = "overtake"
	}
	if err != nil {
		return nil, nil, err
	}

	holding := self.PointsTotal >= target.PointsTotal
	result := Goal{Kind: kind, Subject: self, Target: target, TargetType: targetType,
		TargetRank: targetRank, Holding: holding,
		AlreadyReached: holding && self.PointsPerDay24hAvg >= target.PointsPerDay24hAvg,
		PointsGap:      self.PointsTotal - target.PointsTotal}
	if result.PointsGap < 0 {
		result.PointsGap = -result.PointsGap
	}

	if raw := q.Get("by"); raw != "" {
		when, parseErr := time.Parse("2006-01-02", raw)
		if parseErr != nil {
			return nil, nil, badRequest("by must be a date like 2026-12-31")
		}
		days := when.Sub(snap.At).Hours() / 24
		if days <= 0 {
			return nil, nil, badRequest("by must be after the snapshot date")
		}
		rate := max64(0, requiredRate(self, target, days))
		result.By, result.RequiredBy = &when, &rate
		return result, nil, nil
	}

	_, result.CurrentOvertakeDays, result.CurrentOvertakeAt = projectOvertake(snap.At,
		self.PointsTotal, self.PointsPerDay24hAvg, target.PointsTotal, target.PointsPerDay24hAvg)
	for _, days := range []int{30, 90, 365} {
		rate := max64(0, requiredRate(self, target, float64(days)))
		if rate == 0 {
			continue
		}
		multiple := 0.0
		if self.PointsPerDay24hAvg > 0 {
			multiple = math.Round(float64(rate)/float64(self.PointsPerDay24hAvg)*100) / 100
		}
		result.Horizons = append(result.Horizons, GoalHorizon{Days: days, PointsPerDay: rate, Multiple: multiple})
	}
	return result, nil, nil
}

func (s *Server) movers(snap *Snapshot, r *http.Request) (any, *PageInfo, error) {
	kind, err := insightKind(r.URL.Query().Get("kind"))
	if err != nil {
		return nil, nil, err
	}
	within, err := intParam(r, "within", 1000)
	if err != nil || within < 10 || within > 10000 {
		return nil, nil, badRequest("within must be between 10 and 10000")
	}
	limit, err := intParam(r, "limit", 10)
	if err != nil || limit < 1 || limit > 25 {
		return nil, nil, badRequest("limit must be between 1 and 25")
	}
	direction := r.URL.Query().Get("direction")
	if direction == "" {
		direction = "both"
	}
	if direction != "up" && direction != "down" && direction != "both" {
		return nil, nil, badRequest("direction must be up, down or both")
	}

	result := Movers{Kind: kind, Within: within, Climbed: []Mover{}, Fell: []Mover{}}
	var all []Mover
	if kind == "team" {
		result.FieldSize = len(snap.Ranks.TeamOrder)
		for _, slot := range snap.Ranks.TeamOrder[:min(within, result.FieldSize)] {
			change, ok := snap.Ranks.TeamChange24h(slot)
			if !ok || change == 0 {
				continue
			}
			v := snap.teamView(slot)
			id := v.TeamID
			all = append(all, Mover{InsightEntity: InsightEntity{Kind: kind, Name: v.Name, TeamID: &id,
				Rank: v.Rank, PointsTotal: v.PointsTotal, PointsPerDay24hAvg: v.PointsPerDay24hAvg,
				PointsPerDay7dAvg: v.PointsPerDay7dAvg}, Change24h: change})
		}
	} else {
		result.FieldSize = len(snap.Ranks.Donors)
		for i := 0; i < min(within, result.FieldSize); i++ {
			change, ok := snap.Ranks.DonorChange24h(int32(i))
			if !ok || change == 0 {
				continue
			}
			v := snap.donorView(int32(i), false)
			all = append(all, Mover{InsightEntity: InsightEntity{Kind: kind, Name: v.Name,
				Rank: v.Rank, PointsTotal: v.PointsTotal, PointsPerDay24hAvg: v.PointsPerDay24hAvg,
				PointsPerDay7dAvg: v.PointsPerDay7dAvg}, Change24h: change})
		}
	}

	sort.Slice(all, func(i, j int) bool { return all[i].Change24h > all[j].Change24h })
	if direction != "down" {
		for _, m := range all {
			if m.Change24h <= 0 || len(result.Climbed) == limit {
				break
			}
			result.Climbed = append(result.Climbed, m)
		}
	}
	if direction != "up" {
		for i := len(all) - 1; i >= 0 && len(result.Fell) < limit; i-- {
			if all[i].Change24h >= 0 {
				break
			}
			result.Fell = append(result.Fell, all[i])
		}
	}
	return result, nil, nil
}
