package library

import (
	"math/rand/v2"
	"slices"
	"testing"
)

func TestPositionBetween(t *testing.T) {
	keys := PositionsAfter("", 50)
	if !slices.IsSorted(keys) {
		t.Fatalf("PositionsAfter not increasing: %v", keys)
	}
	if len(keys[49]) > 3 {
		t.Errorf("50 appended keys grew to %q", keys[49])
	}

	// Random inserts anywhere keep a strictly increasing sequence.
	r := rand.New(rand.NewPCG(1, 2))
	seq := []string{}
	for range 2000 {
		i := r.IntN(len(seq) + 1)
		var a, b string
		if i > 0 {
			a = seq[i-1]
		}
		if i < len(seq) {
			b = seq[i]
		}
		k, err := PositionBetween(a, b)
		if err != nil {
			t.Fatalf("between %q and %q: %v", a, b, err)
		}
		if (a != "" && k <= a) || (b != "" && k >= b) || !validKey(k) {
			t.Fatalf("between %q and %q got %q", a, b, k)
		}
		seq = slices.Insert(seq, i, k)
	}
	if !slices.IsSorted(seq) {
		t.Fatal("sequence not sorted")
	}

	// Repeatedly inserting at the front stays valid.
	first := "V"
	for range 200 {
		k, err := PositionBetween("", first)
		if err != nil || k >= first {
			t.Fatalf("before %q: %q %v", first, k, err)
		}
		first = k
	}

	for _, c := range [][2]string{{"b", "a"}, {"a", "a"}, {"a0", ""}, {"a!", ""}} {
		if _, err := PositionBetween(c[0], c[1]); err == nil {
			t.Errorf("between %q and %q: accepted", c[0], c[1])
		}
	}
}
