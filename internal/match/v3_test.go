package match

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/diegohh0411/zapply/internal/model/systemone"
)

func TestWholePostingQuestionsKeepFullTextAndCacheIndependentItems(t *testing.T) {
	s := v2Snapshot()
	s.Description = strings.Repeat("What We're Looking For\nRuby and web fundamentals.\n", 140) + "END OF POSTING"
	questions, err := ReadQuestionsV3(s)
	if err != nil {
		t.Fatal(err)
	}
	// Seniority, three skills, three eligible entries, three highlights.
	if len(questions) != 10 || DefaultPipeline != PipelineV3 {
		t.Fatalf("questions %d default %d", len(questions), DefaultPipeline)
	}
	hashes := map[string]string{}
	for _, q := range questions {
		if err := systemone.ValidateQuestion(q.Question); err != nil {
			t.Fatal(err)
		}
		var state struct{ Posting struct{ Description string } }
		if err := json.Unmarshal(q.State, &state); err != nil || state.Posting.Description != s.Description {
			t.Fatalf("posting changed or truncated: %v", err)
		}
		if strings.Contains(string(q.State), `"lines"`) || strings.Contains(q.Subject, "line:") || strings.Contains(string(q.Question.Instructions), "auto_include") {
			t.Fatalf("chunking or selection flags in model input: %s", q.Subject)
		}
		hashes[q.Subject] = q.StateHash + q.QuestionHash
	}
	s.Evidence[0].Summary = "Changed only this entry"
	s.Evidence[1].AutoInclude = false
	s.Evidence[0].Highlights[0].AutoInclude = false
	again, _ := ReadQuestionsV3(s)
	for _, q := range again {
		changed := hashes[q.Subject] != q.StateHash+q.QuestionHash
		if changed != (q.Subject == "relevance:job") {
			t.Fatalf("%s changed=%v after one entry edit", q.Subject, changed)
		}
	}
	reqs, incomplete := noRequirements(s, nil)
	cover, err := noCoverageQuestions(s, reqs)
	if len(reqs) != 0 || incomplete || len(cover) != 0 || err != nil {
		t.Fatal("whole-posting matching planned requirement analysis")
	}
}

func TestWholePostingRankingWithoutCoverage(t *testing.T) {
	s := v2Snapshot()
	a := map[string]systemone.Answer{
		"seniority":     choiceAnswer("mid", "mid", "not_specified"),
		"relevance:job": scoreAnswer(3, 5), "relevance:degree": scoreAnswer(1, 5), "relevance:hidden": scoreAnswer(4, 5),
		"highlight:h1": scoreAnswer(1, 5), "highlight:h2": scoreAnswer(4, 5), "highlight:h3": scoreAnswer(2, 5),
	}
	for _, sk := range s.Skills {
		p := .7
		if sk.ID == "go" {
			p = .9
		}
		a["skill:"+sk.ID] = systemone.Answer{Type: "noul", Noul: &p}
	}
	r := ComputeV3(s, nil, a, nil, time.Now(), false)
	if r.Partial || r.Score != nil || r.MustTotal != 0 || r.MustMet != 0 || len(r.Requirements) != 0 || len(r.Insights) != 0 {
		t.Fatalf("unexpected gaps, score or incomplete result: %#v", r)
	}
	if len(r.Selection) != 1 || r.Selection[0].EvidenceID != "job" || r.Selection[0].Relevance != .75 || strings.Join(r.Selection[0].HighlightIDs, ",") != "h2,h1" {
		t.Fatalf("selected experience or highlight ordering: %#v", r.Selection)
	}
	if got := r.Selection[0].HighlightScores; got["h1"] != .25 || got["h2"] != 1 || len(got) != 2 {
		t.Fatalf("individual scores lost before rendering: %v", got)
	}
	if len(r.Evidence) != 3 || r.Evidence[0].EvidenceID != "hidden" || len(r.Suggestions) != 1 || r.Suggestions[0].EvidenceID != "hidden" || r.Skills[0].SkillID != "go" {
		t.Fatalf("ranking: %#v", r)
	}
	delete(a, "highlight:h1")
	if r = ComputeV3(s, nil, a, nil, time.Now(), false); !r.Partial || r.Score != nil {
		t.Fatal("missing comparison must yield incomplete rankings without a score")
	}
	if _, known := r.Selection[0].HighlightScores["h1"]; known {
		t.Fatal("unanswered highlight must not become a known zero score")
	}
	s.Evidence[0].Highlights[1].AutoInclude = false
	r = ComputeV3(s, nil, a, nil, time.Now(), false)
	if r.Selection[0].HighlightScores["h2"] != 1 || strings.Contains(strings.Join(r.Selection[0].HighlightIDs, ","), "h2") {
		t.Fatal("disabled highlights retain their score for manual picks, without automatic inclusion")
	}
	if r = ComputeV3(Snapshot{}, nil, map[string]systemone.Answer{"seniority": choiceAnswer("not_specified", "not_specified", "mid")}, nil, time.Now(), false); r.Partial || len(r.Selection) != 0 {
		t.Fatal("empty library should complete with empty rankings")
	}
}
