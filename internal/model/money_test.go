package model

import "testing"

func TestExactMoney(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int64
	}{{"0", 0}, {"0.1", 100000000000}, {"0.0000000000001", 1}, {"1e-9", 1000}, {"9223372.036854775807", 9223372036854775807}} {
		got, err := ParseUSD(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("ParseUSD(%q)=%d,%v want %d", tc.in, got, err, tc.want)
		}
	}
	for _, in := range []string{"", "-1", "1/2", "NaN", "1e100", "9223372.036854775808", " 1"} {
		if _, err := ParseUSD(in); err == nil {
			t.Errorf("accepted %q", in)
		}
	}
	if n, err := QuoteCost("0.0000004", "0.0000016", "0", 100, 128); err != nil || n != 244800000 {
		t.Fatalf("quote=%d,%v", n, err)
	}
	if _, err := QuoteCost("9000000", "1", "0", 2, 1); err == nil {
		t.Fatal("quote overflow accepted")
	}
}
