package library

import "testing"

func TestPartialDate(t *testing.T) {
	for _, s := range []string{"2019", "2021-03", "2023-06-14"} {
		d, err := ParsePartialDate(s)
		if err != nil || d.String() != s {
			t.Errorf("%q: got %v, %v", s, d, err)
		}
	}
	for _, s := range []string{"", "19", "2021-3", "2021-13", "2023-02-30", "1800", "2021/03"} {
		if _, err := ParsePartialDate(s); err == nil {
			t.Errorf("%q: accepted", s)
		}
	}

	mustParse := func(s string) PartialDate {
		d, err := ParsePartialDate(s)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	cases := []struct {
		end, start string
		ok         bool
	}{
		{"2021", "2021-03", true},
		{"2021-03", "2021-03-31", true},
		{"2020-12", "2021-03", false},
		{"2021-02-28", "2021-03", false},
		{"2022", "2021", true},
		{"2020", "2021", false},
	}
	for _, c := range cases {
		if got := mustParse(c.end).NotBefore(mustParse(c.start)); got != c.ok {
			t.Errorf("%s NotBefore %s = %v, want %v", c.end, c.start, got, c.ok)
		}
	}
}
