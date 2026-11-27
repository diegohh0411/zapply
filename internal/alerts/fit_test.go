package alerts

import "testing"

func ptr[T any](v T) *T { return &v }

func TestFits(t *testing.T) {
	mx := Wants{SeniorityMin: "entry", SeniorityMax: "mid", Areas: []string{"engineering_software", "data_analytics"},
		Countries: []string{"MX"}, RemoteOK: true, ExcludedEmployment: []string{"internship"}}
	onsiteOnly := mx
	onsiteOnly.RemoteOK = false
	for _, tc := range []struct {
		name string
		w    Wants
		p    Posting
		want bool
	}{
		{"everything fits", mx, Posting{Seniority: ptr("mid"), Areas: []string{"engineering_software"}, EmploymentType: ptr("full_time"), Countries: []string{"MX"}}, true},
		{"too senior", mx, Posting{Seniority: ptr("senior"), Countries: []string{"MX"}}, false},
		{"too junior", mx, Posting{Seniority: ptr("intern"), Countries: []string{"MX"}}, false},
		{"secondary area fits", mx, Posting{Areas: []string{"sales", "data_analytics"}, Countries: []string{"MX"}}, true},
		{"other areas", mx, Posting{Areas: []string{"sales"}, Countries: []string{"MX"}}, false},
		{"excluded employment", mx, Posting{EmploymentType: ptr("internship"), Countries: []string{"MX"}}, false},
		{"another country", mx, Posting{Countries: []string{"US"}}, false},
		{"remote within another country", mx, Posting{Remote: true, Countries: []string{"US"}}, false},
		{"remote within one of theirs", mx, Posting{Remote: true, Countries: []string{"US", "MX"}}, true},
		{"remote from anywhere", mx, Posting{Remote: true}, true},
		{"remote from anywhere, onsite only", onsiteOnly, Posting{Remote: true}, false},
		{"unknown place", onsiteOnly, Posting{}, true},
		{"nothing labeled", mx, Posting{}, true},
		{"no preferences", Wants{}, Posting{Seniority: ptr("staff"), Areas: []string{"sales"}, EmploymentType: ptr("internship"), Countries: []string{"US"}}, true},
		{"no areas set", Wants{Countries: []string{"US"}}, Posting{Areas: []string{"healthcare"}, Countries: []string{"US"}}, true},
	} {
		if got := Fits(tc.w, tc.p); got != tc.want {
			t.Errorf("%s: Fits = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestUnsureLabelsAreIgnored(t *testing.T) {
	conf := map[string]float64{"seniority": 0.55, "areas": 0.9, "employment_type": 0.4, "remote": 0.5, "countries": 0.59}
	l := labelsOf(true, ptr("MX"), ptr("staff"), []string{"sales"}, ptr("internship"), ptr(false), []string{"US"}, conf)
	if l.Seniority != nil || l.EmploymentType != nil {
		t.Errorf("unsure seniority or employment kept: %+v", l)
	}
	if !l.Remote {
		t.Errorf("unsure remote label replaced the source's flag")
	}
	if len(l.Countries) != 1 || l.Countries[0] != "MX" {
		t.Errorf("countries = %v, want the source's MX", l.Countries)
	}
	if len(l.Areas) != 1 {
		t.Errorf("sure areas dropped: %v", l.Areas)
	}
	sure := labelsOf(false, nil, ptr("staff"), nil, nil, ptr(true), []string{"US"}, map[string]float64{"seniority": 0.6, "remote": 0.99, "countries": 0.8})
	if sure.Seniority == nil || !sure.Remote || sure.Countries[0] != "US" {
		t.Errorf("sure labels dropped: %+v", sure)
	}
}
