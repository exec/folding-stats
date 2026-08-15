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

// Generated 2026-08-15 from three sweeps: the top 2000 teams by 24h PPD, the
// lifetime leaderboard beyond it (only 1,203 teams have any current output), and
// a search for every country name that still had no team. Names and every
// displayed figure still come from the live snapshot; this list only says which
// teams a country claims. A team that cannot be attributed to one country is
// global and absent — creator communities, crypto projects, faith and fandom
// groups, and language or regional groups such as Alliance Francophone.
var countryDefs = []countryDef{
	{Code: "US", Name: "United States", TeamIDs: []int32{
		111065, 32, 259918, 33, 198, 11108, 37726, 40051, 234771, 1, 246129, 257944, 711, 44851,
		52737, 243328, 57391, 238068, 14, 50959, 86926, 999, 1066076, 231300, 239945, 4, 3074,
		255460, 11314, 1063131, 15, 40098, 2233, 40965, 10033, 230318, 3446, 34163, 734, 35054,
		32035, 43573, 93, 262097, 1739, 38928, 36120, 1063859, 39227, 32407, 257579, 232084,
		1115, 2630, 33258, 245196, 13180, 446, 40211, 242705, 55236, 230194, 1287, 77397, 11681,
		41608, 247332, 33147, 45435, 256378, 245755, 227286, 36152, 226665, 59515, 88682, 11812,
		11142, 1064729, 253402, 267158, 12032, 80446, 258971, 229258, 258507, 262556, 237882,
		1062270, 241916, 243108, 261972, 260722, 717, 243112, 227867, 249708, 1239, 10276,
		251259, 253428, 249560, 266931, 38228, 2654, 250299, 236709, 154420, 96, 883, 236370,
		165492, 13515, 215762, 258837, 264460, 1991, 241205, 270, 234080, 48083, 3007, 238444,
		12072, 227455, 163348, 238889, 243214, 150682, 243801, 244353, 233664, 242687, 134751,
		138884, 236760, 31659, 233097, 60194, 95796, 50906, 249802, 258415, 35933, 171808, 69710,
		682, 148892, 1805, 237210, 13051, 247019, 59, 258192, 254839, 34485, 1064918, 1068131,
		247217, 35126, 257967, 237390, 238120, 234192, 249845, 166540, 243788, 3357, 13761,
		242893, 245058, 3103, 1003, 153879, 234523, 254378, 33577, 49191, 241561, 3083, 248357,
		258823, 2740, 52552, 96207, 11534, 2075, 245750, 234456, 64534, 12233, 2116, 251951, 749,
		51431, 229192, 240498, 249513, 44352, 173573, 54238, 238779, 257885, 254273, 261829,
		1305, 1701, 241525, 237854, 10971, 38296, 41235, 264662, 243025, 261194, 40247, 245613,
		256630, 58812, 34479, 41167, 245804, 1064857, 31658, 50979, 241960, 1065005, 46301,
		256579, 159533, 139472, 243210, 244109, 47816, 231032, 252373, 251447, 252534, 258145,
		12477, 225826, 1065661, 1064178, 1064163, 1064804, 1066375, 229657, 134653, 239702,
		268686, 222470, 1809, 237714, 50511, 255950, 51524, 232820, 10213, 235547, 34149, 246058,
		267870, 267181, 716, 236584, 229346, 257105, 1065796, 604, 245607, 1060772, 55309,
		252647, 214786, 244326, 248794, 251060, 2032, 160705, 235, 10175, 239639, 256429, 252716,
		249321, 172248, 242558, 37742, 31673, 238420, 2334, 46680, 238422, 134469, 249282,
		199966, 257290, 236284, 3148, 199679, 55260, 74413, 42753, 257991, 33308, 240012, 233087,
		68208, 3361, 68421, 243097, 10636, 235955, 262795, 254697, 135620, 251893, 263785,
		251230, 251103, 249165, 1061602, 224746, 1063760, 266245, 165451, 92743, 240460, 237851,
		251380, 236952, 1065022, 171061, 257668, 249297, 267635, 249559, 266264, 266047, 264466,
		253560, 242813, 1067634, 61483, 61404, 10282, 13481, 254320, 59002, 39189, 241312, 47445,
		33446, 249677, 233316, 243316, 250214, 266968, 245144, 260288, 230692, 261187, 236581,
		33403, 12099, 235809, 47180, 1064155, 245724, 2247, 2653, 577, 253433, 240563, 46258,
		2901, 257542, 244063, 50945, 254701, 238415, 257190, 1065901, 255884, 259095, 222478,
		52533, 33157, 113685, 264179, 259931, 259820, 251367, 33016, 11675, 258646, 237266,
		201236, 264024, 164322, 163473, 229123, 226912, 266155, 262069, 237108, 257104, 1065368,
		32197, 245846, 249672, 251626, 265364, 111148, 245762, 229997, 40052, 98844, 77630,
		70351, 80633, 231015, 264050, 264474, 252389, 38444, 257946, 3430, 2497, 152875, 237700,
		226949, 123688, 253678, 258530, 243020, 249162, 112645, 13007, 267482, 242923, 245881,
		168924, 264250, 248275, 241390, 245745, 254064, 250794, 2637, 266751, 267305, 42028,
		264561, 260156, 1066882, 32324, 227811, 240402, 47148, 207709, 60893, 47846, 234766,
		159321, 180172, 464, 11232, 88141, 241283, 227775, 13088, 226483, 258120, 268066,
		1062394, 254479, 109561, 265920, 248399, 248140, 189346, 225311, 1065087, 213698, 256729,
		13622, 259844, 257196, 259665, 1066028, 1572, 209894, 48157, 35554, 252071, 39797,
		231250, 1061333, 112, 168145, 1062578, 249630, 243253, 259414, 3212, 116608, 245860,
		250186, 204860, 1066016, 1066032, 253811, 1066120, 252180, 240755, 263476, 1066792,
		250265, 1063984, 1067245, 1065152, 34687, 1063948, 1062094, 1066405, 240045, 56606,
		1062811,
	}},
	{Code: "DE", Name: "Germany", TeamIDs: []int32{
		70335, 240890, 251999, 70911, 229957, 258351, 235935, 3, 127907, 34361, 155780, 249367,
		263429, 94347, 256656, 239199, 235850, 243047, 251386, 263603, 10817, 250506, 250435,
		43829, 246646, 239216, 240251, 240286, 244062, 247690, 43778, 250565, 242601, 238098,
		34099, 264390, 267356, 252827, 242585, 198185, 260111, 245623, 253914, 45876, 45456,
		265730, 1064458, 255225, 176487, 1062814, 12841, 244822, 243660, 259021, 1064575, 254360,
		2804, 226201, 240007, 254100, 2984, 53140, 248705, 225372, 240046, 150621, 229160,
		241425, 265898, 252034, 246632, 246373, 257831, 2682, 245789, 245540, 243421, 250660,
		250788, 258441, 253308, 250155, 253662, 262535, 265042, 240966, 253305, 10022, 258126,
		1068028, 66332, 238966, 250985, 45700, 252772, 1066030, 244687, 252637, 255716, 243871,
		257057, 266722, 70408, 264821, 264169, 241412, 237775, 262941, 229984, 250888, 1067373,
		1067091, 265346, 1065430, 165208, 1065107,
	}},
	{Code: "NO", Name: "Norway", TeamIDs: []int32{
		37651, 37827, 264955, 240650, 237481, 241978, 237638, 243541, 1064929, 254030, 183891,
		237803, 238979, 249160, 268660, 220963, 1067329, 244865, 234175, 255822, 242236, 61994,
		240255, 1065660,
	}},
	{Code: "RU", Name: "Russia", TeamIDs: []int32{
		47191, 279, 122296, 247472, 228792, 107505, 133034,
	}},
	{Code: "GB", Name: "United Kingdom", TeamIDs: []int32{
		35947, 98860, 240216, 265163, 244613, 250966, 10, 240995, 46590, 252057, 259515, 252914,
		245680, 242863, 246309, 248222, 69263, 48192, 196420, 244604, 248010, 245392, 588,
		1065740, 240845, 247471, 45392, 241263, 236472, 249510, 263598, 229167, 458, 263270,
		232267, 69776, 242491, 1066289, 258448, 258322, 78042, 257982, 1061841, 246098, 264018,
		238479, 234111, 243191, 2412, 246692, 258310, 245403, 239203, 251803, 257848, 258327,
		163049, 61671, 37363, 242995, 54234, 1068232, 263289, 1064077, 243272, 1068331,
	}},
	{Code: "CN", Name: "China", TeamIDs: []int32{
		3213, 253541,
	}},
	{Code: "AU", Name: "Australia", TeamIDs: []int32{
		24, 43781, 253919, 186, 47155, 252720, 267219, 36010, 267810, 237022, 44171, 44170,
		243288, 248479, 225000, 268126, 265716, 267244, 240641, 246095, 244539, 46401, 266266,
	}},
	{Code: "TW", Name: "Taiwan", TeamIDs: []int32{
		31403, 244178, 37766, 651, 46810,
	}},
	{Code: "BY", Name: "Belarus", TeamIDs: []int32{
		11897, 773,
	}},
	{Code: "CA", Name: "Canada", TeamIDs: []int32{
		245993, 250396, 10987, 54196, 10733, 268694, 31574, 60, 1068010, 235793, 248458, 231656,
		37412, 259377, 230690, 256648, 1065300, 34242, 258921, 47936, 86, 253168, 246505, 247736,
		256609, 190589, 39286, 167613, 121684, 1063963, 249765, 260347, 230045, 1063901, 264900,
		13362, 115804, 244070, 257771, 235872, 47687, 236046, 253800, 1061741, 13496, 1064775,
		1068265, 34384, 264085, 127980,
	}},
	{Code: "SE", Name: "Sweden", TeamIDs: []int32{
		37451, 74600, 245476, 1061755, 831, 252390, 242624, 256133, 256869, 241128, 42956,
		241438, 246962, 247646, 239904, 255108, 239210, 257338, 12044, 246538, 233257, 267236,
		253993, 256923, 253937, 127292, 238606, 254957, 251757, 239347, 1068136, 260500,
	}},
	{Code: "NZ", Name: "New Zealand", TeamIDs: []int32{
		32887, 246916, 11053,
	}},
	{Code: "NL", Name: "Netherlands", TeamIDs: []int32{
		92, 69411, 247522, 236111, 242482, 48658, 1288, 13505, 223282, 240144, 1067515,
	}},
	{Code: "JP", Name: "Japan", TeamIDs: []int32{
		162, 261481, 257728, 254402, 257261, 260900, 222, 255396, 253284, 253230, 259660, 60630,
		264997, 258804, 256823, 106346, 259877, 55486, 261092, 39204, 254224, 264491, 254140,
		260045, 257002, 256822, 242102, 263798, 256025, 254156, 254001, 236911, 265895, 256955,
		256103, 1067225, 261487, 255463, 266586, 247317, 259015, 261543, 264379, 179680, 263995,
		1064945,
	}},
	{Code: "FR", Name: "France", TeamIDs: []int32{
		45363, 242460, 247377, 229738, 44798, 261556, 263483, 243764, 420, 200905, 251916, 10317,
		263700, 244682, 1188, 236445, 53653, 245642, 254077, 246220, 240342, 1065443,
	}},
	{Code: "UA", Name: "Ukraine", TeamIDs: []int32{
		2164, 156571, 153624,
	}},
	{Code: "VN", Name: "Vietnam", TeamIDs: []int32{
		38156, 240761,
	}},
	{Code: "PL", Name: "Poland", TeamIDs: []int32{
		276, 254123, 250728, 1064915, 236420, 1066850, 77920,
	}},
	{Code: "BR", Name: "Brazil", TeamIDs: []int32{
		247463, 148894, 13802, 53066, 1062169, 266910, 210814, 229296, 231006,
	}},
	{Code: "FI", Name: "Finland", TeamIDs: []int32{
		231988, 62, 454, 237157, 239618, 38850, 257637, 240198, 242321, 235445, 245556, 365,
		56662, 165614, 249389, 254869, 237394, 78355, 242221, 265624, 250694,
	}},
	{Code: "HU", Name: "Hungary", TeamIDs: []int32{
		240228, 239968, 43299, 679, 264726, 1061303,
	}},
	{Code: "GR", Name: "Greece", TeamIDs: []int32{
		44079, 36673, 838, 13416, 1065818, 3322, 1068194,
	}},
	{Code: "CZ", Name: "Czechia", TeamIDs: []int32{
		49658, 252064, 236798, 249477, 236157, 237170, 245800, 235910, 246110, 241054, 246927,
		263313, 241584, 261291, 114, 10643, 239186, 240945, 157986, 240727, 1067491, 236542,
	}},
	{Code: "IT", Name: "Italy", TeamIDs: []int32{
		246330, 61171, 1065, 246223, 262479, 249208, 252841, 46729, 1344, 258830, 180924, 256411,
		263392, 69630,
	}},
	{Code: "MY", Name: "Malaysia", TeamIDs: []int32{
		2999, 192653, 10531,
	}},
	{Code: "ID", Name: "Indonesia", TeamIDs: []int32{
		38608, 1068346,
	}},
	{Code: "CH", Name: "Switzerland", TeamIDs: []int32{
		248998, 38188, 65440, 3129, 42, 231211, 158203, 2866, 249164, 1067671, 264704, 264890,
		256671,
	}},
	{Code: "TR", Name: "Turkey", TeamIDs: []int32{
		39655, 175051,
	}},
	{Code: "IE", Name: "Ireland", TeamIDs: []int32{
		60443, 39432, 242964, 257898, 246106, 266057,
	}},
	{Code: "MV", Name: "Maldives", TeamIDs: []int32{
		32777,
	}},
	{Code: "DK", Name: "Denmark", TeamIDs: []int32{
		262701, 244, 234221, 1068101, 34037, 250770, 240840, 1675, 259376, 237006, 34688, 249218,
		33012, 69476, 245856,
	}},
	{Code: "PT", Name: "Portugal", TeamIDs: []int32{
		35271, 260601, 257559, 498, 1068128,
	}},
	{Code: "BH", Name: "Bahrain", TeamIDs: []int32{
		34353,
	}},
	{Code: "BN", Name: "Brunei", TeamIDs: []int32{
		135369,
	}},
	{Code: "EE", Name: "Estonia", TeamIDs: []int32{
		385, 238919,
	}},
	{Code: "BG", Name: "Bulgaria", TeamIDs: []int32{
		32435, 845, 32276,
	}},
	{Code: "BE", Name: "Belgium", TeamIDs: []int32{
		3455, 37249, 239672, 243566, 34517, 233874, 33528, 243169, 243492, 240743, 37639,
	}},
	{Code: "LT", Name: "Lithuania", TeamIDs: []int32{
		36816, 3558,
	}},
	{Code: "HR", Name: "Croatia", TeamIDs: []int32{
		137516, 237668, 829,
	}},
	{Code: "SG", Name: "Singapore", TeamIDs: []int32{
		197, 268453,
	}},
	{Code: "AT", Name: "Austria", TeamIDs: []int32{
		1604, 245443, 264767, 11246, 253113, 53791, 1061406, 259260, 49324, 239053, 251248,
		263034,
	}},
	{Code: "PH", Name: "Philippines", TeamIDs: []int32{
		2291, 46447,
	}},
	{Code: "SI", Name: "Slovenia", TeamIDs: []int32{
		256107, 10824, 299,
	}},
	{Code: "KR", Name: "South Korea", TeamIDs: []int32{
		149281, 261341,
	}},
	{Code: "ES", Name: "Spain", TeamIDs: []int32{
		259280, 1250, 230209, 257094, 235246, 149424, 257515,
	}},
	{Code: "ZA", Name: "South Africa", TeamIDs: []int32{
		37857, 234483, 1065774,
	}},
	{Code: "TH", Name: "Thailand", TeamIDs: []int32{
		248500, 265239, 52128, 1064025,
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
	{Code: "HK", Name: "Hong Kong", TeamIDs: []int32{
		41751, 248547, 231170, 10576, 32535,
	}},
	{Code: "LV", Name: "Latvia", TeamIDs: []int32{
		41587,
	}},
	{Code: "CO", Name: "Colombia", TeamIDs: []int32{
		13772,
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
	{Code: "RS", Name: "Serbia", TeamIDs: []int32{
		387, 239922, 513, 1064229, 155427, 470,
	}},
	{Code: "LU", Name: "Luxembourg", TeamIDs: []int32{
		11170, 110242,
	}},
	{Code: "KW", Name: "Kuwait", TeamIDs: []int32{
		53527, 33618, 158711, 120003, 100869, 121839,
	}},
	{Code: "PK", Name: "Pakistan", TeamIDs: []int32{
		256988, 266412, 32084, 13627,
	}},
	{Code: "PY", Name: "Paraguay", TeamIDs: []int32{
		76602, 197618, 139479, 148333, 83471,
	}},
	{Code: "LY", Name: "Libya", TeamIDs: []int32{
		1067580, 1063519, 267747,
	}},
	{Code: "MD", Name: "Moldova", TeamIDs: []int32{
		48152,
	}},
	{Code: "GL", Name: "Greenland", TeamIDs: []int32{
		323,
	}},
	{Code: "EG", Name: "Egypt", TeamIDs: []int32{
		46644, 241239, 104901, 241588, 207000, 137401,
	}},
	{Code: "NC", Name: "New Caledonia", TeamIDs: []int32{
		253877, 1060690,
	}},
	{Code: "CR", Name: "Costa Rica", TeamIDs: []int32{
		43522, 208154,
	}},
	{Code: "EC", Name: "Ecuador", TeamIDs: []int32{
		230268, 58039, 832, 121351,
	}},
	{Code: "PS", Name: "Palestine", TeamIDs: []int32{
		39163,
	}},
	{Code: "VE", Name: "Venezuela", TeamIDs: []int32{
		216, 232045,
	}},
	{Code: "IR", Name: "Iran", TeamIDs: []int32{
		34657, 232208, 261524, 34507, 92117,
	}},
	{Code: "UY", Name: "Uruguay", TeamIDs: []int32{
		34269,
	}},
	{Code: "PE", Name: "Peru", TeamIDs: []int32{
		248392, 636, 246336, 223458,
	}},
	{Code: "KZ", Name: "Kazakhstan", TeamIDs: []int32{
		163012, 12525,
	}},
	{Code: "NG", Name: "Nigeria", TeamIDs: []int32{
		74346, 265842, 221372,
	}},
	{Code: "SA", Name: "Saudi Arabia", TeamIDs: []int32{
		243130, 93230, 116530, 203335, 35863, 157008,
	}},
	{Code: "UZ", Name: "Uzbekistan", TeamIDs: []int32{
		223185,
	}},
	{Code: "CL", Name: "Chile", TeamIDs: []int32{
		36667, 261345, 243843, 33544, 50172, 68764,
	}},
	{Code: "BO", Name: "Bolivia", TeamIDs: []int32{
		245683, 53475, 265010,
	}},
	{Code: "BD", Name: "Bangladesh", TeamIDs: []int32{
		250236, 248672,
	}},
	{Code: "KG", Name: "Kyrgyzstan", TeamIDs: []int32{
		149251,
	}},
	{Code: "LK", Name: "Sri Lanka", TeamIDs: []int32{
		225273, 40470,
	}},
	{Code: "CY", Name: "Cyprus", TeamIDs: []int32{
		245626, 46731, 232325, 156306,
	}},
	{Code: "AZ", Name: "Azerbaijan", TeamIDs: []int32{
		1067891, 226896, 106957,
	}},
	{Code: "NP", Name: "Nepal", TeamIDs: []int32{
		433, 240513,
	}},
	{Code: "IQ", Name: "Iraq", TeamIDs: []int32{
		204809, 169027,
	}},
	{Code: "PA", Name: "Panama", TeamIDs: []int32{
		62671, 236285,
	}},
	{Code: "BA", Name: "Bosnia and Herzegovina", TeamIDs: []int32{
		228194, 106812, 148558, 11986,
	}},
	{Code: "TT", Name: "Trinidad and Tobago", TeamIDs: []int32{
		1064053, 58993,
	}},
	{Code: "HN", Name: "Honduras", TeamIDs: []int32{
		132700,
	}},
	{Code: "HT", Name: "Haiti", TeamIDs: []int32{
		95087,
	}},
	{Code: "GY", Name: "Guyana", TeamIDs: []int32{
		40839,
	}},
	{Code: "MK", Name: "North Macedonia", TeamIDs: []int32{
		87689, 255844,
	}},
	{Code: "AM", Name: "Armenia", TeamIDs: []int32{
		237425, 82735,
	}},
	{Code: "GH", Name: "Ghana", TeamIDs: []int32{
		262142,
	}},
	{Code: "AO", Name: "Angola", TeamIDs: []int32{
		223899, 68404, 165676,
	}},
	{Code: "KE", Name: "Kenya", TeamIDs: []int32{
		246712, 228464, 191190,
	}},
	{Code: "DO", Name: "Dominican Republic", TeamIDs: []int32{
		1065406, 10771, 74891, 104944, 143148, 51356,
	}},
	{Code: "SR", Name: "Suriname", TeamIDs: []int32{
		125789,
	}},
	{Code: "TN", Name: "Tunisia", TeamIDs: []int32{
		41538, 247134,
	}},
	{Code: "ER", Name: "Eritrea", TeamIDs: []int32{
		62243,
	}},
	{Code: "AL", Name: "Albania", TeamIDs: []int32{
		1061582, 191034,
	}},
	{Code: "GT", Name: "Guatemala", TeamIDs: []int32{
		69808, 208877, 10248,
	}},
	{Code: "SY", Name: "Syria", TeamIDs: []int32{
		89349, 46402,
	}},
	{Code: "MA", Name: "Morocco", TeamIDs: []int32{
		59109, 258859,
	}},
	{Code: "FJ", Name: "Fiji", TeamIDs: []int32{
		164437, 105620,
	}},
	{Code: "JM", Name: "Jamaica", TeamIDs: []int32{
		144464,
	}},
	{Code: "QA", Name: "Qatar", TeamIDs: []int32{
		56145, 222363, 93600, 151123,
	}},
	{Code: "SV", Name: "El Salvador", TeamIDs: []int32{
		124334, 59459,
	}},
	{Code: "SD", Name: "Sudan", TeamIDs: []int32{
		104833,
	}},
	{Code: "MW", Name: "Malawi", TeamIDs: []int32{
		87267,
	}},
	{Code: "MN", Name: "Mongolia", TeamIDs: []int32{
		48297,
	}},
	{Code: "CI", Name: "Ivory Coast", TeamIDs: []int32{
		152359,
	}},
	{Code: "MZ", Name: "Mozambique", TeamIDs: []int32{
		170881, 127099,
	}},
	{Code: "NI", Name: "Nicaragua", TeamIDs: []int32{
		187871,
	}},
	{Code: "BS", Name: "Bahamas", TeamIDs: []int32{
		57044,
	}},
	{Code: "ET", Name: "Ethiopia", TeamIDs: []int32{
		56392,
	}},
	{Code: "OM", Name: "Oman", TeamIDs: []int32{
		233941, 235168,
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
