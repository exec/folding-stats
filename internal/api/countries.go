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

// Generated from the top ~3000 teams, 2026-08-15: the top 2000 by 24h PPD plus
// the lifetime leaderboard beyond it, because only 1,203 teams have any current
// output at all. Names and every displayed figure still come from the live
// snapshot; this list only says which teams a country claims. A team that cannot
// be attributed to one country is global and absent — creator communities, crypto
// projects, faith and fandom groups, and language or regional groups such as
// Alliance Francophone, IberoFolders and Folding Benelux.
var countryDefs = []countryDef{
	{Code: "US", Name: "United States", TeamIDs: []int32{
		243328, 238068, 255460, 230318, 34163, 262097, 1063859, 257579, 1115, 2630, 245196,
		242705, 230194, 41608, 247332, 36152, 226665, 11812, 253402, 267158, 12032, 80446,
		258971, 258507, 241916, 1239, 251259, 253428, 266931, 154420, 96, 883, 165492, 215762,
		258837, 241205, 270, 234080, 48083, 238444, 238889, 150682, 244353, 233664, 242687,
		134751, 138884, 31659, 233097, 60194, 95796, 249802, 258415, 35933, 171808, 682, 237210,
		258192, 254839, 34485, 257967, 237390, 238120, 234192, 249845, 166540, 243788, 242893,
		153879, 254378, 49191, 241561, 3083, 248357, 258823, 2740, 96207, 2075, 245750, 234456,
		12233, 2116, 251951, 749, 51431, 229192, 240498, 249513, 173573, 238779, 257885, 254273,
		1305, 241525, 237854, 38296, 41235, 264662, 243025, 261194, 245613, 256630, 58812,
		245804, 1064857, 241960, 1065005, 46301, 256579, 159533, 139472, 244109, 47816, 231032,
		252373, 251447, 252534, 258145, 12477, 225826, 1065661, 1064178, 1064163, 1064804,
		1066375, 229657, 239702, 268686, 222470, 1809, 237714, 255950, 51524, 232820, 10213,
		235547, 246058, 267870, 267181, 229346, 257105, 1065796, 245607, 1060772, 55309, 252647,
		214786, 244326, 248794, 251060, 2032, 160705, 10175, 239639, 256429, 252716, 249321,
		172248, 242558, 31673, 238420, 2334, 46680, 238422, 134469, 249282, 199966, 257290, 3148,
		199679, 42753, 33308, 240012, 233087, 3361, 68421, 243097, 10636, 262795, 254697, 135620,
		251893, 263785, 251230, 251103, 249165, 1061602, 1063760, 266245, 165451, 92743, 240460,
		237851, 251380, 236952, 1065022, 257668, 249297, 266264, 266047, 264466, 253560, 242813,
		61483, 61404, 10282, 13481, 254320, 59002, 39189, 241312, 47445, 33446, 249677, 233316,
		243316, 250214, 266968, 245144, 260288, 230692, 261187, 236581, 33403, 12099, 235809,
		47180, 1064155, 245724, 2247, 577, 253433, 240563, 46258, 2901, 257542, 244063, 254701,
		238415, 257190, 1065901, 255884, 259095, 222478, 52533, 33157, 113685, 264179, 259931,
		259820, 251367, 33016, 11675, 258646, 237266, 201236, 264024, 164322, 229123, 226912,
		266155, 262069, 237108, 32197, 245846, 249672, 265364, 111148, 245762, 229997, 40052,
		98844, 77630, 70351, 80633, 231015, 264050, 264474, 252389, 38444, 257946, 2497, 152875,
		237700, 226949, 123688, 253678, 258530, 243020, 249162, 112645, 13007, 267482, 242923,
		245881, 168924, 264250, 248275, 241390, 245745, 254064, 250794, 2637, 266751, 267305,
		264561, 260156, 32324, 227811, 240402, 47148, 207709, 60893, 47846, 234766, 159321, 464,
		88141, 241283, 227775, 13088, 226483, 258120, 268066, 1062394, 254479, 109561, 265920,
		248399, 189346, 225311, 1065087, 259844, 1572, 48157, 35554, 1062578, 249630, 1066032,
		250265, 1065152, 1063948, 1066405, 56606, 1062811, 1, 1003, 10033, 10276, 1061333,
		1062094, 1062270, 1063131, 1063984, 1064729, 1064918, 1065368, 1066016, 1066028, 1066076,
		1066120, 1066792, 1066882, 1067245, 1067634, 1068131, 10971, 111065, 11108, 11142, 112,
		11232, 11314, 11534, 116608, 11681, 12072, 1287, 13051, 13180, 134653, 13515, 13622,
		13761, 14, 148892, 15, 163348, 163473, 168145, 1701, 171061, 1739, 180172, 1805, 198,
		1991, 204860, 209894, 213698, 2233, 224746, 227286, 227455, 227867, 229258, 231250,
		231300, 232084, 234523, 234771, 235, 235955, 236284, 236370, 236584, 236709, 236760,
		237882, 239945, 240045, 240755, 243108, 243112, 243210, 243214, 243253, 243801, 245058,
		245755, 245860, 246129, 247019, 247217, 248140, 249559, 249560, 249708, 250186, 250299,
		251626, 252071, 252180, 253811, 256378, 256729, 257104, 257196, 257944, 257991, 259414,
		259665, 259918, 260722, 261829, 261972, 262556, 263476, 264460, 2653, 2654, 267635, 3007,
		3074, 3103, 31658, 32, 32035, 3212, 32407, 33, 33147, 33258, 3357, 33577, 34149, 3430,
		3446, 34479, 34687, 35054, 35126, 36120, 37726, 37742, 38228, 38928, 39227, 39797, 4,
		40051, 40098, 40211, 40247, 40965, 41167, 42028, 43573, 44352, 446, 44851, 45435, 50511,
		50906, 50945, 50959, 50979, 52552, 52737, 54238, 55236, 55260, 57391, 59, 59515, 604,
		64534, 68208, 69710, 711, 716, 717, 734, 74413, 77397, 86926, 88682, 93, 999,
	}},
	{Code: "DE", Name: "Germany", TeamIDs: []int32{
		258351, 127907, 155780, 249367, 94347, 251386, 263603, 10817, 250506, 250435, 246646,
		239216, 244062, 247690, 43778, 250565, 238098, 34099, 264390, 252827, 242585, 198185,
		260111, 245623, 253914, 265730, 1064458, 255225, 176487, 1062814, 12841, 244822, 243660,
		259021, 1064575, 254360, 2804, 240007, 254100, 2984, 248705, 225372, 240046, 150621,
		229160, 241425, 246373, 2682, 245789, 245540, 243421, 250788, 258441, 253308, 250155,
		253662, 262535, 265042, 240966, 258126, 66332, 238966, 250985, 252772, 1066030, 244687,
		252637, 255716, 257057, 237775, 262941, 250888, 165208, 1065107, 10022, 1065430, 1067091,
		1067373, 1068028, 226201, 229957, 229984, 235850, 235935, 239199, 240251, 240286, 240890,
		241412, 242601, 243047, 243871, 246632, 250660, 251999, 252034, 253305, 256656, 257831,
		263429, 264169, 264821, 265346, 265898, 266722, 267356, 3, 34361, 43829, 45456, 45700,
		45876, 53140, 70335, 70408, 70911,
	}},
	{Code: "NO", Name: "Norway", TeamIDs: []int32{
		241978, 1064929, 254030, 237803, 238979, 249160, 268660, 220963, 1067329, 244865, 234175,
		255822, 242236, 61994, 240255, 1065660, 183891, 237481, 237638, 240650, 243541, 264955,
		37651, 37827,
	}},
	{Code: "RU", Name: "Russia", TeamIDs: []int32{
		247472, 228792, 107505, 133034, 122296, 279, 47191,
	}},
	{Code: "GB", Name: "United Kingdom", TeamIDs: []int32{
		240216, 265163, 10, 240995, 252057, 259515, 245680, 242863, 246309, 248222, 48192,
		196420, 248010, 245392, 1065740, 240845, 247471, 45392, 241263, 236472, 249510, 263598,
		229167, 458, 263270, 232267, 69776, 242491, 1066289, 258448, 78042, 257982, 1061841,
		246098, 264018, 238479, 243191, 2412, 246692, 258310, 239203, 257848, 258327, 163049,
		1064077, 1068232, 1068331, 234111, 242995, 243272, 244604, 244613, 245403, 250966,
		251803, 252914, 258322, 263289, 35947, 37363, 46590, 54234, 588, 61671, 69263, 98860,
	}},
	{Code: "CN", Name: "China", TeamIDs: []int32{
		253541, 3213,
	}},
	{Code: "AU", Name: "Australia", TeamIDs: []int32{
		253919, 252720, 267219, 36010, 237022, 44170, 248479, 268126, 265716, 240641, 244539,
		186, 225000, 24, 243288, 246095, 266266, 267244, 267810, 43781, 44171, 46401, 47155,
	}},
	{Code: "TW", Name: "Taiwan", TeamIDs: []int32{
		244178, 37766, 651, 46810, 31403,
	}},
	{Code: "BY", Name: "Belarus", TeamIDs: []int32{
		11897, 773,
	}},
	{Code: "CA", Name: "Canada", TeamIDs: []int32{
		245993, 250396, 268694, 1068010, 235793, 248458, 231656, 259377, 230690, 1065300, 258921,
		47936, 86, 253168, 246505, 247736, 256609, 190589, 39286, 167613, 121684, 1063963,
		249765, 260347, 230045, 1063901, 264900, 13362, 115804, 244070, 257771, 235872, 47687,
		236046, 1061741, 13496, 34384, 264085, 127980, 1064775, 1068265, 10733, 10987, 253800,
		256648, 31574, 34242, 37412, 54196, 60,
	}},
	{Code: "SE", Name: "Sweden", TeamIDs: []int32{
		1061755, 252390, 242624, 256133, 256869, 241128, 42956, 241438, 246962, 239904, 255108,
		239210, 257338, 12044, 246538, 233257, 267236, 253993, 256923, 253937, 127292, 238606,
		254957, 251757, 1068136, 239347, 245476, 247646, 260500, 37451, 74600, 831,
	}},
	{Code: "NZ", Name: "New Zealand", TeamIDs: []int32{
		11053, 246916, 32887,
	}},
	{Code: "NL", Name: "Netherlands", TeamIDs: []int32{
		247522, 236111, 242482, 223282, 240144, 1067515, 1288, 13505, 48658, 69411, 92,
	}},
	{Code: "JP", Name: "Japan", TeamIDs: []int32{
		261481, 260900, 253284, 259660, 60630, 264997, 256823, 106346, 259877, 55486, 39204,
		254224, 264491, 260045, 257002, 256822, 242102, 263798, 256025, 254156, 254001, 236911,
		265895, 256955, 256103, 261487, 255463, 266586, 247317, 259015, 261543, 263995, 1064945,
		1067225, 162, 179680, 222, 253230, 254140, 254402, 255396, 257261, 257728, 258804,
		261092, 264379,
	}},
	{Code: "FR", Name: "France", TeamIDs: []int32{
		229738, 44798, 261556, 263483, 243764, 420, 263700, 244682, 1188, 53653, 245642, 254077,
		246220, 240342, 1065443, 10317, 200905, 236445, 242460, 247377, 251916, 45363,
	}},
	{Code: "UA", Name: "Ukraine", TeamIDs: []int32{
		153624, 156571, 2164,
	}},
	{Code: "VN", Name: "Vietnam", TeamIDs: []int32{
		240761, 38156,
	}},
	{Code: "PL", Name: "Poland", TeamIDs: []int32{
		254123, 250728, 1064915, 236420, 1066850, 276, 77920,
	}},
	{Code: "BR", Name: "Brazil", TeamIDs: []int32{
		247463, 210814, 229296, 1062169, 13802, 148894, 231006, 266910, 53066,
	}},
	{Code: "FI", Name: "Finland", TeamIDs: []int32{
		237157, 257637, 240198, 242321, 235445, 245556, 365, 56662, 165614, 249389, 237394,
		78355, 242221, 265624, 231988, 239618, 250694, 254869, 38850, 454, 62,
	}},
	{Code: "HU", Name: "Hungary", TeamIDs: []int32{
		240228, 264726, 1061303, 239968, 43299, 679,
	}},
	{Code: "GR", Name: "Greece", TeamIDs: []int32{
		1065818, 1068194, 13416, 3322, 36673, 44079, 838,
	}},
	{Code: "CZ", Name: "Czechia", TeamIDs: []int32{
		252064, 249477, 237170, 245800, 241054, 246927, 263313, 241584, 261291, 114, 10643,
		239186, 240945, 1067491, 157986, 235910, 236157, 236542, 236798, 240727, 246110, 49658,
	}},
	{Code: "IT", Name: "Italy", TeamIDs: []int32{
		1065, 246223, 262479, 249208, 46729, 1344, 258830, 263392, 69630, 180924, 246330, 252841,
		256411, 61171,
	}},
	{Code: "MY", Name: "Malaysia", TeamIDs: []int32{
		192653, 10531, 2999,
	}},
	{Code: "ID", Name: "Indonesia", TeamIDs: []int32{
		1068346, 38608,
	}},
	{Code: "CH", Name: "Switzerland", TeamIDs: []int32{
		248998, 65440, 3129, 42, 231211, 158203, 249164, 1067671, 264704, 256671, 264890, 2866,
		38188,
	}},
	{Code: "TR", Name: "Turkey", TeamIDs: []int32{
		175051, 39655,
	}},
	{Code: "IE", Name: "Ireland", TeamIDs: []int32{
		39432, 242964, 246106, 266057, 257898, 60443,
	}},
	{Code: "MV", Name: "Maldives", TeamIDs: []int32{
		32777,
	}},
	{Code: "DK", Name: "Denmark", TeamIDs: []int32{
		234221, 34037, 250770, 240840, 1675, 259376, 237006, 34688, 249218, 33012, 1068101, 244,
		245856, 262701, 69476,
	}},
	{Code: "PT", Name: "Portugal", TeamIDs: []int32{
		260601, 257559, 1068128, 35271, 498,
	}},
	{Code: "BH", Name: "Bahrain", TeamIDs: []int32{
		34353,
	}},
	{Code: "BN", Name: "Brunei", TeamIDs: []int32{
		135369,
	}},
	{Code: "EE", Name: "Estonia", TeamIDs: []int32{
		238919, 385,
	}},
	{Code: "BG", Name: "Bulgaria", TeamIDs: []int32{
		32435, 32276, 845,
	}},
	{Code: "BE", Name: "Belgium", TeamIDs: []int32{
		37249, 243566, 34517, 233874, 33528, 243169, 243492, 240743, 239672, 3455, 37639,
	}},
	{Code: "LT", Name: "Lithuania", TeamIDs: []int32{
		3558, 36816,
	}},
	{Code: "HR", Name: "Croatia", TeamIDs: []int32{
		237668, 829, 137516,
	}},
	{Code: "SG", Name: "Singapore", TeamIDs: []int32{
		268453, 197,
	}},
	{Code: "AT", Name: "Austria", TeamIDs: []int32{
		245443, 264767, 11246, 253113, 53791, 1061406, 259260, 49324, 239053, 251248, 1604,
		263034,
	}},
	{Code: "PH", Name: "Philippines", TeamIDs: []int32{
		46447, 2291,
	}},
	{Code: "SI", Name: "Slovenia", TeamIDs: []int32{
		256107, 10824, 299,
	}},
	{Code: "KR", Name: "South Korea", TeamIDs: []int32{
		261341, 149281,
	}},
	{Code: "ES", Name: "Spain", TeamIDs: []int32{
		259280, 230209, 257094, 235246, 257515, 1250, 149424,
	}},
	{Code: "ZA", Name: "South Africa", TeamIDs: []int32{
		37857, 1065774, 234483,
	}},
	{Code: "TH", Name: "Thailand", TeamIDs: []int32{
		248500, 265239, 1064025, 52128,
	}},
	{Code: "DZ", Name: "Algeria", TeamIDs: []int32{
		1067419,
	}},
	{Code: "RO", Name: "Romania", TeamIDs: []int32{
		75559, 240374, 3044,
	}},
	{Code: "IL", Name: "Israel", TeamIDs: []int32{
		2792, 235813, 39261,
	}},
	{Code: "SK", Name: "Slovakia", TeamIDs: []int32{
		124, 124205, 47295,
	}},
	{Code: "CO", Name: "Colombia", TeamIDs: []int32{
		13772,
	}},
	{Code: "HK", Name: "Hong Kong", TeamIDs: []int32{
		248547, 231170, 10576, 32535, 41751,
	}},
	{Code: "LV", Name: "Latvia", TeamIDs: []int32{
		41587,
	}},
	{Code: "AR", Name: "Argentina", TeamIDs: []int32{
		1747,
	}},
	{Code: "IS", Name: "Iceland", TeamIDs: []int32{
		184739, 237478, 1062171,
	}},
	{Code: "PR", Name: "Puerto Rico", TeamIDs: []int32{
		36206,
	}},
	{Code: "IN", Name: "India", TeamIDs: []int32{
		525,
	}},
	{Code: "MM", Name: "Myanmar", TeamIDs: []int32{
		250150,
	}},
	{Code: "LB", Name: "Lebanon", TeamIDs: []int32{
		49137,
	}},
	{Code: "AE", Name: "United Arab Emirates", TeamIDs: []int32{
		217114,
	}},
	{Code: "MX", Name: "Mexico", TeamIDs: []int32{
		1108,
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
		// Dormant countries are returned too. They used to be filtered out here, which
		// made them unreachable the moment the map offered a lifetime-points view: a
		// country whose teams stopped folding still has a history and a roster, and
		// somebody looking for a team to join needs to see it. What counts as
		// "highlighted" is a question about the metric on screen, so the client decides.
		out = append(out, snap.countryView(def, 10))
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
