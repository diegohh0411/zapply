package billing

import (
	"errors"
	"math"
	"testing"
)

// chat is a chat-completion sheet: output priced and capped by max_tokens.
func chat() Sheet {
	return Sheet{InputRate: 100, OutputRate: 1000, RequestRate: 7, RequestPerQuestion: true, Context: 10_000, MaxOutput: 500,
		OutputEnforced: true, Prior: Tokenization{TokensPerKB: 1000, OverheadTokens: 100, OutputPerQ: 2}}
}

// decision is a decision sheet: no output limit, so no output price.
func decision() Sheet {
	return Sheet{InputRate: 100, RequestRate: 7, RequestPerQuestion: true, Context: 10_000,
		Prior: Tokenization{TokensPerKB: 1000, OverheadTokens: 100, OutputPerQ: 2}}
}

func TestPredictPrior(t *testing.T) {
	p, e := Predict(decision(), Shape{RequestBytes: 2000, Questions: 3}, Calibration{})
	if e != nil {
		t.Fatal(e)
	}
	// input 2000+100 tokens, fee 3×7.
	if p.InputTokens != 2100 || p.Expected != 2100*100+21 || p.Basis != BasisPrior {
		t.Fatalf("%+v", p)
	}
	if want := int64(2100*100*1250/1000 + 21); p.Reserved != want {
		t.Fatalf("reserved %d want %d", p.Reserved, want)
	}
	if want := int64(10_000*100 + 21); p.Ceiling != want {
		t.Fatalf("ceiling %d want %d", p.Ceiling, want)
	}
}

func TestPredictChatReservesOutputCap(t *testing.T) {
	p, e := Predict(chat(), Shape{RequestBytes: 2000, Questions: 1}, Calibration{})
	if e != nil {
		t.Fatal(e)
	}
	// Expected output is the prior; the reservation holds the whole cap.
	if p.OutputTokens != 2 || p.Expected != 2100*100+2*1000+7 {
		t.Fatalf("%+v", p)
	}
	if want := int64(2100*100*1250/1000 + 500*1000 + 7); p.Reserved != want {
		t.Fatalf("reserved %d want %d", p.Reserved, want)
	}
	// The call's own max_tokens tightens the cap.
	p, _ = Predict(chat(), Shape{RequestBytes: 2000, Questions: 1, MaxOutput: 128}, Calibration{})
	if want := int64(2100*100*1250/1000 + 128*1000 + 7); p.Reserved != want || p.Ceiling != 10_000*100+128*1000+7 {
		t.Fatalf("max_tokens cap %+v", p)
	}
	// A larger max_tokens never exceeds the endpoint's limit.
	p, _ = Predict(chat(), Shape{RequestBytes: 2000, Questions: 1, MaxOutput: 1 << 20}, Calibration{})
	if want := int64(2100*100*1250/1000 + 500*1000 + 7); p.Reserved != want {
		t.Fatalf("endpoint cap %+v", p)
	}
}

func TestPredictRejectsUncappedOutput(t *testing.T) {
	s := chat()
	s.OutputEnforced = false
	if _, e := Predict(s, Shape{RequestBytes: 1, Questions: 1}, Calibration{}); !errors.Is(e, ErrUnboundedOutput) {
		t.Fatalf("uncapped output priced: %v", e)
	}
}

func TestPredictCapsAtCeiling(t *testing.T) {
	p, e := Predict(chat(), Shape{RequestBytes: 1 << 20, Questions: 1000}, Calibration{})
	if e != nil {
		t.Fatal(e)
	}
	if p.InputTokens != 10_000 || p.OutputTokens != 500 || p.Reserved != p.Ceiling || p.Expected != p.Ceiling {
		t.Fatalf("%+v", p)
	}
}

func TestPredictCalibrated(t *testing.T) {
	s := decision()
	// Too few samples keep the prior.
	p, _ := Predict(s, Shape{RequestBytes: 2000, Questions: 1}, Calibration{Samples: MinSamples - 1, TokensPerKB: 10})
	if p.Basis != BasisPrior {
		t.Fatalf("%+v", p)
	}
	p, _ = Predict(s, Shape{RequestBytes: 2000, Questions: 1}, Calibration{Samples: MinSamples, TokensPerKB: 250, CostPermil: 500})
	if p.Basis != BasisCalibrated || p.InputTokens != 600 {
		t.Fatalf("%+v", p)
	}
	if want := int64(600*100*1250/1000 + 7); p.Reserved != want {
		t.Fatalf("low ratio keeps the minimum margin: %d want %d", p.Reserved, want)
	}
	// Settlements at twice the prediction widen the margin to 2.2×.
	p, _ = Predict(s, Shape{RequestBytes: 2000, Questions: 1}, Calibration{Samples: MinSamples, TokensPerKB: 250, CostPermil: 2000})
	if want := int64(600*100*2200/1000 + 7); p.Reserved != want {
		t.Fatalf("widened margin: %d want %d", p.Reserved, want)
	}
	// Calibrated output widens the expected cost but never drops below the
	// prior: settlements that omit output telemetry report zero.
	p, _ = Predict(chat(), Shape{RequestBytes: 2000, Questions: 1}, Calibration{Samples: MinSamples, TokensPerKB: 250, OutputPerQ: 40})
	if p.OutputTokens != 40 {
		t.Fatalf("%+v", p)
	}
	p, _ = Predict(chat(), Shape{RequestBytes: 2000, Questions: 1}, Calibration{Samples: MinSamples, TokensPerKB: 250})
	if p.OutputTokens != 2 {
		t.Fatalf("%+v", p)
	}
}

func TestPredictExceededUsesCeiling(t *testing.T) {
	p, _ := Predict(chat(), Shape{RequestBytes: 10, Questions: 1}, Calibration{Samples: 50, TokensPerKB: 1, Exceeded: true})
	if p.Basis != BasisCeiling || p.Reserved != p.Ceiling {
		t.Fatalf("%+v", p)
	}
}

func TestPredictRates(t *testing.T) {
	// Free model reserves nothing.
	p, _ := Predict(Sheet{Context: 100, Prior: Tokenization{TokensPerKB: 1000}}, Shape{RequestBytes: 1, Questions: 1}, Calibration{})
	if p.Reserved != 0 || p.Ceiling != 0 {
		t.Fatalf("%+v", p)
	}
	// A paid input that predicts to zero tokens reserves the ceiling.
	p, _ = Predict(Sheet{InputRate: 5, Context: 100}, Shape{RequestBytes: 0, Questions: 1}, Calibration{})
	if p.Reserved != p.Ceiling || p.Reserved != 500 || p.Basis != BasisCeiling {
		t.Fatalf("%+v", p)
	}
	// Input billed once per question.
	s := decision()
	s.InputPerQuestion = true
	p, _ = Predict(s, Shape{RequestBytes: 900, Questions: 2}, Calibration{})
	if p.Expected != 2*1000*100+14 {
		t.Fatalf("%+v", p)
	}
}

func TestPredictRejects(t *testing.T) {
	for name, c := range map[string]struct {
		s     Sheet
		shape Shape
	}{
		"no context":   {Sheet{}, Shape{Questions: 1}},
		"no questions": {decision(), Shape{}},
		"negative":     {Sheet{Context: 1, InputRate: -1}, Shape{Questions: 1}},
		"overflow":     {Sheet{Context: math.MaxInt32, InputRate: math.MaxInt64}, Shape{Questions: 1}},
	} {
		if _, e := Predict(c.s, c.shape, Calibration{}); e == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// One recorded settlement on ~liquid/d1-ultra-latest: a 62 KB, 96-question
// request billed 12,705 input tokens for $0.00053361 at $0.000000042/token.
// The reservation must cover it under the prior, and stay within 3× once
// calibrated from it.
func TestPredictRecordedSettlement(t *testing.T) {
	s := Sheet{InputRate: 42_000, RequestPerQuestion: true, Context: 32_768,
		Prior: Tokenization{TokensPerKB: 1000, OverheadTokens: 1024, OutputPerQ: 16}}
	shape := Shape{RequestBytes: 62_000, Questions: 96}
	settled := int64(12_705 * 42_000)
	prior, e := Predict(s, shape, Calibration{})
	if e != nil || prior.Reserved < settled {
		t.Fatalf("prior %+v below settled %d: %v", prior, settled, e)
	}
	// What platform.model_calibration returns for this one sample.
	c := Calibration{Samples: MinSamples, TokensPerKB: (12_705*1000 + 62_000 - 1) / 62_000}
	expected, _ := Predict(s, shape, c)
	c.CostPermil = int((settled*1000 + expected.Expected - 1) / expected.Expected)
	p, e := Predict(s, shape, c)
	if e != nil || p.Reserved < settled || p.Reserved > 3*settled || p.Basis != BasisCalibrated {
		t.Fatalf("calibrated %+v, settled %d: %v", p, settled, e)
	}
}
