package library

import (
	"fmt"
	"time"
)

// Precision is how much of a date the user actually knows.
type Precision string

const (
	Year  Precision = "year"
	Month Precision = "month"
	Day   Precision = "day"
)

// PartialDate is a date known to year, month or day precision. Its text form
// is "2019", "2021-03" or "2023-06-14"; unknown parts are never invented.
type PartialDate struct {
	Time      time.Time // first instant of the period, UTC
	Precision Precision
}

var layouts = map[Precision]string{Year: "2006", Month: "2006-01", Day: "2006-01-02"}

func ParsePartialDate(s string) (PartialDate, error) {
	var p Precision
	switch len(s) {
	case 4:
		p = Year
	case 7:
		p = Month
	case 10:
		p = Day
	default:
		return PartialDate{}, fmt.Errorf("date %q must be YYYY, YYYY-MM or YYYY-MM-DD", s)
	}
	t, err := time.Parse(layouts[p], s)
	if err != nil {
		return PartialDate{}, fmt.Errorf("date %q must be YYYY, YYYY-MM or YYYY-MM-DD", s)
	}
	if t.Year() < 1900 || t.Year() > 2200 {
		return PartialDate{}, fmt.Errorf("date %q is out of range", s)
	}
	return PartialDate{Time: t, Precision: p}, nil
}

func (d PartialDate) String() string { return d.Time.Format(layouts[d.Precision]) }

// end returns the first instant after the period.
func (d PartialDate) end() time.Time {
	switch d.Precision {
	case Year:
		return d.Time.AddDate(1, 0, 0)
	case Month:
		return d.Time.AddDate(0, 1, 0)
	default:
		return d.Time.AddDate(0, 0, 1)
	}
}

// NotBefore reports whether d can end a period that starts at start: some
// moment of d must not precede start. "2021" can end a period starting
// "2021-03"; "2020-12" cannot.
func (d PartialDate) NotBefore(start PartialDate) bool {
	return d.end().After(start.Time)
}
