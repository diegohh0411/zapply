package billing

import (
	"math"
	"testing"
)

func sheet() Sheet {
	return Sheet{InputRate: 100, OutputRate: 1000, RequestRate: 7, RequestPerQuestion: true, Context: 10_000, MaxOutput: 500,
		Prior: Tokenization{TokensPerKB: 1000, OverheadTokens: 100, OutputPerQ: 2}}
}

func TestPredictPrior(t *testing.T) {
	p, e := Predict(sheet(), Shape{RequestBytes: 2000, Questions: 3}, Calibration{})
	if e != nil {
		t.Fatal(e)
	}
	// input 2000+100 tokens, output 6, fee 3×7.
	if p.InputTokens != 2100 || p.OutputTokens != 6 || p.Expected != 2100*100+6*1000+21 || p.Basis != BasisPrior {
		t.Fatalf("%+v", p)
	}
	if want := int64((2100*100+6*1000)*1250/1000 + 21); p.Reserved != want {
		t.Fatalf("reserved %d want %d", p.Reserved, want)
	}
	if want := int64(10_000*100 + 500*1000 + 21); p.Ceiling != want {
		t.Fatalf("ceiling %d want %d", p.Ceiling, want)
	}
}

func TestPredictCapsAtCeiling(t *testing.T) {
	p, e := Predict(sheet(), Shape{RequestBytes: 1 << 20, Questions: 1000}, Calibration{})
	if e != nil {
		t.Fatal(e)
	}
	if p.InputTokens != 10_000 || p.OutputTokens != 500 || p.Reserved != p.Ceiling || p.Expected != p.Ceiling {
		t.Fatalf("%+v", p)
	}
}

func TestPredictCalibrated(t *testing.T) {
	s := sheet()
	// Calibration never lowers output below the prior.
	p, _ := Predict(s, Shape{RequestBytes: 2000, Questions: 1}, Calibration{Samples: MinSamples, TokensPerKB: 250, OutputPerQ: 1})
	if p.OutputTokens != 2 {
		t.Fatalf("%+v", p)
	}
	// Too few samples keep the prior.
	p, _ = Predict(s, Shape{RequestBytes: 2000, Questions: 1}, Calibration{Samples: MinSamples - 1, TokensPerKB: 10})
	if p.Basis != BasisPrior {
		t.Fatalf("%+v", p)
	}
	p, _ = Predict(s, Shape{RequestBytes: 2000, Questions: 1}, Calibration{Samples: MinSamples, TokensPerKB: 250, OutputPerQ: 3, CostPermil: 500})
	if p.Basis != BasisCalibrated || p.InputTokens != 600 || p.OutputTokens != 3 {
		t.Fatalf("%+v", p)
	}
	if want := int64((600*100+3000)*1250/1000 + 7); p.Reserved != want {
		t.Fatalf("low ratio keeps the minimum margin: %d want %d", p.Reserved, want)
	}
	// Settlements at twice the prediction widen the margin to 2.2×.
	p, _ = Predict(s, Shape{RequestBytes: 2000, Questions: 1}, Calibration{Samples: MinSamples, TokensPerKB: 250, OutputPerQ: 3, CostPermil: 2000})
	if want := int64((600*100+3000)*2200/1000 + 7); p.Reserved != want {
		t.Fatalf("widened margin: %d want %d", p.Reserved, want)
	}
}

func TestPredictExceededUsesCeiling(t *testing.T) {
	p, _ := Predict(sheet(), Shape{RequestBytes: 10, Questions: 1}, Calibration{Samples: 50, TokensPerKB: 1, Exceeded: true})
	if p.Basis != BasisCeiling || p.Reserved != p.Ceiling {
		t.Fatalf("%+v", p)
	}
}

func TestPredictRates(t *testing.T) {
	// Only an output rate: still a positive reservation.
	p, _ := Predict(Sheet{OutputRate: 5, Context: 100, MaxOutput: 10, Prior: Tokenization{OutputPerQ: 1}}, Shape{RequestBytes: 1, Questions: 1}, Calibration{})
	if p.Reserved <= 0 {
		t.Fatalf("%+v", p)
	}
	// Free model reserves nothing.
	p, _ = Predict(Sheet{Context: 100, Prior: Tokenization{TokensPerKB: 1000}}, Shape{RequestBytes: 1, Questions: 1}, Calibration{})
	if p.Reserved != 0 || p.Ceiling != 0 {
		t.Fatalf("%+v", p)
	}
	// Input billed once per question.
	s := sheet()
	s.InputPerQuestion = true
	p, _ = Predict(s, Shape{RequestBytes: 900, Questions: 2}, Calibration{})
	if p.Expected != 2*1000*100+4*1000+14 {
		t.Fatalf("%+v", p)
	}
}

func TestPredictRejects(t *testing.T) {
	for name, c := range map[string]struct {
		s     Sheet
		shape Shape
	}{
		"no context":   {Sheet{}, Shape{Questions: 1}},
		"no questions": {sheet(), Shape{}},
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
	s := Sheet{InputRate: 42_000, RequestPerQuestion: true, Context: 32_768, MaxOutput: 262_144,
		Prior: Tokenization{TokensPerKB: 1000, OverheadTokens: 1024, OutputPerQ: 16}}
	shape := Shape{RequestBytes: 62_000, Questions: 96}
	settled := int64(12_705 * 42_000)
	prior, e := Predict(s, shape, Calibration{})
	if e != nil || prior.Reserved < settled {
		t.Fatalf("prior %+v below settled %d: %v", prior, settled, e)
	}
	// What platform.decision_calibration returns for this one sample.
	c := Calibration{Samples: MinSamples, TokensPerKB: (12_705*1000 + 62_000 - 1) / 62_000}
	expected, _ := Predict(s, shape, c)
	c.CostPermil = int((settled*1000 + expected.Expected - 1) / expected.Expected)
	p, e := Predict(s, shape, c)
	if e != nil || p.Reserved < settled || p.Reserved > 3*settled || p.Basis != BasisCalibrated {
		t.Fatalf("calibrated %+v, settled %d: %v", p, settled, e)
	}
}

// Five settlements that omitted output telemetry calibrate output to zero.
// An output-priced model must still reserve.
func TestPredictMissingOutputTelemetry(t *testing.T) {
	s := Sheet{OutputRate: 1000, Context: 100, MaxOutput: 500, Prior: Tokenization{TokensPerKB: 1000, OutputPerQ: 2}}
	p, e := Predict(s, Shape{RequestBytes: 100, Questions: 1}, Calibration{Samples: MinSamples, TokensPerKB: 100, CostPermil: 1000})
	if e != nil || p.OutputTokens != 2 || p.Reserved <= 0 {
		t.Fatalf("%+v %v", p, e)
	}
	// With no prior to fall back on, a paid call reserves the ceiling.
	s.Prior.OutputPerQ = 0
	p, _ = Predict(s, Shape{RequestBytes: 100, Questions: 1}, Calibration{Samples: MinSamples, CostPermil: 1000})
	if p.Reserved != p.Ceiling || p.Basis != BasisCeiling {
		t.Fatalf("%+v", p)
	}
}
