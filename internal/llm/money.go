// Package llm contains model accounting and provider-independent contracts.
package llm

import (
	"encoding/json"
	"errors"
	"math/big"
	"regexp"
	"strconv"
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

func checkedMul(rate int64, tokens int) (int64, error) {
	if tokens < 0 || rate < 0 {
		return 0, errors.New("invalid quote")
	}
	n := new(big.Int).Mul(big.NewInt(rate), big.NewInt(int64(tokens)))
	if !n.IsInt64() {
		return 0, errors.New("quote overflow")
	}
	return strconv.ParseInt(n.String(), 10, 64)
}

func QuoteCost(inputRate, outputRate, requestRate string, input, output int) (int64, error) {
	a, err := ParseUSD(inputRate)
	if err != nil {
		return 0, err
	}
	b, err := ParseUSD(outputRate)
	if err != nil {
		return 0, err
	}
	c, err := ParseUSD(requestRate)
	if err != nil {
		return 0, err
	}
	x, err := checkedMul(a, input)
	if err != nil {
		return 0, err
	}
	y, err := checkedMul(b, output)
	if err != nil {
		return 0, err
	}
	total := new(big.Int).Add(big.NewInt(x), big.NewInt(y))
	total.Add(total, big.NewInt(c))
	if !total.IsInt64() {
		return 0, errors.New("quote overflow")
	}
	return total.Int64(), nil
}
