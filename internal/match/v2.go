package match

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/diegohh0411/zapply/internal/decide"
	"github.com/diegohh0411/zapply/internal/model/systemone"
)

// Pipeline 2 asks a number of questions that grows with the posting plus the
// library, never their product. Round one (stage "read") classifies posting
// lines and rates every entry and highlight against the whole posting, as cvd
// did. Round two (stage "score") asks four questions per requirement against
// the whole library: which entry shows it, how strongly, which highlight cites
// it, and whether it is a degree or language requirement.
const (
	PipelineV1 = 1
	PipelineV2 = 2
	// DefaultPipeline is used for new runs unless one is requested.
	DefaultPipeline = PipelineV2
)

type pipeline struct {
	read         func(Snapshot) ([]decide.Pending, error)
	requirements func(Snapshot, map[string]systemone.Answer, ...map[string]string) ([]Requirement, bool)
	score        func(Snapshot, []Requirement) ([]decide.Pending, error)
	compute      func(Snapshot, []Requirement, map[string]systemone.Answer, map[string]string, time.Time, bool) Result
}

var pipelines = map[int]pipeline{
	PipelineV1: {ReadQuestions, Requirements, ScoreQuestions, Compute},
	PipelineV2: {ReadQuestionsV2, RequirementsV2, CoverQuestionsV2, ComputeV2},
}

func pipelineFor(version int) pipeline {
	if p, ok := pipelines[version]; ok {
		return p
	}
	return pipelines[PipelineV1]
}

const ScoringVersionV2 = 2

// Thresholds for reading choice distributions over library items.
const (
	coverFloor = .2  // least probability that attributes a requirement to an entry
	citeFloor  = .25 // least probability that cites a highlight
	maxCites   = 3
	// selectFloor matches cvd's relevance floor of 1.5 on its 0–4 scale.
	selectFloor = .375
)

var degreeLevels = map[string]string{"not_specified": "No minimum degree level is stated.", "secondary": "Secondary school completion is required.", "associate": "An associate degree is required.", "bachelor": "A bachelor degree is required.", "master": "A master degree is required.", "doctorate": "A doctoral degree is required."}

var classWeight = map[string]float64{"must": 2, "preferred": 1, "duty": 1, "trait": .5}

var relevanceCriteria = []string{
	"The content has nothing to do with the work or qualifications the posting asks for.",
	"The content touches the posting's field only tangentially.",
	"The content shows related work or skills, but not the core of what the posting asks for.",
	"The content shows work or skills that the posting asks for.",
	"The content directly shows the core work or qualifications that the posting asks for.",
}

type postingState struct {
	Title   string   `json:"title"`
	Company string   `json:"company"`
	Lines   []string `json:"lines"`
}
type libraryEntry struct {
	Kind         string             `json:"kind"`
	Category     string             `json:"category,omitempty"`
	Title        string             `json:"title"`
	Organization string             `json:"organization,omitempty"`
	StartDate    *string            `json:"start_date,omitempty"`
	EndDate      *string            `json:"end_date,omitempty"`
	IsCurrent    bool               `json:"is_current,omitempty"`
	Summary      string             `json:"summary,omitempty"`
	Details      map[string]any     `json:"details,omitempty"`
	Highlights   []libraryHighlight `json:"highlights,omitempty"`
	Skills       []string           `json:"skills,omitempty"`
}
type libraryHighlight struct {
	Text        string   `json:"text"`
	MetricValue *float64 `json:"metric_value,omitempty"`
	MetricUnit  string   `json:"metric_unit,omitempty"`
	Skills      []string `json:"skills,omitempty"`
}

func eligible(s Snapshot) []Evidence {
	out := make([]Evidence, 0, len(s.Evidence))
	for _, e := range s.Evidence {
		if e.Kind != "career_break" {
			out = append(out, e)
		}
	}
	return out
}
func posting(s Snapshot) postingState {
	lines := make([]string, len(s.Lines))
	for i, l := range s.Lines {
		lines[i] = l.Text
	}
	return postingState{s.Title, s.Company, lines}
}
func skillNames(skills []Skill) []string {
	out := make([]string, 0, len(skills))
	for _, sk := range skills {
		out = append(out, sk.Name)
	}
	return out
}

// libraryState holds claims only. Items are referred to by path, so ids,
// hashes and CV selection flags stay out of the model's input.
func libraryState(s Snapshot) []libraryEntry {
	out := []libraryEntry{}
	for _, e := range eligible(s) {
		entry := libraryEntry{Kind: e.Kind, Category: e.Category, Title: e.Title, StartDate: e.StartDate, EndDate: e.EndDate, IsCurrent: e.IsCurrent, Summary: e.Summary, Details: e.Details, Skills: skillNames(e.Skills)}
		if org, ok := e.Organization.(map[string]any); ok {
			entry.Organization, _ = org["name"].(string)
		}
		for _, h := range e.Highlights {
			entry.Highlights = append(entry.Highlights, libraryHighlight{h.Text, h.MetricValue, h.MetricUnit, skillNames(h.Skills)})
		}
		out = append(out, entry)
	}
	return out
}
func postingOnly(s Snapshot) []byte {
	return systemone.Raw(map[string]any{"posting": posting(s)})
}
func withLibrary(s Snapshot) []byte {
	return systemone.Raw(map[string]any{"posting": posting(s), "library": map[string]any{"evidence": libraryState(s)}})
}

func ReadQuestionsV2(s Snapshot) ([]decide.Pending, error) {
	p, l := postingOnly(s), withLibrary(s)
	result := []decide.Pending{}
	add := func(key string, state []byte, q systemone.Question) error {
		pending, e := decide.New(key, state, q)
		if e == nil {
			result = append(result, pending)
		}
		return e
	}
	for i := range s.Lines {
		path := fmt.Sprintf("`posting.lines[%d]`", i)
		if e := add(fmt.Sprintf("line:%d:class", i), p, choice("What is "+path+" in the context of `posting`?", map[string]string{
			"must":      "A qualification candidates must meet.",
			"preferred": "A preferred or bonus qualification.",
			"duty":      "A responsibility of the role.",
			"trait":     "A personal quality, attitude or working style the employer looks for, rather than a skill, credential or experience.",
			"other":     "Company information, benefits, process, a heading, or anything other than a qualification, duty or personal quality.",
		})); e != nil {
			return nil, e
		}
	}
	if e := add("seniority", p, choice("What seniority does `posting` require?", map[string]string{"not_specified": "No seniority level is stated.", "entry": "An entry level role requiring no prior professional experience.", "junior": "A junior role working under guidance.", "mid": "An independently contributing professional role.", "senior": "A senior role requiring substantial expertise.", "lead": "A lead or management role directing a team.", "executive": "An executive role setting organization strategy."})); e != nil {
		return nil, e
	}
	for _, sk := range s.Skills {
		if sk.Type == "language" {
			q := systemone.Question{Type: "choice", Instructions: systemone.Raw(map[string]string{"question": "What minimum proficiency in `language` does `posting` explicitly require?", "language": sk.Name}), Criteria: systemone.Raw(map[string]string{"not_specified": "No minimum proficiency in this language is stated.", "A1": "Beginner CEFR A1 proficiency is required.", "A2": "Elementary CEFR A2 proficiency is required.", "B1": "Intermediate CEFR B1 proficiency is required.", "B2": "Upper intermediate CEFR B2 proficiency is required.", "C1": "Advanced CEFR C1 proficiency is required.", "C2": "Proficient CEFR C2 proficiency is required.", "native": "Native proficiency is required."})}
			if e := add("language:"+sk.ID, p, q); e != nil {
				return nil, e
			}
		}
		q := systemone.Question{Type: "noul", Instructions: systemone.Raw(map[string]string{"question": "Does `posting` require or value `skill`?", "skill": sk.Name})}
		if e := add("skill:"+sk.ID, p, q); e != nil {
			return nil, e
		}
	}
	score := func(path string) systemone.Question {
		return systemone.Question{Type: "score", Instructions: systemone.Raw("How relevant is " + path + " to `posting`? Judge only the supplied content."), Criteria: systemone.Raw(relevanceCriteria)}
	}
	for i, e := range eligible(s) {
		if err := add("relevance:"+e.ID, l, score(fmt.Sprintf("`library.evidence[%d]`", i))); err != nil {
			return nil, err
		}
		for j, h := range e.Highlights {
			if err := add("highlight:"+h.ID, l, score(fmt.Sprintf("`library.evidence[%d].highlights[%d]`", i, j))); err != nil {
				return nil, err
			}
		}
	}
	return result, nil
}

// RequirementsV2 keeps posting lines classed as a qualification, duty or
// trait. Degree and language checks come from round two, not per line.
func RequirementsV2(s Snapshot, a map[string]systemone.Answer, corrections ...map[string]string) ([]Requirement, bool) {
	ignored := func(hash string) bool { return len(corrections) > 0 && corrections[0][hash] == "ignore" }
	requirements := []Requirement{}
	seen := map[string]bool{}
	incomplete := false
	for i, line := range s.Lines {
		norm := strings.ToLower(strings.Join(strings.Fields(line.Text), " "))
		h, _ := systemone.Hash(norm)
		c, ok := a[fmt.Sprintf("line:%d:class", i)]
		if strings.TrimSpace(s.Description) == "" && i == 0 {
			c.Choice, ok = "must", true
		}
		if !ok {
			if !ignored(h) {
				incomplete = true
			}
			continue
		}
		if c.Choice == "other" || seen[norm] {
			continue
		}
		seen[norm] = true
		requirements = append(requirements, Requirement{Text: line.Text, TextHash: h, Class: c.Choice, OriginalClass: c.Choice, Line: i, SourceStart: line.Start, SourceEnd: line.End, MinYears: YearsIn(line.Text)})
	}
	sort.SliceStable(requirements, func(i, j int) bool { return requirements[i].Class == "must" && requirements[j].Class != "must" })
	if len(requirements) > 40 {
		requirements = requirements[:40]
	}
	return requirements, incomplete
}

func evidenceOption(i int) string  { return fmt.Sprintf("e%d", i) }
func highlightOption(i int) string { return fmt.Sprintf("h%d", i) }

func CoverQuestionsV2(s Snapshot, requirements []Requirement) ([]decide.Pending, error) {
	p, l := postingOnly(s), withLibrary(s)
	items := eligible(s)
	entries := map[string]string{"none": "No entry in `library.evidence` demonstrates it."}
	highlights := map[string]string{"none": "No highlight in `library.evidence` demonstrates it."}
	n := 0
	for i, e := range items {
		entries[evidenceOption(i)] = fmt.Sprintf("`library.evidence[%d]` demonstrates it best.", i)
		for j := range e.Highlights {
			highlights[highlightOption(n)] = fmt.Sprintf("`library.evidence[%d].highlights[%d]` demonstrates it best.", i, j)
			n++
		}
	}
	result := []decide.Pending{}
	add := func(key string, state []byte, q systemone.Question) error {
		pending, e := decide.New(key, state, q)
		if e == nil {
			result = append(result, pending)
		}
		return e
	}
	for _, r := range requirements {
		path := fmt.Sprintf("`posting.lines[%d]`", r.Line)
		if e := add("strength:"+r.TextHash, l, systemone.Question{Type: "score", Instructions: systemone.Raw("How strongly does `library` demonstrate the requirement in " + path + "? Judge only the supplied content."), Criteria: systemone.Raw(strengthCriteria)}); e != nil {
			return nil, e
		}
		if len(items) > 0 {
			if e := add("cover:"+r.TextHash, l, choice("Which entry in `library.evidence` best demonstrates the requirement in "+path+"?", entries)); e != nil {
				return nil, e
			}
		}
		if n > 0 {
			if e := add("cite:"+r.TextHash, l, choice("Which highlight in `library.evidence` best demonstrates the requirement in "+path+"?", highlights)); e != nil {
				return nil, e
			}
		}
		// Each line states its own level: "bachelor required" and "master preferred" differ.
		if e := add("degree:"+r.TextHash, p, choice("What minimum completed degree level does "+path+" explicitly require?", degreeLevels)); e != nil {
			return nil, e
		}
		if e := add("check:"+r.TextHash, p, choice("What does "+path+" ask for?", map[string]string{"degree": "A degree or other formal education.", "language": "Ability in a human language.", "other": "Anything other than a degree, formal education or human language ability."})); e != nil {
			return nil, e
		}
	}
	return result, nil
}

// expected maps a score answer to [0, 1] by its expected level.
func expected(a systemone.Answer) float64 {
	n := len(a.Probabilities)
	if n < 2 {
		return 0
	}
	sum := 0.
	for i := range n {
		sum += float64(i) * a.Probabilities[fmt.Sprint(i)]
	}
	return sum / float64(n-1)
}

func ComputeV2(s Snapshot, requirements []Requirement, a map[string]systemone.Answer, overrides map[string]string, asOf time.Time, readIncomplete bool) Result {
	s.Evidence = eligible(s)
	out := Result{ScoringVersion: ScoringVersionV2, Partial: readIncomplete, Requirements: []Coverage{}, Evidence: []Selection{}, Selection: []Selection{}, Suggestions: []Selection{}, Skills: []RankedSkill{}, Insights: []Insight{}}
	out.Seniority = a["seniority"].Choice
	if out.Seniority == "" {
		out.Partial = true
	}
	flat := []Highlight{}
	relevance := map[string]float64{}
	hrelevance := map[string]float64{}
	for _, e := range s.Evidence {
		if ans, ok := a["relevance:"+e.ID]; ok {
			relevance[e.ID] = expected(ans)
		} else {
			out.Partial = true
		}
		for _, h := range e.Highlights {
			flat = append(flat, h)
			if ans, ok := a["highlight:"+h.ID]; ok {
				hrelevance[h.ID] = expected(ans)
			} else {
				out.Partial = true
			}
		}
	}
	ranks := map[string]float64{}
	hranks := map[string]float64{}
	covered := map[string]float64{}
	num, denom := 0., 0.
	for _, original := range requirements {
		r := original
		if c, ok := overrides[r.TextHash]; ok {
			r.Class = c
		}
		if r.Class == "ignore" {
			out.Requirements = append(out.Requirements, Coverage{Requirement: r, Status: "ignored", HighlightIDs: []string{}, Citations: []Citation{}})
			continue
		}
		w := classWeight[r.Class]
		if w == 0 {
			w = 1
		}
		if r.Class == "must" {
			out.MustTotal++
		}
		denom += w
		c := Coverage{Requirement: r, Status: "gap", HighlightIDs: []string{}, Citations: []Citation{}}
		strength, ok := a["strength:"+r.TextHash]
		if ok {
			c.Strength = Strength(strength)
			copy := strength
			c.Answer = &copy
			maxp := 0.
			for _, p := range strength.Probabilities {
				maxp = math.Max(maxp, p)
			}
			c.Review = maxp <= .5
		} else {
			c.Provisional = true
			out.Partial = true
		}
		// The entry distribution attributes the requirement and gives every
		// entry a soft share of it, without asking entry × requirement.
		var best *Evidence
		eligibleYears := []Evidence{}
		cover, coverOK := a["cover:"+r.TextHash]
		if len(s.Evidence) > 0 && !coverOK {
			c.Provisional = true
			out.Partial = true
		}
		bestP := 0.
		for i, e := range s.Evidence {
			p := cover.Probabilities[evidenceOption(i)]
			ranks[e.ID] += w * p * c.Strength
			if p >= coverFloor {
				eligibleYears = append(eligibleYears, e)
				if p > bestP {
					best, bestP = &s.Evidence[i], p
				}
			}
		}
		check, checkOK := a["check:"+r.TextHash]
		if !checkOK {
			out.Partial = true
		}
		capped := false
		if r.MinYears > 0 {
			years := Years(eligibleYears, asOf)
			c.Check = fmt.Sprintf("%.1f of %.1f years", years, r.MinYears)
			capped = years < r.MinYears
		}
		switch check.Choice {
		case "degree":
			r.Type = "education"
			degree, ok := a["degree:"+r.TextHash]
			if !ok {
				out.Partial = true
			}
			r.Degree = degree.Choice
			if r.Degree != "" && r.Degree != "not_specified" {
				c.Check = "Required degree: " + r.Degree
				// The best completed degree at the required level satisfies the check.
				var match *Evidence
				matchP := -1.
				for i, e := range s.Evidence {
					d, _ := e.Details["degree_type"].(string)
					status, _ := e.Details["status"].(string)
					if p := cover.Probabilities[evidenceOption(i)]; status == "completed" && level(d, degrees) >= level(r.Degree, degrees) && p > matchP {
						match, matchP = &s.Evidence[i], p
					}
				}
				if match != nil {
					best = match
				} else {
					capped = true
				}
				if best != nil {
					d, _ := best.Details["degree_type"].(string)
					status, _ := best.Details["status"].(string)
					if d != "" {
						c.Check += "; " + d + " " + strings.ReplaceAll(status, "_", " ")
					}
				}
			}
		case "language":
			r.Type = "language"
			// Every language the line names must meet its level ("C1 English and C1 Spanish").
			required, met, named := "", true, false
			for _, sk := range s.Skills {
				if sk.Type != "language" || !strings.Contains(strings.ToLower(r.Text), strings.ToLower(sk.Name)) {
					continue
				}
				want, ok := a["language:"+sk.ID]
				if !ok {
					out.Partial = true
					continue
				}
				if want.Choice == "not_specified" {
					continue
				}
				named = true
				if level(want.Choice, languages) > level(required, languages) {
					required = want.Choice
				}
				if level(sk.Proficiency, languages) < level(want.Choice, languages) {
					met = false
				}
			}
			if named {
				r.Language = required
				c.Check = "Required language level: " + required
				for _, sk := range s.Skills {
					if sk.Type == "language" && strings.Contains(strings.ToLower(r.Text), strings.ToLower(sk.Name)) {
						c.Check += "; " + sk.Name + " " + sk.Proficiency
					}
				}
				capped = capped || !met
			}
		case "other":
			r.Type = "other"
		}
		c.Requirement = r
		if capped {
			c.Strength = math.Min(.6, c.Strength)
		}
		recency := 1.
		if best != nil {
			c.EvidenceID, c.EvidenceTitle = best.ID, best.Title
			covered[best.ID] = math.Max(covered[best.ID], c.Strength)
			recency = Recency(*best, asOf)
		}
		switch {
		case c.Strength >= .8:
			c.Status = "met"
		case c.Strength >= .5:
			c.Status = "partial"
		case c.Strength >= .2:
			c.Status = "adjacent"
		}
		if c.Provisional && c.Status == "gap" {
			c.Status = "unknown"
		}
		if r.Class == "must" && (c.Status == "met" || c.Status == "partial") {
			out.MustMet++
		}
		if cite, ok := a["cite:"+r.TextHash]; ok {
			type candidate struct {
				h Highlight
				p float64
			}
			candidates := []candidate{}
			for i, h := range flat {
				p := cite.Probabilities[highlightOption(i)]
				hranks[h.ID] += w * p * c.Strength
				if p >= citeFloor {
					candidates = append(candidates, candidate{h, p})
				}
			}
			sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].p > candidates[j].p })
			if c.Strength >= .4 {
				for _, h := range candidates[:min(maxCites, len(candidates))] {
					c.HighlightIDs = append(c.HighlightIDs, h.h.ID)
					c.Citations = append(c.Citations, Citation{h.h.ID, h.h.Text})
				}
			}
		} else if len(flat) > 0 {
			out.Partial = true
		}
		if c.Status == "gap" {
			out.Insights = append(out.Insights, Insight{Type: "gap", TextHash: r.TextHash, Text: r.Text})
			out.Insights = append(out.Insights, Insight{Type: "library_prompt", TextHash: r.TextHash, Text: "Add evidence or a highlight for this requirement."})
		} else if c.Status == "adjacent" || c.Check != "" && c.Strength < .8 {
			out.Insights = append(out.Insights, Insight{Type: "near_miss", TextHash: r.TextHash, Text: c.Check})
		}
		out.Requirements = append(out.Requirements, c)
		num += w * c.Strength * recency
	}
	relevant := []Evidence{}
	for _, e := range s.Evidence {
		selection := Selection{EvidenceID: e.ID, Relevance: Recency(e, asOf) * (relevance[e.ID] + ranks[e.ID]), HighlightIDs: []string{}, Title: e.Title}
		highlights := append([]Highlight{}, e.Highlights...)
		sort.SliceStable(highlights, func(i, j int) bool {
			return hrelevance[highlights[i].ID]+hranks[highlights[i].ID] > hrelevance[highlights[j].ID]+hranks[highlights[j].ID]
		})
		for _, h := range highlights {
			if h.AutoInclude {
				selection.HighlightIDs = append(selection.HighlightIDs, h.ID)
			}
		}
		out.Evidence = append(out.Evidence, selection)
		if relevance[e.ID] >= .5 || covered[e.ID] >= .5 {
			relevant = append(relevant, e)
		}
		if relevance[e.ID] >= selectFloor || covered[e.ID] >= .4 {
			if e.AutoInclude {
				out.Selection = append(out.Selection, selection)
			} else {
				out.Suggestions = append(out.Suggestions, selection)
				out.Insights = append(out.Insights, Insight{Type: "hidden_evidence", Text: e.Title, EvidenceID: e.ID})
			}
		}
	}
	for _, list := range [][]Selection{out.Evidence, out.Selection, out.Suggestions} {
		sort.SliceStable(list, func(i, j int) bool { return list[i].Relevance > list[j].Relevance })
	}
	for _, sk := range s.Skills {
		ans, ok := a["skill:"+sk.ID]
		if !ok || ans.Noul == nil {
			out.Partial = true
			continue
		}
		rank := *ans.Noul * 2
		for _, e := range s.Evidence {
			for _, linked := range e.Skills {
				if linked.ID == sk.ID {
					rank += (relevance[e.ID] + ranks[e.ID]) / float64(max(1, len(e.Skills)))
				}
			}
		}
		out.Skills = append(out.Skills, RankedSkill{SkillID: sk.ID, Name: sk.Name, Relevance: rank})
	}
	sort.SliceStable(out.Skills, func(i, j int) bool { return out.Skills[i].Relevance > out.Skills[j].Relevance })
	out.RelevantYears = Years(relevant, asOf)
	sort.SliceStable(out.Insights, func(i, j int) bool { return out.Insights[i].Type == "gap" && out.Insights[j].Type != "gap" })
	if denom > 0 && !out.Partial {
		score := 100 * num / denom
		out.Score = &score
	}
	return out
}
