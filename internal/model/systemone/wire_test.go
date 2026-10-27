package systemone

import "testing"

func TestCanonicalJSONBRoundTrip(t *testing.T) {
	for _, pair := range [][2]string{{`{"x":1.0,"y":1e3}`, `{"y":1000.00,"x":1}`}, {`0.0010`, `1e-3`}, {`-0.0`, `0`}, {`[1.00,0.00012]`, `[1,1.2e-4]`}} {
		a, e := Canonical([]byte(pair[0]))
		if e != nil {
			t.Fatal(e)
		}
		b, e := Canonical([]byte(pair[1]))
		if e != nil {
			t.Fatal(e)
		}
		if string(a) != string(b) {
			t.Fatalf("%s != %s", a, b)
		}
	}
	if _, e := Canonical([]byte(`{} {}`)); e == nil {
		t.Fatal("trailing document accepted")
	}
}
