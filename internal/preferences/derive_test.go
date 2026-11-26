package preferences

import (
	"slices"
	"testing"
	"time"
)

var now = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

func d(y, m int) *time.Time { t := time.Date(y, time.Month(m), 1, 0, 0, 0, 0, time.UTC); return &t }

func TestDerive(t *testing.T) {
	cases := []struct {
		name     string
		entries  []Entry
		location string
		years    float64
		min, max string
		skip     []string
		country  []string
	}{
		{
			name: "student with two internships",
			entries: []Entry{
				{Kind: "degree_program", Category: "education", Start: d(2023, 8), Current: true},
				{Kind: "internship", Category: "paid", Start: d(2024, 6), End: d(2024, 8)},
				{Kind: "internship", Category: "unpaid", Start: d(2025, 6), End: d(2025, 8)},
			},
			location: "Monterrey, Mexico",
			years:    0.3, min: "intern", max: "entry", skip: []string{}, country: []string{"MX"},
		},
		{
			name:    "a year and a half of work",
			entries: []Entry{{Kind: "job", Category: "paid", Title: "Analyst", Start: d(2025, 5), Current: true}},
			years:   1.5, min: "entry", max: "mid", skip: []string{}, country: []string{},
		},
		{
			name: "ten-year senior engineer, overlapping jobs count once",
			entries: []Entry{
				{Kind: "job", Category: "paid", Title: "Software Engineer", Start: d(2016, 11), End: d(2021, 10), Location: "Austin, TX"},
				{Kind: "freelance", Category: "paid", Title: "Consultant", Start: d(2020, 1), End: d(2020, 12)},
				{Kind: "job", Category: "paid", Title: "Senior Engineer", Start: d(2021, 11), Current: true, Location: "Austin, TX"},
				{Kind: "volunteering", Category: "unpaid", Start: d(2010, 1), End: d(2015, 1)},
			},
			years: 10, min: "senior", max: "staff", skip: []string{"internship"}, country: []string{"US"},
		},
		{
			name: "manager",
			entries: []Entry{
				{Kind: "job", Category: "paid", Title: "Engineer", Start: d(2018, 11), End: d(2022, 10)},
				{Kind: "job", Category: "paid", Title: "Engineering Manager", Start: d(2022, 11), Current: true},
			},
			years: 8, min: "senior", max: "manager", skip: []string{"internship"}, country: []string{},
		},
		{
			name: "tech lead is not a manager; work during a degree counts half",
			entries: []Entry{
				{Kind: "degree_program", Category: "education", Start: d(2018, 1), End: d(2021, 12)},
				{Kind: "job", Category: "paid", Title: "Developer", Start: d(2020, 1), End: d(2021, 12)},
				{Kind: "job", Category: "paid", Title: "Tech Lead", Start: d(2022, 1), Current: true},
			},
			years: 5.8, min: "mid", max: "senior", skip: []string{"internship"}, country: []string{},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := Derive(c.entries, c.location, now)
			if s.WorkYears != c.years || s.SeniorityMin != c.min || s.SeniorityMax != c.max ||
				!slices.Equal(s.ExcludedEmployment, c.skip) || !slices.Equal(s.Countries, c.country) || !s.RemoteOK {
				t.Fatalf("got %+v", s)
			}
		})
	}
}

func TestDeriveCapsInternshipCredit(t *testing.T) {
	s := Derive([]Entry{{Kind: "internship", Category: "paid", Start: d(2020, 1), End: d(2025, 12)}}, "", now)
	if s.WorkYears != 1 {
		t.Fatalf("five years of internships should credit one year, got %v", s.WorkYears)
	}
}
