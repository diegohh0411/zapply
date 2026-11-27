package alerts

import (
	"slices"

	"github.com/diegohh0411/zapply/internal/catalog"
)

// MinConfidence is how sure the classifier must be for a label to keep a
// posting from a member. A label below it is ignored, as if unknown.
const MinConfidence = 0.6

// Wants is what a member is looking for (their job_preference). An empty
// group sets no limit.
type Wants struct {
	SeniorityMin, SeniorityMax string
	Areas                      []string
	Countries                  []string
	RemoteOK                   bool
	ExcludedEmployment         []string
}

// Posting is what routing knows about a catalog posting: its labels, with
// confidence, and what the source said.
type Posting struct {
	Seniority      *string
	Areas          []string
	EmploymentType *string
	// Remote is the label, or the source's flag when there is no sure label.
	Remote bool
	// Countries are the labeled countries, or the source's country.
	Countries []string
}

// Fits reports whether p is for a member who wants w. Anything unknown or
// unsure passes: a missing label never hides a posting.
func Fits(w Wants, p Posting) bool {
	if w.SeniorityMin != "" && p.Seniority != nil {
		lo, hi, at := slices.Index(catalog.Seniorities, w.SeniorityMin), slices.Index(catalog.Seniorities, w.SeniorityMax), slices.Index(catalog.Seniorities, *p.Seniority)
		if at >= 0 && lo >= 0 && hi >= 0 && (at < lo || at > hi) {
			return false
		}
	}
	if len(w.Areas) > 0 && len(p.Areas) > 0 && !overlaps(w.Areas, p.Areas) {
		return false
	}
	if p.EmploymentType != nil && slices.Contains(w.ExcludedEmployment, *p.EmploymentType) {
		return false
	}
	if len(w.Countries) > 0 {
		switch {
		case overlaps(w.Countries, p.Countries):
		case p.Remote && len(p.Countries) == 0:
			// Remote from anywhere.
			if !w.RemoteOK {
				return false
			}
		case len(p.Countries) == 0:
			// Somewhere unknown.
		default:
			return false
		}
	}
	return true
}

func overlaps(a, b []string) bool {
	for _, x := range a {
		if slices.Contains(b, x) {
			return true
		}
	}
	return false
}

// sure keeps a label only when its confidence reaches MinConfidence. A label
// with no recorded confidence came from the source, and is kept.
func sure(conf map[string]float64, field string) bool {
	c, ok := conf[field]
	return !ok || c >= MinConfidence
}
