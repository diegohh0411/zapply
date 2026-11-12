package api

import (
	"encoding/json"
	"testing"

	"github.com/diegohh0411/zapply/internal/model/openrouter"
)

func TestSupportsEffortFollowsTheCatalogue(t *testing.T) {
	var models []openrouter.DecisionModel
	if err := json.Unmarshal([]byte(`[
		{"id":"reason/model","reasoning":{"supported_efforts":["high","low","none"]}},
		{"id":"mandatory/model","reasoning":{"mandatory":true,"supported_efforts":["high","none"]}},
		{"id":"plain/model"}
	]`), &models); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		model, effort string
		want          bool
	}{
		{"reason/model", "high", true},
		{"reason/model", "off", true},
		{"reason/model", "xhigh", false},
		{"mandatory/model", "high", true},
		{"mandatory/model", "off", false},
		{"plain/model", "low", false},
		{"missing/model", "low", false},
	} {
		if got := supportsEffort(models, tc.model, tc.effort); got != tc.want {
			t.Errorf("supportsEffort(%s, %s) = %v; want %v", tc.model, tc.effort, got, tc.want)
		}
	}
}
