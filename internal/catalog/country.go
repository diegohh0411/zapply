package catalog

import (
	"strings"
)

// Country reads the ISO 3166-1 alpha-2 country a posting's location names:
// "Mexico City, Mexico", "Austin, TX", "Remote - Spain", "Toronto, ON, CA".
// It looks at each comma, dash or slash separated part from the last, so the
// broadest place decides; "" when none is recognized.
func Country(location string) string {
	parts := strings.FieldsFunc(location, func(r rune) bool {
		return r == ',' || r == '-' || r == '/' || r == '|' || r == '(' || r == ')' || r == '–'
	})
	for i := len(parts) - 1; i >= 0; i-- {
		raw := strings.TrimSpace(parts[i])
		if raw == "" {
			continue
		}
		// Two- and three-letter codes count only as written in capitals, so
		// "in" or "de" in a sentence is not India or Germany.
		if len(raw) <= 3 && raw == strings.ToUpper(raw) {
			c, country := codes[raw]
			_, state := usStates[raw]
			_, province := caProvinces[raw]
			switch {
			// "Austin, TX" is Texas, but "Toronto, ON, CA" is Canada: a code
			// after a state or province code is a country.
			case country && (!state || (i > 0 && isRegion(strings.TrimSpace(parts[i-1])))):
				return c
			case state:
				return "US"
			case province:
				return "CA"
			}
			continue
		}
		f := Fold(raw)
		if c, ok := names[f]; ok {
			return c
		}
		if _, ok := usStateNames[f]; ok {
			return "US"
		}
	}
	return ""
}

func isRegion(s string) bool {
	_, state := usStates[s]
	_, province := caProvinces[s]
	return state || province
}

// CountryCode reads one country a model or a source wrote, as a code or a
// name in English or Spanish; "" when unknown.
func CountryCode(s string) string {
	t := strings.TrimSpace(s)
	if len(t) == 2 {
		if c, ok := codes[strings.ToUpper(t)]; ok {
			return c
		}
	}
	if len(t) == 3 {
		if c, ok := codes[strings.ToUpper(t)]; ok {
			return c
		}
	}
	return names[Fold(t)]
}

// codes maps alpha-2 codes to themselves and common alpha-3 codes and
// abbreviations to alpha-2.
var codes = func() map[string]string {
	m := map[string]string{"USA": "US", "UK": "GB", "MEX": "MX", "CAN": "CA", "BRA": "BR", "ARG": "AR", "COL": "CO", "CHL": "CL",
		"PER": "PE", "ESP": "ES", "DEU": "DE", "FRA": "FR", "GBR": "GB", "IND": "IN", "PRT": "PT", "NLD": "NL", "IRL": "IE", "ITA": "IT",
		"POL": "PL", "AUS": "AU", "NZL": "NZ", "CHE": "CH", "SWE": "SE", "URY": "UY", "CRI": "CR", "UAE": "AE", "ARE": "AE"}
	for _, c := range alpha2 {
		m[c] = c
	}
	return m
}()

var alpha2 = strings.Fields(`AD AE AF AG AL AM AO AR AT AU AZ BA BB BD BE BF BG BH BI BJ BN BO BR BS BT BW BY BZ CA CD CF CG CH CI CL CM CN CO CR CU CV CY CZ
DE DJ DK DM DO DZ EC EE EG ER ES ET FI FJ FR GA GB GD GE GH GM GN GQ GR GT GW GY HK HN HR HT HU ID IE IL IN IQ IR IS IT JM JO JP KE KG KH KR KW KZ
LA LB LK LR LS LT LU LV LY MA MC MD ME MG MK ML MM MN MO MR MT MU MV MW MX MY MZ NA NE NG NI NL NO NP NZ OM PA PE PG PH PK PL PR PS PT PY QA RO RS
RU RW SA SC SD SE SG SI SK SL SN SO SR SV SY SZ TD TG TH TJ TL TM TN TR TT TW TZ UA UG US UY UZ VE VN YE ZA ZM ZW`)

// names maps folded country names, in English and Spanish, to alpha-2.
var names = map[string]string{
	"united states": "US", "united states of america": "US", "usa": "US", "us": "US", "estados unidos": "US", "eeuu": "US", "america": "US",
	"mexico": "MX", "canada": "CA", "brazil": "BR", "brasil": "BR", "argentina": "AR", "colombia": "CO", "chile": "CL", "peru": "PE",
	"uruguay": "UY", "paraguay": "PY", "bolivia": "BO", "ecuador": "EC", "venezuela": "VE", "costa rica": "CR", "panama": "PA",
	"guatemala": "GT", "honduras": "HN", "el salvador": "SV", "nicaragua": "NI", "dominican republic": "DO", "republica dominicana": "DO",
	"puerto rico": "PR", "cuba": "CU", "jamaica": "JM",
	"united kingdom": "GB", "uk": "GB", "great britain": "GB", "england": "GB", "scotland": "GB", "wales": "GB", "northern ireland": "GB", "reino unido": "GB",
	"ireland": "IE", "irlanda": "IE", "spain": "ES", "espana": "ES", "portugal": "PT", "france": "FR", "francia": "FR", "germany": "DE", "alemania": "DE", "deutschland": "DE",
	"italy": "IT", "italia": "IT", "netherlands": "NL", "the netherlands": "NL", "holland": "NL", "paises bajos": "NL", "belgium": "BE", "belgica": "BE",
	"switzerland": "CH", "suiza": "CH", "austria": "AT", "sweden": "SE", "suecia": "SE", "norway": "NO", "noruega": "NO", "denmark": "DK", "dinamarca": "DK",
	"finland": "FI", "finlandia": "FI", "iceland": "IS", "poland": "PL", "polonia": "PL", "czech republic": "CZ", "czechia": "CZ", "slovakia": "SK",
	"hungary": "HU", "romania": "RO", "rumania": "RO", "bulgaria": "BG", "greece": "GR", "grecia": "GR", "croatia": "HR", "serbia": "RS", "slovenia": "SI",
	"estonia": "EE", "latvia": "LV", "lithuania": "LT", "ukraine": "UA", "ucrania": "UA", "luxembourg": "LU", "malta": "MT", "cyprus": "CY",
	"turkey": "TR", "turkiye": "TR", "turquia": "TR", "russia": "RU", "israel": "IL", "united arab emirates": "AE", "uae": "AE", "emiratos arabes unidos": "AE",
	"saudi arabia": "SA", "qatar": "QA", "egypt": "EG", "morocco": "MA", "marruecos": "MA", "nigeria": "NG", "kenya": "KE", "south africa": "ZA", "sudafrica": "ZA",
	"ghana": "GH", "india": "IN", "pakistan": "PK", "bangladesh": "BD", "sri lanka": "LK", "china": "CN", "hong kong": "HK", "taiwan": "TW",
	"japan": "JP", "japon": "JP", "south korea": "KR", "korea": "KR", "corea del sur": "KR", "singapore": "SG", "singapur": "SG", "malaysia": "MY",
	"indonesia": "ID", "philippines": "PH", "filipinas": "PH", "vietnam": "VN", "thailand": "TH", "tailandia": "TH",
	"australia": "AU", "new zealand": "NZ", "nueva zelanda": "NZ",
}

// usStates are the postal codes of US states and DC.
var usStates = set(`AL AK AZ AR CA CO CT DE DC FL GA HI ID IL IN IA KS KY LA ME MD MA MI MN MS MO MT NE NV NH NJ NM NY NC ND OH OK OR PA RI SC SD TN TX UT VT VA WA WV WI WY`)

// caProvinces are Canada's province and territory codes.
var caProvinces = set(`AB BC MB NB NL NS NT NU ON PE QC SK YT`)

var usStateNames = set(`alabama|alaska|arizona|arkansas|california|colorado|connecticut|delaware|florida|georgia|hawaii|idaho|illinois|indiana|iowa|kansas|kentucky|louisiana|maine|maryland|massachusetts|michigan|minnesota|mississippi|missouri|montana|nebraska|nevada|new hampshire|new jersey|new mexico|new york|north carolina|north dakota|ohio|oklahoma|oregon|pennsylvania|rhode island|south carolina|south dakota|tennessee|texas|utah|vermont|virginia|washington|west virginia|wisconsin|wyoming|district of columbia|washington dc`)

func set(s string) map[string]struct{} {
	sep := " "
	if strings.Contains(s, "|") {
		sep = "|"
	}
	m := map[string]struct{}{}
	for _, v := range strings.Split(s, sep) {
		if v = strings.TrimSpace(v); v != "" {
			m[v] = struct{}{}
		}
	}
	return m
}
