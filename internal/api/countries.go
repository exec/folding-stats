package api

import (
	"net/http"
	"sort"
	"strings"
)

// countryDef is the deliberately boring source of truth for approved assignments.
// A future request workflow only has to add a numeric team id here after review; names
// and every displayed figure continue to come from the current Folding@home snapshot.
type countryDef struct {
	Code    string
	Name    string
	TeamIDs []int32
}

// Generated from the top 1000 teams by 24h PPD on 2026-08-15. Names and every
// displayed figure still come from the live snapshot; this list only says which
// teams a country claims. Teams whose identity is global — creator communities,
// crypto projects, faith and fandom groups — are deliberately absent.
var countryDefs = []countryDef{
	{Code: "US", Name: "United States", TeamIDs: []int32{
		999, 32, 111065, 33, 11108, 40051, 37726, 246129, 1066076, 1063131, 198, 234771, 1, 15,
		40098, 3074, 1068131, 257944, 57391, 40965, 36120, 734, 14, 11314, 1805, 88682, 249708,
		35054, 77397, 259918, 39227, 10276, 33258, 32035, 4, 1064729, 44851, 717, 716, 3446,
		249560, 1287, 2233, 247019, 43573, 55236, 64534, 1064918, 13180, 31658, 148892, 38228,
		1067634, 11681, 236370, 1003, 2653, 446, 13515, 10033, 243214, 50959, 262556, 243210,
		237882, 33147, 250299, 1739, 3007, 243112, 32407, 232084, 35126, 40247, 236760, 50511,
		260722, 261829, 74413, 227867, 59515, 38928, 1062270, 229258, 40211, 163348, 3103,
		227455, 243253, 247217, 41167, 34479, 3212, 44352, 2654, 13051, 1066882, 93, 171061,
		52737, 243801, 252071, 1066028, 37742, 236584, 55260, 10971, 711, 239945, 68208, 50906,
		251626, 42028, 248140, 245058, 33577, 257104, 34687, 604, 50979, 11232, 11534, 231300,
		243108, 34149, 134653, 1061333, 11142, 116608, 227286, 1701, 3430, 163473, 1991, 224746,
		261972, 52552, 12072, 1066792, 54238, 231250, 209894, 259665, 235, 256729, 1062094,
		234523, 256378, 86926, 249559, 1067245, 112, 1066120, 235955, 180172, 236709, 45435,
		236284, 267635, 1063984, 240045, 69710, 13761, 263476, 259414, 13622, 213698, 257196,
		264460, 240755, 250186, 253811, 39797, 59, 204860, 3357, 252180, 1065368, 245860, 50945,
		168145, 245755, 1066016, 257991,
	}},
	{Code: "DE", Name: "Germany", TeamIDs: []int32{
		240890, 70335, 70911, 34361, 3, 251999, 240251, 235935, 1068028, 229957, 265898, 256656,
		45456, 267356, 263429, 243047, 226201, 43829, 257831, 235850, 53140, 1067091, 252034,
		253305, 1067373, 246632, 10022, 266722, 45700, 242601, 240286, 264821, 241412, 239199,
		265346, 45876, 243871, 70408, 1065430, 264169, 250660, 229984,
	}},
	{Code: "NO", Name: "Norway", TeamIDs: []int32{
		37651, 37827, 240650, 264955, 183891, 237481, 243541, 237638,
	}},
	{Code: "RU", Name: "Russia", TeamIDs: []int32{
		47191, 122296, 279,
	}},
	{Code: "GB", Name: "United Kingdom", TeamIDs: []int32{
		35947, 250966, 244613, 98860, 61671, 37363, 1068232, 251803, 69263, 588, 263289, 1064077,
		252914, 1068331, 54234, 242995, 258322, 46590, 234111, 245403, 244604, 243272,
	}},
	{Code: "CN", Name: "China", TeamIDs: []int32{
		3213, 253541,
	}},
	{Code: "AU", Name: "Australia", TeamIDs: []int32{
		24, 43781, 267810, 47155, 225000, 186, 267244, 246095, 44171, 243288, 46401, 266266,
	}},
	{Code: "TW", Name: "Taiwan", TeamIDs: []int32{
		31403,
	}},
	{Code: "BY", Name: "Belarus", TeamIDs: []int32{
		11897, 773,
	}},
	{Code: "CA", Name: "Canada", TeamIDs: []int32{
		10733, 10987, 60, 34242, 1068265, 31574, 54196, 37412, 253800, 1064775, 256648,
	}},
	{Code: "SE", Name: "Sweden", TeamIDs: []int32{
		37451, 245476, 239347, 831, 247646, 1068136, 74600, 260500,
	}},
	{Code: "NZ", Name: "New Zealand", TeamIDs: []int32{
		32887, 246916,
	}},
	{Code: "NL", Name: "Netherlands", TeamIDs: []int32{
		92, 69411, 13505, 1288, 48658, 1067515,
	}},
	{Code: "JP", Name: "Japan", TeamIDs: []int32{
		162, 254402, 222, 255396, 258804, 257728, 179680, 261092, 253230, 254140, 1067225,
		264379, 257261, 1064945,
	}},
	{Code: "FR", Name: "France", TeamIDs: []int32{
		45363, 200905, 242460, 247377, 236445, 251916, 10317,
	}},
	{Code: "UA", Name: "Ukraine", TeamIDs: []int32{
		2164, 156571,
	}},
	{Code: "VN", Name: "Vietnam", TeamIDs: []int32{
		38156,
	}},
	{Code: "PL", Name: "Poland", TeamIDs: []int32{
		276, 1066850, 77920,
	}},
	{Code: "BR", Name: "Brazil", TeamIDs: []int32{
		148894, 13802, 231006, 1062169, 266910, 53066,
	}},
	{Code: "FI", Name: "Finland", TeamIDs: []int32{
		62, 239618, 231988, 454, 38850, 254869, 250694,
	}},
	{Code: "HU", Name: "Hungary", TeamIDs: []int32{
		239968, 43299, 679,
	}},
	{Code: "GR", Name: "Greece", TeamIDs: []int32{
		36673, 1065818, 13416, 838, 3322, 1068194, 44079,
	}},
	{Code: "CZ", Name: "Czechia", TeamIDs: []int32{
		236798, 49658, 235910, 1067491, 236157, 157986, 246110, 240727, 236542,
	}},
	{Code: "IT", Name: "Italy", TeamIDs: []int32{
		246330, 61171, 252841, 256411, 180924,
	}},
	{Code: "MY", Name: "Malaysia", TeamIDs: []int32{
		2999,
	}},
	{Code: "ID", Name: "Indonesia", TeamIDs: []int32{
		1068346, 38608,
	}},
	{Code: "CH", Name: "Switzerland", TeamIDs: []int32{
		2866, 38188, 264890,
	}},
	{Code: "TR", Name: "Turkey", TeamIDs: []int32{
		39655,
	}},
	{Code: "IE", Name: "Ireland", TeamIDs: []int32{
		60443, 257898,
	}},
	{Code: "MV", Name: "Maldives", TeamIDs: []int32{
		32777,
	}},
	{Code: "DK", Name: "Denmark", TeamIDs: []int32{
		1068101, 262701, 69476, 245856, 244,
	}},
	{Code: "PT", Name: "Portugal", TeamIDs: []int32{
		1068128, 35271, 498,
	}},
	{Code: "BH", Name: "Bahrain", TeamIDs: []int32{
		34353,
	}},
	{Code: "BN", Name: "Brunei", TeamIDs: []int32{
		135369,
	}},
	{Code: "EE", Name: "Estonia", TeamIDs: []int32{
		385,
	}},
	{Code: "BG", Name: "Bulgaria", TeamIDs: []int32{
		845, 32276,
	}},
	{Code: "BE", Name: "Belgium", TeamIDs: []int32{
		3455, 37639, 239672,
	}},
	{Code: "LT", Name: "Lithuania", TeamIDs: []int32{
		36816, 3558,
	}},
	{Code: "HR", Name: "Croatia", TeamIDs: []int32{
		137516,
	}},
	{Code: "SG", Name: "Singapore", TeamIDs: []int32{
		197,
	}},
	{Code: "AT", Name: "Austria", TeamIDs: []int32{
		1604, 263034,
	}},
	{Code: "PH", Name: "Philippines", TeamIDs: []int32{
		2291,
	}},
	{Code: "SI", Name: "Slovenia", TeamIDs: []int32{
		299,
	}},
	{Code: "KR", Name: "South Korea", TeamIDs: []int32{
		149281,
	}},
	{Code: "ES", Name: "Spain", TeamIDs: []int32{
		1250, 149424,
	}},
	{Code: "ZA", Name: "South Africa", TeamIDs: []int32{
		234483,
	}},
	{Code: "TH", Name: "Thailand", TeamIDs: []int32{
		52128, 1064025,
	}},
	{Code: "DZ", Name: "Algeria", TeamIDs: []int32{
		1067419,
	}},
	{Code: "RO", Name: "Romania", TeamIDs: []int32{
		3044,
	}},
	{Code: "IL", Name: "Israel", TeamIDs: []int32{
		39261,
	}},
	{Code: "CO", Name: "Colombia", TeamIDs: []int32{
		13772,
	}},
	{Code: "LV", Name: "Latvia", TeamIDs: []int32{
		41587,
	}},
	{Code: "HK", Name: "Hong Kong", TeamIDs: []int32{
		41751,
	}},
}

func countryDefByCode(code string) (countryDef, bool) {
	code = strings.ToUpper(code)
	for _, def := range countryDefs {
		if def.Code == code {
			return def, true
		}
	}
	return countryDef{}, false
}

// Country is the combined production of every approved team in one country.
// Teams is capped on the collection endpoint and complete on the detail endpoint.
type Country struct {
	Code        string `json:"code"`
	Name        string `json:"name"`
	TeamsTotal  int    `json:"teams_total"`
	TeamsActive int    `json:"teams_active"`
	Teams       []Team `json:"teams"`
	Production
}

func (s *Snapshot) countryView(def countryDef, limit int) Country {
	c := Country{Code: def.Code, Name: def.Name, Teams: []Team{}}
	for _, id := range def.TeamIDs {
		slot, ok := s.State.TeamSlot(id)
		if !ok {
			continue
		}
		t := s.teamView(slot)
		c.Teams = append(c.Teams, t)
		c.TeamsTotal++
		if t.PointsLast7d > 0 {
			c.TeamsActive++
		}
		c.PointsTotal += t.PointsTotal
		c.WUsTotal += t.WUsTotal
		c.PointsLastCycle += t.PointsLastCycle
		c.PointsLast24h += t.PointsLast24h
		c.PointsLast7d += t.PointsLast7d
		c.PointsTodayUTC += t.PointsTodayUTC
		c.PointsThisWeekUTC += t.PointsThisWeekUTC
		c.PointsThisMonthUTC += t.PointsThisMonthUTC
		c.PointsPerDay24hAvg += t.PointsPerDay24hAvg
		c.PointsPerDay7dAvg += t.PointsPerDay7dAvg
	}
	sort.Slice(c.Teams, func(i, j int) bool {
		return c.Teams[i].PointsTotal > c.Teams[j].PointsTotal
	})
	c.PointsPerWU = perWU(c.PointsTotal, c.WUsTotal)
	if limit > 0 && len(c.Teams) > limit {
		c.Teams = c.Teams[:limit]
	}
	return c
}

func (s *Server) countries(snap *Snapshot, _ *http.Request) (any, *PageInfo, error) {
	out := make([]Country, 0, len(countryDefs))
	for _, def := range countryDefs {
		c := snap.countryView(def, 10)
		// An assigned but dormant country stays on its detail page, but not on the
		// interactive layer: only countries folding now receive hover targets.
		if c.TeamsActive > 0 {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].PointsPerDay24hAvg > out[j].PointsPerDay24hAvg
	})
	return out, nil, nil
}

func (s *Server) country(snap *Snapshot, r *http.Request) (any, *PageInfo, error) {
	code := strings.ToUpper(r.PathValue("code"))
	if def, ok := countryDefByCode(code); ok {
		return snap.countryView(def, 0), nil, nil
	}
	return nil, nil, notFound("no country with code %q", code)
}
