package preferences

import (
	"regexp"
	"sort"
	"time"

	"github.com/diegohh0411/zapply/internal/catalog"
)

// Entry is one piece of top-level, unarchived evidence, as derivation reads it.
type Entry struct {
	Kind     string
	Category string // education, paid, unpaid
	Title    string
	Org      string
	Location string
	Start    *time.Time
	End      *time.Time
	Current  bool
}

// Suggestion is what the rules derive from a library. Areas come from the
// model separately.
type Suggestion struct {
	// WorkYears is the work experience the seniority is read from.
	WorkYears          float64  `json:"work_years"`
	Enrolled           bool     `json:"enrolled"`
	Manager            bool     `json:"manager"`
	SeniorityMin       string   `json:"seniority_min"`
	SeniorityMax       string   `json:"seniority_max"`
	Countries          []string `json:"countries"`
	RemoteOK           bool     `json:"remote_ok"`
	ExcludedEmployment []string `json:"excluded_employment"`
	Areas              []string `json:"areas"`
}

const (
	// halfCapMonths bounds the credit internships and work during a degree
	// add up to: a year, however long they ran.
	halfCapMonths = 12
	// managerYears is the experience a manager title needs to count.
	managerYears = 3
	// skipInternshipsYears is the experience from which internships are skipped.
	skipInternshipsYears = 2
)

// fullWork kinds count fully as work when paid; halfWork kinds count half.
var (
	fullWork = map[string]bool{"job": true, "freelance": true, "business": true, "military_service": true}
	halfWork = map[string]bool{"internship": true, "apprenticeship": true}
)

// managerTitle matches titles of people who manage people, in English and Spanish.
// "Lead" alone (Tech Lead) does not count; "Lead of"/"Head of" do.
var managerTitle = regexp.MustCompile(`(?i)\b(manager|head of|head|director|directora|vp|vice president|lead of|gerente|jefe|jefa|coordinador|coordinadora|chief)\b`)

// Derive applies the rules to a library: work years from dated evidence, a
// seniority range from them, and defaults for where and which employment.
func Derive(entries []Entry, profileLocation string, now time.Time) Suggestion {
	nowM := month(now)
	span := func(e Entry) (int, int, bool) {
		if e.Start == nil {
			return 0, 0, false
		}
		s := month(*e.Start)
		end := s
		switch {
		case e.Current:
			end = nowM - 1 // through last month: this one is not over
		case e.End != nil:
			end = month(*e.End)
		}
		if end >= nowM {
			end = nowM - 1
		}
		if end < s && e.Current {
			end = s // started this month
		}
		if end < s {
			return 0, 0, false
		}
		return s, end, true
	}

	full, half, study := map[int]bool{}, map[int]bool{}, map[int]bool{}
	enrolled := false
	for _, e := range entries {
		s, end, ok := span(e)
		if !ok {
			continue
		}
		var into map[int]bool
		switch {
		case e.Kind == "degree_program":
			into = study
			if e.Current {
				enrolled = true
			}
		case fullWork[e.Kind] && e.Category == "paid":
			into = full
		case halfWork[e.Kind]: // paid or not, an internship is work experience
			into = half
		default:
			continue
		}
		for m := s; m <= end; m++ {
			into[m] = true
		}
	}
	fullMonths, halfMonths := 0, 0
	for m := range full {
		if study[m] {
			half[m] = true // work while studying counts half
		} else {
			fullMonths++
		}
	}
	for m := range half {
		if !full[m] || study[m] {
			halfMonths++
		}
	}
	credit := float64(halfMonths) / 2
	if credit > halfCapMonths {
		credit = halfCapMonths
	}
	years := round1((float64(fullMonths) + credit) / 12)

	sg := Suggestion{WorkYears: years, Enrolled: enrolled, RemoteOK: true, Countries: []string{}, ExcludedEmployment: []string{}, Areas: []string{}}
	switch {
	case years < 1:
		sg.SeniorityMin, sg.SeniorityMax = "intern", "entry"
	case years < 3:
		sg.SeniorityMin, sg.SeniorityMax = "entry", "mid"
	case years < 7:
		sg.SeniorityMin, sg.SeniorityMax = "mid", "senior"
	default:
		sg.SeniorityMin, sg.SeniorityMax = "senior", "staff"
	}
	if latest, ok := latestWork(entries, span); ok && years >= managerYears && managerTitle.MatchString(latest.Title) {
		sg.Manager = true
		sg.SeniorityMax = "manager"
	}
	if years >= skipInternshipsYears {
		sg.ExcludedEmployment = []string{"internship"}
	}
	if c := catalog.Country(profileLocation); c != "" {
		sg.Countries = []string{c}
	} else if latest, ok := latestWork(entries, span); ok {
		if c := catalog.Country(latest.Location); c != "" {
			sg.Countries = []string{c}
		}
	}
	return sg
}

// latestWork is the paid work that ended last (current work first).
func latestWork(entries []Entry, span func(Entry) (int, int, bool)) (Entry, bool) {
	var best Entry
	bestEnd, bestStart, found := -1, -1, false
	for _, e := range entries {
		if !(fullWork[e.Kind] && e.Category == "paid") {
			continue
		}
		s, end, ok := span(e)
		if !ok {
			continue
		}
		if end > bestEnd || (end == bestEnd && s > bestStart) {
			best, bestEnd, bestStart, found = e, end, s, true
		}
	}
	return best, found
}

func month(t time.Time) int { return t.Year()*12 + int(t.Month()) - 1 }

func round1(f float64) float64 { return float64(int(f*10+0.5)) / 10 }

// Levels are the seniority levels in order; manager sits above staff.
var Levels = catalog.Seniorities

func level(s string) int {
	for i, l := range Levels {
		if l == s {
			return i
		}
	}
	return -1
}

func sortedUnique(vs []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, v := range vs {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}
