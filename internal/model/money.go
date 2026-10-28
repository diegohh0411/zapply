// Package model contains model accounting and provider-independent contracts.
package model

import (
	"encoding/json"
	"errors"
	"math/big"
	"regexp"
	"strings"
)

const UnitsPerUSD int64 = 1_000_000_000_000

var decimalAmount = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?$`)

// ParseUSD parses a non-negative decimal, rounding up to one picodollar.
// Floating point is never used for costs or allowance comparisons.
func ParseUSD(s string) (int64, error) {
	if len(s) == 0 || len(s) > 80 || !decimalAmount.MatchString(s) {
		return 0, errors.New("invalid USD amount")
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok || r.Sign() < 0 {
		return 0, errors.New("invalid USD amount")
	}
	r.Mul(r, new(big.Rat).SetInt64(UnitsPerUSD))
	q, rem := new(big.Int), new(big.Int)
	q.QuoRem(r.Num(), r.Denom(), rem)
	if rem.Sign() > 0 {
		q.Add(q, big.NewInt(1))
	}
	if !q.IsInt64() {
		return 0, errors.New("USD amount too large")
	}
	return q.Int64(), nil
}

func USD(units int64) string {
	return new(big.Rat).SetFrac(big.NewInt(units), big.NewInt(UnitsPerUSD)).FloatString(12)
}

// Cost accepts provider JSON numbers without converting through float64.
func Cost(raw json.RawMessage) (int64, error) {
	s := strings.Trim(string(raw), `"`)
	return ParseUSD(s)
}
