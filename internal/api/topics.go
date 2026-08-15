package api

import (
	"net/http"
	"sort"
	"strings"
)

// topicDef is the country list's sibling, with one deliberate difference: a team may
// appear under several topics. PC Games Hardware is a German hardware magazine,
// Curecoin is a cryptocurrency whose whole purpose is disease research, and
// GamersNexus is a creator and a hardware outlet. Forcing one label each would make
// most of them arbitrary.
//
// Nationality is not a topic. That is the country list, and repeating it here would
// put the same teams and the same production on two pages.
type topicDef struct {
	Slug        string
	Name        string
	Description string
	TeamIDs     []int32
}

// Curated from the 999 teams folding today, by recognition rather than by pattern.
// Countries could be matched on their name; topics cannot. [H]ardOCP is a hardware
// forum whose name never says "hardware", and a keyword pass over the same list
// reached only a third of it while filing Team UnRAID under games for the "raid" in
// it, ASRockMania under music for the "rock", and homelab under research for the
// "lab". Teams that have stopped folding are a later pass.
var topicDefs = []topicDef{
	{Slug: "hardware", Name: "Hardware & overclocking",
		Description: "Forums, magazines and communities built around building, tuning and cooling PCs.",
		TeamIDs: []int32{
			111065, 32, 37651, 70335, 33, 198, 11108, 37726, 40051, 35947, 50711, 37451, 251999, 24,
			239902, 257944, 64, 70911, 252872, 4, 3074, 11314, 32377, 98860, 3, 69411, 15, 54196,
			3446, 734, 1971, 142900, 32407, 232084, 12772, 34361, 33258, 148894, 236734, 31574,
			239968, 241865, 245755, 237039, 13802, 44079, 36362, 36673, 62, 38910, 33272, 38608,
			46590, 37412, 39655, 51394, 3007, 34106, 12072, 1714, 35126, 242601, 13761, 257618,
			44352, 183368, 45456, 268855, 238272, 156571, 34149, 254140, 236284, 246916, 235955, 18,
			243288, 13505, 90831, 213698, 1061684, 252180, 175532,
		}},
	{Slug: "creators", Name: "Creators, podcasts & tech media",
		Description: "Teams that formed around a channel, a show or a masthead.",
		TeamIDs: []int32{
			223518, 86565, 234771, 44851, 57391, 14, 1066966, 1066076, 231300, 2233, 35054, 244613,
			250966, 39227, 263429, 237882, 260722, 227867, 235869, 243047, 230319, 239672, 240286,
			240297, 69263, 201140, 34479, 54458, 68208, 257787, 171563, 243272, 1068058,
		}},
	{Slug: "forums", Name: "Chat & forum communities",
		Description: "Places people already talk, that put a folding team behind the conversation.",
		TeamIDs: []int32{
			225605, 162, 229500, 150, 236269, 50959, 35819, 93, 12679, 13854, 235850, 155945, 97103,
			55265, 90478, 241653, 1068298, 241957, 1067065, 48658, 46304, 32598,
		}},
	{Slug: "distributed", Name: "Distributed computing & crypto",
		Description: "Crunchers who came from another project, and coins that reward the work.",
		TeamIDs: []int32{
			234980, 224497, 226715, 226804, 1067140, 43781, 13180, 262156, 238663, 236370, 43829,
			1065229, 1066815, 253230, 61171, 200905, 149281, 1066779,
		}},
	{Slug: "universities", Name: "Universities & schools",
		Description: "Campuses, departments, labs and the odd high school.",
		TeamIDs: []int32{
			86926, 235935, 10033, 40211, 1287, 74600, 59515, 1064729, 717, 243112, 10276, 38228,
			236709, 13515, 252914, 264460, 163348, 148892, 1805, 59, 1064918, 247217, 256648, 3357,
			3103, 34242, 52552, 11534, 246110, 261829, 10971, 40247, 41167, 31658, 50511, 716, 235,
			2866, 44171, 1067555, 257898, 171061, 50945, 163473, 234483, 1065368, 3430, 1067225,
			13622, 266722, 157986, 209894, 250694, 231250, 1061333, 259414, 3212, 179680, 229984,
			149424, 204860, 1062169, 1066016, 240755, 1067245, 265346, 231006, 56606,
		}},
	{Slug: "employers", Name: "Employers & industry",
		Description: "Teams that exist because of where people work.",
		TeamIDs: []int32{
			259918, 246129, 711, 52737, 229957, 999, 254402, 32035, 43573, 1739, 446, 11681, 256378,
			236798, 11142, 229258, 262556, 1062270, 241916, 243108, 261972, 249708, 59547, 249560,
			2654, 250299, 240650, 1991, 227455, 243214, 243801, 236760, 247019, 245058, 1003, 261092,
			33577, 267356, 54238, 244604, 243541, 50979, 134653, 236584, 253915, 604, 37742, 224746,
			252034, 267635, 249559, 1068101, 251626, 234111, 10022, 251803, 253541, 256729, 257196,
			259665, 239347, 263034, 252071, 1068136, 112, 264085, 1068232, 263289, 41281, 1067373,
			245856, 260500,
		}},
	{Slug: "opensource", Name: "Linux, BSD & open source",
		Description: "Distributions, projects and the people who run them at home.",
		TeamIDs: []int32{
			76140, 39340, 45032, 227802, 45104, 11298, 247478, 11743, 163, 2019, 251916, 239813,
			247646, 2190, 36480, 236565, 53140, 246632, 234437, 234067, 240045,
		}},
	{Slug: "research", Name: "Science & research labs",
		Description: "The laboratories on the other end of the work units, and their neighbours.",
		TeamIDs: []int32{
			1, 38188, 239945, 264560, 1063131, 131, 77397, 13203, 1061083, 10105, 1063263, 258105,
			32461, 13, 252841, 157071, 3528, 202953, 1065818, 156662, 239228, 1066745,
		}},
	{Slug: "health", Name: "Health & disease advocacy",
		Description: "Teams folding for one illness, or for the people living with it.",
		TeamIDs: []int32{
			254500, 198251, 245171, 244051, 52388, 236962, 7, 1063446, 268715, 41355, 249314,
			1067730, 71500, 1068197, 1097, 246095, 47429, 69998, 1066792, 230948, 1068093,
		}},
	{Slug: "faith", Name: "Faith & religion",
		Description: "Congregations and believers, folding together.",
		TeamIDs: []int32{
			1067884, 227204, 198251, 156823, 38412, 1066690, 41355, 1068093, 266266,
		}},
	{Slug: "secular", Name: "Secular & humanist",
		Description: "Atheist, humanist and skeptic communities.",
		TeamIDs: []int32{
			182116, 34395, 157440, 231020,
		}},
	{Slug: "memorial", Name: "In memory of",
		Description: "Teams named for someone.",
		TeamIDs: []int32{
			36120, 264796, 1067736, 1067821, 234455, 255070, 47429, 54197,
		}},
	{Slug: "radio", Name: "Amateur radio",
		Description: "Hams, satellites and callsigns.",
		TeamIDs: []int32{
			55236, 246763, 341, 69710, 227388, 72200, 1067515, 174971,
		}},
	{Slug: "fandom", Name: "Furry, anime & fandom",
		Description: "Fandoms that turned into folding teams.",
		TeamIDs: []int32{
			257728, 212997, 230362, 60091, 255396, 236749, 34645, 236605, 258804, 238918, 167809,
			237236, 252162, 236705, 246305,
		}},
	{Slug: "tabletop", Name: "Tabletop, sci-fi & books",
		Description: "Star Trek, D&D, boardgames and the fleets people invent.",
		TeamIDs: []int32{
			169927, 54345, 117, 41481, 23, 245299, 82447, 249855, 53338, 25,
		}},
	{Slug: "gaming", Name: "Games & virtual worlds",
		Description: "A game, a server or a world that people fold for.",
		TeamIDs: []int32{
			259009, 243287, 143016, 1068228, 264122, 1065597, 245209, 1060820, 267810, 232000,
			237128, 2582, 1067137, 117210, 227654,
		}},
	{Slug: "lgbtq", Name: "LGBTQ+",
		Description: "Queer folders and the teams they made.",
		TeamIDs: []int32{
			261045, 230510, 1063887, 1061351, 1065909, 1067473,
		}},
	{Slug: "service", Name: "Military, veterans & emergency services",
		Description: "Forces, first responders and the people who served.",
		TeamIDs: []int32{
			135860, 13051, 74413, 234447, 54234, 11126, 1066792,
		}},
	{Slug: "motoring", Name: "Cars, bikes & motorsport",
		Description: "Marques, forums and race teams.",
		TeamIDs: []int32{
			35780, 104636, 261120, 45435, 50906, 266969, 1061333, 244166,
		}},
	{Slug: "sports", Name: "Sports fandom",
		Description: "Clubs and the people who follow them.",
		TeamIDs: []int32{
			256656, 263245, 50906, 59, 242509, 231250, 116608, 1064945,
		}},
	{Slug: "music", Name: "Music",
		Description: "Bands, labels and listeners.",
		TeamIDs: []int32{
			258681, 1068171, 260952, 251387, 1068058, 1068326,
		}},
	{Slug: "politics", Name: "Politics & activism",
		Description: "Teams organised around a cause or a party.",
		TeamIDs: []int32{
			36120, 1067634, 11232,
		}},
}

func topicDefBySlug(slug string) (topicDef, bool) {
	slug = strings.ToLower(slug)
	for _, def := range topicDefs {
		if def.Slug == slug {
			return def, true
		}
	}
	return topicDef{}, false
}

// Topic is the combined production of every team under one heading. Teams is capped
// on the collection endpoint and complete on the detail endpoint, as for a country.
type Topic struct {
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description"`
	TeamsTotal  int    `json:"teams_total"`
	TeamsActive int    `json:"teams_active"`
	Teams       []Team `json:"teams"`
	Production
}

func (s *Snapshot) topicView(def topicDef, limit int) Topic {
	t := Topic{Slug: def.Slug, Name: def.Name, Description: def.Description, Teams: []Team{}}
	for _, id := range def.TeamIDs {
		slot, ok := s.State.TeamSlot(id)
		if !ok {
			continue
		}
		team := s.teamView(slot)
		t.Teams = append(t.Teams, team)
		t.TeamsTotal++
		if team.PointsLast7d > 0 {
			t.TeamsActive++
		}
		t.PointsTotal += team.PointsTotal
		t.WUsTotal += team.WUsTotal
		t.PointsLastCycle += team.PointsLastCycle
		t.PointsLast24h += team.PointsLast24h
		t.PointsLast7d += team.PointsLast7d
		t.PointsTodayUTC += team.PointsTodayUTC
		t.PointsThisWeekUTC += team.PointsThisWeekUTC
		t.PointsThisMonthUTC += team.PointsThisMonthUTC
		t.PointsPerDay24hAvg += team.PointsPerDay24hAvg
		t.PointsPerDay7dAvg += team.PointsPerDay7dAvg
	}
	sort.Slice(t.Teams, func(i, j int) bool {
		return t.Teams[i].PointsTotal > t.Teams[j].PointsTotal
	})
	t.PointsPerWU = perWU(t.PointsTotal, t.WUsTotal)
	if limit > 0 && len(t.Teams) > limit {
		t.Teams = t.Teams[:limit]
	}
	return t
}

func (s *Server) topics(snap *Snapshot, _ *http.Request) (any, *PageInfo, error) {
	out := make([]Topic, 0, len(topicDefs))
	for _, def := range topicDefs {
		out = append(out, snap.topicView(def, 6))
	}
	// By team count rather than production. A topic's size is how many people it
	// gathered, and ordering by points would bury amateur radio and the LGBTQ+ teams
	// under a handful of overclockers with more GPUs — which is the opposite of what
	// somebody browsing for their own corner of this is looking for.
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].TeamsTotal > out[j].TeamsTotal
	})
	return out, nil, nil
}

func (s *Server) topic(snap *Snapshot, r *http.Request) (any, *PageInfo, error) {
	slug := r.PathValue("slug")
	if def, ok := topicDefBySlug(slug); ok {
		return snap.topicView(def, 0), nil, nil
	}
	return nil, nil, notFound("no topic with slug %q", slug)
}
