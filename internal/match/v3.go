package match

import (
	"sort"
	"strings"
	"time"

	"github.com/diegohh0411/zapply/internal/decide"
	"github.com/diegohh0411/zapply/internal/model/systemone"
)

// Pipeline 3 follows CVD: the full posting is shared state and each question
// carries only the entry, highlight or skill being judged. There is no
// requirement extraction, coverage round or aggregate fit score.
func ReadQuestionsV3(s Snapshot) ([]decide.Pending, error) {
	state := systemone.Raw(map[string]any{"posting": map[string]string{
		"title": s.Title, "company": s.Company, "description": s.Description,
	}})
	result := []decide.Pending{}
	add := func(key string, q systemone.Question) error {
		pending, err := decide.New(key, state, q)
		if err == nil {
			result = append(result, pending)
		}
		return err
	}
	if err := add("seniority", choice("What seniority does `posting` require?", map[string]string{
		"not_specified": "No seniority level is stated.", "entry": "An entry level role requiring no prior professional experience.",
		"junior": "A junior role working under guidance.", "mid": "An independently contributing professional role.",
		"senior": "A senior role requiring substantial expertise.", "lead": "A lead or management role directing a team.",
		"executive": "An executive role setting organization strategy.",
	})); err != nil {
		return nil, err
	}
	for _, sk := range s.Skills {
		if err := add("skill:"+sk.ID, systemone.Question{Type: "noul", Instructions: systemone.Raw(map[string]any{
			"question": "Does `posting` require or value `skill`?", "skill": sk.Name,
		})}); err != nil {
			return nil, err
		}
	}
	entries := libraryState(s)
	for i, e := range eligible(s) {
		if err := add("relevance:"+e.ID, systemone.Question{Type: "score", Criteria: systemone.Raw(relevanceCriteria), Instructions: systemone.Raw(map[string]any{
			"question": "How relevant is `entry` to `posting`? Judge only the supplied content.", "entry": entries[i],
		})}); err != nil {
			return nil, err
		}
		for j, h := range e.Highlights {
			if err := add("highlight:"+h.ID, systemone.Question{Type: "score", Criteria: systemone.Raw(relevanceCriteria), Instructions: systemone.Raw(map[string]any{
				"question": "How strongly does `highlight` from `entry` support a fit for `posting`? Judge only the supplied content.",
				"entry":    map[string]string{"title": entries[i].Title, "organization": entries[i].Organization}, "highlight": entries[i].Highlights[j],
			})}); err != nil {
				return nil, err
			}
		}
	}
	return result, nil
}

func noRequirements(Snapshot, map[string]systemone.Answer, ...map[string]string) ([]Requirement, bool) {
	return []Requirement{}, false
}

func noCoverageQuestions(Snapshot, []Requirement) ([]decide.Pending, error) {
	return []decide.Pending{}, nil
}

func ComputeV3(s Snapshot, _ []Requirement, answers map[string]systemone.Answer, _ map[string]string, asOf time.Time, _ bool) Result {
	out := emptyResult()
	out.ScoringVersion = PipelineV3
	out.Seniority = answers["seniority"].Choice
	out.Partial = out.Seniority == ""
	score := func(key string) float64 {
		a, ok := answers[key]
		if !ok {
			out.Partial = true
		}
		return expected(a)
	}
	relevant := []Evidence{}
	for _, e := range eligible(s) {
		relevance := score("relevance:" + e.ID)
		selection := Selection{EvidenceID: e.ID, Title: e.Title, Relevance: relevance, HighlightIDs: []string{}}
		highlights := append([]Highlight{}, e.Highlights...)
		highlightScores := map[string]float64{}
		for _, h := range highlights {
			highlightScores[h.ID] = score("highlight:" + h.ID)
		}
		sort.SliceStable(highlights, func(i, j int) bool { return highlightScores[highlights[i].ID] > highlightScores[highlights[j].ID] })
		for _, h := range highlights {
			if h.AutoInclude {
				selection.HighlightIDs = append(selection.HighlightIDs, h.ID)
			}
		}
		out.Evidence = append(out.Evidence, selection)
		if relevance >= .5 {
			relevant = append(relevant, e)
		}
		if relevance >= selectFloor {
			if e.AutoInclude {
				out.Selection = append(out.Selection, selection)
			} else {
				out.Suggestions = append(out.Suggestions, selection)
			}
		}
	}
	for _, list := range [][]Selection{out.Evidence, out.Selection, out.Suggestions} {
		sort.SliceStable(list, func(i, j int) bool { return list[i].Relevance > list[j].Relevance })
	}
	for _, sk := range s.Skills {
		a, ok := answers["skill:"+sk.ID]
		if !ok || a.Noul == nil {
			out.Partial = true
			continue
		}
		out.Skills = append(out.Skills, RankedSkill{SkillID: sk.ID, Name: sk.Name, Relevance: *a.Noul})
	}
	sort.SliceStable(out.Skills, func(i, j int) bool { return out.Skills[i].Relevance > out.Skills[j].Relevance })
	out.RelevantYears = Years(relevant, asOf)
	return out
}

// Only historical pipelines need source chunks. New snapshots keep the posting
// intact; no line-count truncation applies to whole-posting matching.
func legacyLines(s Snapshot) Snapshot {
	text := s.Description
	if strings.TrimSpace(text) == "" {
		text = s.Title
	}
	s.Lines, s.Truncated = SplitLines(text)
	return s
}
