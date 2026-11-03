// Package billing predicts what a model call will cost before it is sent.
//
// A provider describes a model's billing as a Sheet: the rates it charges and
// the limits that bound a call. Predict turns a request's Shape into tokens and
// tokens into picodollars, widened by a margin learned from recent settlements.
// The prediction never exceeds the ceiling the sheet's limits imply.
//
// The same sheet describes chat LLMs and decision models. The difference is
// output: a chat endpoint enforces max_tokens (reasoning included), so output
// is reserved at that cap; a decision endpoint takes no output limit, so its
// sheet must not price output at all.
package billing

import (
	"errors"
	"math/big"
	"time"
)

// Sheet is what a provider knows about a model's billing. It is stored in the
// quote so estimates can predict without calling the provider.
type Sheet struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`

	// Rates in picodollars. Each is the highest rate among the components
	// that can apply to that unit, so the sheet never prices below reality.
	InputRate   int64 `json:"input_rate"`   // per input token
	OutputRate  int64 `json:"output_rate"`  // per output token
	RequestRate int64 `json:"request_rate"` // per call, or per question

	RequestPerQuestion bool `json:"request_per_question,omitempty"`
	InputPerQuestion   bool `json:"input_per_question,omitempty"` // state billed again for each question

	Context   int `json:"context"`    // billable input tokens per evaluation, at most
	MaxOutput int `json:"max_output"` // billable output tokens per call, at most

	// OutputEnforced means the provider stops billing output at MaxOutput (or
	// at the call's own max_tokens). Without it a positive OutputRate cannot
	// be bounded and Predict refuses the sheet.
	OutputEnforced bool `json:"output_enforced,omitempty"`

	MaxRequestBytes int `json:"max_request_bytes"`
	MaxQuestions    int `json:"max_questions,omitempty"` // zero means no count limit

	Prior      Tokenization `json:"prior"`
	Components []Component  `json:"components"`
	FetchedAt  time.Time    `json:"fetched_at"`
}

// Tokenization converts request size into tokens.
type Tokenization struct {
	TokensPerKB    int `json:"tokens_per_kb"`   // input tokens per 1,000 request bytes
	OverheadTokens int `json:"overhead_tokens"` // template tokens per call
	OutputPerQ     int `json:"output_per_q"`    // output tokens per question
}

// Component roles. Every pricing component a provider reports gets one; a
// component no role fits makes the model incompatible instead.
const (
	RoleInput    = "input"
	RoleOutput   = "output"
	RoleRequest  = "request"
	RoleUnused   = "unused" // billed for content decision calls never send
	RoleZero     = "ignored_zero"
	RoleDiscount = "discount"
)

type Component struct {
	Name string `json:"name"`
	Rate string `json:"rate"`
	Role string `json:"role"`
}

// Shape is what is known about a call before it is sent. A chat call is one
// question.
type Shape struct {
	RequestBytes int `json:"request_bytes"`
	Questions    int `json:"questions"`
	MaxOutput    int `json:"max_output,omitempty"` // the call's max_tokens; zero means the sheet's
}

// Calibration summarises recent settlements for one model.
type Calibration struct {
	Samples     int  `json:"samples"`
	TokensPerKB int  `json:"tokens_per_kb"` // high quantile of settled input tokens per KB
	OutputPerQ  int  `json:"output_per_q"`  // high quantile of settled output tokens per question
	CostPermil  int  `json:"cost_permil"`   // high quantile of settled/expected cost, in thousandths
	Exceeded    bool `json:"exceeded"`      // a recent settlement went over its reservation
}

// MinSamples is how many settlements a model needs before calibration
// replaces the sheet's prior.
const MinSamples = 5

// Margin bounds, in thousandths: at least 25% over the expected cost, and 10%
// over the worst recent settled/expected ratio.
const (
	minMarginPermil   = 1250
	ratioMarginPermil = 1100
)

const (
	BasisPrior      = "prior"
	BasisCalibrated = "calibrated"
	BasisCeiling    = "ceiling"
)

type Prediction struct {
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
	Expected     int64  `json:"expected"` // predicted cost without margin
	Reserved     int64  `json:"reserved"` // what the gateway holds
	Ceiling      int64  `json:"ceiling"`  // the most the sheet's limits allow
	Basis        string `json:"basis"`
}

var (
	errOverflow        = errors.New("quote overflow")
	ErrUnboundedOutput = errors.New("output is priced but not capped")
)

// Predict sizes a reservation for one call. Input is predicted and gets the
// margin; output is reserved at the enforced cap, as max_tokens bounds it
// exactly; request fees are exact. The result is never above the ceiling,
// never below the expected cost, and positive whenever any rate is.
func Predict(s Sheet, shape Shape, c Calibration) (Prediction, error) {
	if s.Context <= 0 || s.MaxOutput < 0 || shape.RequestBytes < 0 || shape.Questions <= 0 || shape.MaxOutput < 0 ||
		s.InputRate < 0 || s.OutputRate < 0 || s.RequestRate < 0 {
		return Prediction{}, errors.New("invalid quote")
	}
	if s.OutputRate > 0 && !s.OutputEnforced {
		return Prediction{}, ErrUnboundedOutput
	}
	outputCap := s.MaxOutput
	if shape.MaxOutput > 0 {
		outputCap = min(outputCap, shape.MaxOutput)
	}
	t := s.Prior
	basis := BasisPrior
	if c.Samples >= MinSamples {
		// Output only widens: settlements that omit output telemetry report
		// zero, which must not price output away.
		t.TokensPerKB, t.OutputPerQ = c.TokensPerKB, max(t.OutputPerQ, c.OutputPerQ)
		basis = BasisCalibrated
	}
	evals, fees := int64(1), int64(1)
	if s.InputPerQuestion {
		evals = int64(shape.Questions)
	}
	if s.RequestPerQuestion {
		fees = int64(shape.Questions)
	}

	input := new(big.Int).Mul(big.NewInt(int64(shape.RequestBytes)), big.NewInt(int64(t.TokensPerKB)))
	input = ceilDiv(input, big.NewInt(1000))
	input.Add(input, big.NewInt(int64(t.OverheadTokens)))
	input = minInt(input, big.NewInt(int64(s.Context)))
	capTokens := big.NewInt(int64(outputCap))
	output := minInt(new(big.Int).Mul(big.NewInt(int64(t.OutputPerQ)), big.NewInt(int64(shape.Questions))), capTokens)

	fee := new(big.Int).Mul(big.NewInt(s.RequestRate), big.NewInt(fees))
	inputCost := func(in *big.Int) *big.Int {
		n := new(big.Int).Mul(in, big.NewInt(s.InputRate))
		return n.Mul(n, big.NewInt(evals))
	}
	outputCost := func(out *big.Int) *big.Int { return new(big.Int).Mul(out, big.NewInt(s.OutputRate)) }
	expected := new(big.Int).Add(inputCost(input), outputCost(output))
	expected.Add(expected, fee)
	outputCeiling := outputCost(capTokens)
	ceiling := new(big.Int).Add(inputCost(big.NewInt(int64(s.Context))), outputCeiling)
	ceiling.Add(ceiling, fee)

	margin := int64(minMarginPermil)
	if c.Samples >= MinSamples {
		margin = max(margin, int64(c.CostPermil)*ratioMarginPermil/1000)
	}
	reserved := ceilDiv(new(big.Int).Mul(inputCost(input), big.NewInt(margin)), big.NewInt(1000))
	reserved.Add(reserved, outputCeiling)
	reserved.Add(reserved, fee)
	// An overrun, or a prediction that prices a paid call at nothing, falls
	// back to the ceiling.
	if c.Exceeded || (reserved.Sign() == 0 && ceiling.Sign() > 0) {
		basis = BasisCeiling
		reserved.Set(ceiling)
	}
	reserved = minInt(reserved, ceiling)

	if !expected.IsInt64() || !ceiling.IsInt64() || !reserved.IsInt64() {
		return Prediction{}, errOverflow
	}
	return Prediction{InputTokens: int(input.Int64()), OutputTokens: int(output.Int64()), Expected: expected.Int64(),
		Reserved: reserved.Int64(), Ceiling: ceiling.Int64(), Basis: basis}, nil
}

func ceilDiv(a, b *big.Int) *big.Int {
	q, r := new(big.Int).QuoRem(a, b, new(big.Int))
	if r.Sign() > 0 {
		q.Add(q, big.NewInt(1))
	}
	return q
}

func minInt(a, b *big.Int) *big.Int {
	if a.Cmp(b) > 0 {
		return new(big.Int).Set(b)
	}
	return a
}
