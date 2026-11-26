package catalog

import (
	"regexp"
	"strings"
	"time"
	"unicode"

	"golang.org/x/text/unicode/norm"

	"github.com/diegohh0411/zapply/internal/joburl"
)

// FingerprintTTL is how long a company+title fingerprint holds: past it, the
// same role posted again is a new opening, not a duplicate.
const FingerprintTTL = 60 * 24 * time.Hour

// companySuffix drops legal forms, so "Acme, Inc." and "ACME" are one company.
var companySuffix = regexp.MustCompile(`\b(inc|llc|ltd|limited|corp|corporation|co|gmbh|sa|sas|s a|sa de cv|s de rl de cv|sapi de cv|bv|plc|ag)\b\.?$`)

// Company folds a company name and drops its legal form.
func Company(s string) string {
	return strings.TrimSpace(companySuffix.ReplaceAllString(Fold(s), ""))
}

// Fold lowercases s, strips accents and punctuation, and collapses spaces.
func Fold(s string) string {
	var b strings.Builder
	space := false
	for _, r := range norm.NFD.String(strings.ToLower(s)) {
		switch {
		case unicode.Is(unicode.Mn, r):
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			space = false
			b.WriteRune(r)
		default:
			space = true
		}
	}
	return b.String()
}

// Key kinds, the prefix of each identity.
const (
	KindATS = "ats"
	KindURL = "url"
	KindFP  = "fp"
)

// Keys are a sighting's identities, strongest first: its ATS posting, its
// canonical URL, and its company, title and country. Without a known country
// the fingerprint uses the folded location instead, so two cities in an
// unknown country stay apart rather than merge.
func (s Sighting) Keys() []string {
	var out []string
	if s.ATSKey != "" {
		out = append(out, KindATS+":"+s.ATSKey)
	}
	if u, err := joburl.Normalize(s.URL); err == nil {
		out = append(out, KindURL+":"+u)
	}
	c, t := Company(s.Company), Fold(s.Title)
	if c != "" && t != "" {
		where := strings.ToLower(s.country())
		if where == "" {
			where = Fold(s.Location)
		}
		out = append(out, KindFP+":"+c+"|"+t+"|"+where)
	}
	return out
}

// country is the source's country, else the one the location names.
func (s Sighting) country() string {
	if len(s.Country) == 2 {
		return strings.ToUpper(s.Country)
	}
	return Country(s.Location)
}

func kindOf(key string) string {
	k, _, _ := strings.Cut(key, ":")
	return k
}
