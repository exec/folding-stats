package api

import (
	"sort"
	"sync"
)

const maxActivityMembers = 25

type activityList struct {
	total   int
	members []int32
}

type teamActivity struct {
	stopped, surging, joined activityList
	newKnown                 bool
}

type activityEntry struct {
	once    sync.Once
	summary *teamActivity
}

func (s *Snapshot) teamActivity(teamID int32) *teamActivity {
	roster := s.Ranks.TeamMembers(teamID)
	if s.activities == nil || len(roster) < rosterCacheMin {
		return s.buildTeamActivity(roster)
	}
	s.activityMu.Lock()
	e := s.activities[teamID]
	if e == nil {
		if len(s.activities) >= rosterCacheMax {
			s.activityMu.Unlock()
			return s.buildTeamActivity(roster)
		}
		e = &activityEntry{}
		s.activities[teamID] = e
	}
	s.activityMu.Unlock()
	e.once.Do(func() { e.summary = s.buildTeamActivity(roster) })
	return e.summary
}

func (s *Snapshot) buildTeamActivity(roster []int32) *teamActivity {
	type entry struct {
		slot int32
		by   int64
	}
	var stopped, surging, joined []entry
	out := &teamActivity{}
	for _, m := range roster {
		last24, last7 := s.Members.Last24h(m), s.Members.Last7d(m)
		if arrived, ok := s.Ranks.MemberArrivedSince24h(m); ok {
			out.newKnown = true
			if arrived {
				joined = append(joined, entry{m, s.State.Members[m].Score})
				// New members have no yesterday to have stopped or surged against.
				continue
			}
		}
		switch {
		case last7 > 0 && last24 == 0:
			// Rank by the production the team is missing while they are quiet.
			stopped = append(stopped, entry{m, last7})
		case last24 > 0:
			// A surge is relative to the member's own average, not the team's.
			if rate := s.Members.PointsPerDay(m); rate > 0 && float64(last24) >= surgeRatio*float64(rate) {
				surging = append(surging, entry{m, last24 - rate})
			}
		}
	}
	selectMembers := func(entries []entry) activityList {
		sort.Slice(entries, func(i, j int) bool { return entries[i].by > entries[j].by })
		// Retain only the largest allowed response, without keeping the full backing array.
		members := make([]int32, min(len(entries), maxActivityMembers))
		for i := range members {
			members[i] = entries[i].slot
		}
		return activityList{total: len(entries), members: members}
	}
	out.stopped, out.surging, out.joined = selectMembers(stopped), selectMembers(surging), selectMembers(joined)
	return out
}
