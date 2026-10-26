package library

import (
	"errors"
	"strings"
)

// Positions are fractional sort keys: base-62 strings ordered bytewise (the
// database column uses COLLATE "C"). A key can always be made between two
// others, so moving an item rewrites only that item.
//
// Port of the midpoint step of Greenspan's fractional indexing, as in
// rocicorp/fractional-indexing, without the integer part.

const digits = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

var errBadPosition = errors.New("invalid position keys")

// PositionBetween returns a key strictly between a and b. An empty a means
// "before everything", an empty b "after everything".
func PositionBetween(a, b string) (string, error) {
	if !validKey(a) || !validKey(b) || (a != "" && b != "" && a >= b) {
		return "", errBadPosition
	}
	if b == "" {
		return after(a), nil
	}
	return midpoint(a, b, true), nil
}

// PositionsAfter returns n increasing keys after a (or from the start).
func PositionsAfter(a string, n int) []string {
	keys := make([]string, n)
	for i := range keys {
		a = after(a)
		keys[i] = a
	}
	return keys
}

// after returns a key just past a. Appending is the common case, so it steps
// by one unit in the second digit instead of halving the remaining space;
// keys stay two characters long for ~1900 appends.
func after(a string) string {
	if a == "" {
		return digits[len(digits)/2 : len(digits)/2+1]
	}
	b := []byte(a)
	for len(b) < 2 {
		b = append(b, digits[0])
	}
	for i := len(b) - 1; i >= 0; i-- {
		d := strings.IndexByte(digits, b[i])
		if d < len(digits)-1 {
			b[i] = digits[d+1]
			return string(b[:i+1]) // the carried digits after i were zeros
		}
		b[i] = digits[0]
	}
	return midpoint(a, "", false) // a was all 'z'
}

func validKey(k string) bool {
	if k == "" {
		return true
	}
	if k[len(k)-1] == digits[0] {
		return false
	}
	for i := range len(k) {
		if strings.IndexByte(digits, k[i]) < 0 {
			return false
		}
	}
	return true
}

func midpoint(a, b string, hasB bool) string {
	if hasB {
		n := 0
		for n < len(b) && digitAt(a, n) == b[n] {
			n++
		}
		if n > 0 {
			rest := ""
			if n < len(a) {
				rest = a[n:]
			}
			return b[:n] + midpoint(rest, b[n:], true)
		}
	}
	da := 0
	if a != "" {
		da = strings.IndexByte(digits, a[0])
	}
	db := len(digits)
	if hasB {
		db = strings.IndexByte(digits, b[0])
	}
	if db-da > 1 {
		return string(digits[(da+db+1)/2])
	}
	if hasB && len(b) > 1 {
		return b[:1]
	}
	rest := ""
	if len(a) > 1 {
		rest = a[1:]
	}
	return string(digits[da]) + midpoint(rest, "", false)
}

func digitAt(s string, i int) byte {
	if i < len(s) {
		return s[i]
	}
	return digits[0]
}
