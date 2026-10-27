// Package match reads postings using typed decisions and computes deterministic
// coverage. It never changes library content.
package match

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/diegohh0411/zapply/internal/decide"
	"github.com/diegohh0411/zapply/internal/model/systemone"
)

const ScoringVersion = 1

type Line struct {
	Text  string `json:"text"`
	Start int    `json:"start"`
	End   int    `json:"end"`
}
type Skill struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Type        string `json:"type"`
	Proficiency string `json:"proficiency"`
}
type Highlight struct {
	ID          string   `json:"id"`
	Text        string   `json:"text"`
	MetricValue *float64 `json:"metric_value"`
	MetricUnit  string   `json:"metric_unit"`
	AutoInclude bool     `json:"auto_include"`
	Skills      []Skill  `json:"skills"`
}
type Evidence struct {
	ID           string         `json:"id"`
	Kind         string         `json:"kind"`
	Category     string         `json:"category"`
	Title        string         `json:"title"`
	Summary      string         `json:"summary"`
	Organization any            `json:"organization"`
	StartDate    *string        `json:"start_date"`
	EndDate      *string        `json:"end_date"`
	IsCurrent    bool           `json:"is_current"`
	AutoInclude  bool           `json:"auto_include"`
	ContentHash  string         `json:"content_hash"`
	Details      map[string]any `json:"details"`
	Highlights   []Highlight    `json:"highlights"`
	Skills       []Skill        `json:"skills"`
}
type Snapshot struct {
	Title       string     `json:"title"`
	Company     string     `json:"company"`
	Description string     `json:"description"`
	Lines       []Line     `json:"lines"`
	Truncated   bool       `json:"truncated"`
	Evidence    []Evidence `json:"evidence"`
	Skills      []Skill    `json:"skills"`
}
type Requirement struct {
	Text          string  `json:"text"`
	TextHash      string  `json:"text_hash"`
	Class         string  `json:"class"`
	OriginalClass string  `json:"original_class"`
	Type          string  `json:"type"`
	Line          int     `json:"line"`
	SourceStart   int     `json:"source_start"`
	SourceEnd     int     `json:"source_end"`
	MinYears      float64 `json:"min_years"`
	Degree        string  `json:"degree"`
	Language      string  `json:"language"`
}

var bullet = regexp.MustCompile(`(?m)(?:^[\t ]*(?:[-*•▪◦‣]|[0-9]+[.)])[\t ]+|[\t ]+[•▪◦‣][\t ]+)`)

// SplitLines retains exact source slices and byte offsets, including short
// qualifications. Paragraphs split at sentence boundaries and UTF-8 boundaries.
func SplitLines(text string) ([]Line, bool) {
	lines := []Line{}
	bounds := [][2]int{}
	start := 0
	for i, r := range text {
		if r == '\n' || r == '\r' {
			bounds = append(bounds, [2]int{start, i})
			start = i + utf8.RuneLen(r)
		}
	}
	bounds = append(bounds, [2]int{start, len(text)})
	add := func(a, b int) {
		for a < b {
			r, n := utf8.DecodeRuneInString(text[a:b])
			if !unicode.IsSpace(r) {
				break
			}
			a += n
		}
		for b > a {
			r, n := utf8.DecodeLastRuneInString(text[a:b])
			if !unicode.IsSpace(r) {
				break
			}
			b -= n
		}
		if a < b {
			lines = append(lines, Line{text[a:b], a, b})
		}
	}
	for _, span := range bounds {
		a, b := span[0], span[1]
		matches := bullet.FindAllStringIndex(text[a:b], -1)
		parts := [][2]int{}
		p := a
		for _, m := range matches {
			if a+m[0] > p {
				parts = append(parts, [2]int{p, a + m[0]})
			}
			p = a + m[1]
		}
		parts = append(parts, [2]int{p, b})
		for _, part := range parts {
			a, b = part[0], part[1]
			for utf8.RuneCountInString(text[a:b]) > 400 {
				end := a
				count := 0
				sentence := 0
				for end < b && count < 400 {
					r, n := utf8.DecodeRuneInString(text[end:b])
					end += n
					count++
					if (r == '.' || r == '!' || r == '?') && end < b && unicode.IsSpace(rune(text[end])) {
						sentence = end
					}
				}
				if sentence > a {
					end = sentence
				}
				add(a, end)
				a = end
			}
			add(a, b)
		}
	}
	truncated := len(lines) > 120
	if truncated {
		lines = lines[:120]
	}
	return lines, truncated
}

var yearsPattern = regexp.MustCompile(`(?i)(?:at least\s+)?(\d+(?:\.\d+)?|one|two|three|four|five|six|seven|eight|nine|ten)(?:\s*[–—-]\s*\d+)?\s*\+?\s*(?:years?|yrs?)\b`)

func YearsIn(s string) float64 {
	m := yearsPattern.FindStringSubmatch(s)
	if len(m) == 0 {
		return 0
	}
	words := map[string]float64{"one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "six": 6, "seven": 7, "eight": 8, "nine": 9, "ten": 10}
	if n, ok := words[strings.ToLower(m[1])]; ok {
		return n
	}
	n, _ := strconv.ParseFloat(m[1], 64)
	return n
}
func choice(question string, options map[string]string) systemone.Question {
	return systemone.Question{Type: "choice", Instructions: systemone.Raw(question), Criteria: systemone.Raw(options)}
}
func ReadQuestions(s Snapshot) ([]decide.Pending, error) {
	state := systemone.Raw(map[string]any{"posting": map[string]any{"title": s.Title, "company": s.Company, "description": s.Description, "lines": s.Lines}})
	result := []decide.Pending{}
	add := func(key string, q systemone.Question) error {
		p, e := decide.New(key, state, q)
		if e == nil {
			result = append(result, p)
		}
		return e
	}
	for i := range s.Lines {
		path := fmt.Sprintf("`posting.lines[%d].text`", i)
		for _, v := range []struct {
			key, question string
			options       map[string]string
		}{
			{"class", "What is " + path + " in the context of the posting?", map[string]string{"must": "A qualification candidates must meet.", "preferred": "A preferred or bonus qualification.", "duty": "A responsibility of the role.", "other": "Company information, benefits, process, a heading, or anything other than a qualification or duty."}},
			{"type", "What qualification or work does " + path + " ask for?", map[string]string{"skill": "A practical capability or technology.", "experience": "Prior work or years of experience.", "education": "A degree or formal education.", "credential": "A professional license, certification or clearance.", "language": "Ability in a human language.", "other": "Anything other than skills, prior work, degrees, credentials or human languages."}},
			{"degree", "What minimum completed degree level is explicitly required by " + path + "?", map[string]string{"not_specified": "No minimum degree level is stated.", "secondary": "Secondary school completion is required.", "associate": "An associate degree is required.", "bachelor": "A bachelor degree is required.", "master": "A master degree is required.", "doctorate": "A doctoral degree is required."}},
			{"language", "What minimum human language proficiency is explicitly required by " + path + "?", map[string]string{"not_specified": "No minimum language proficiency is stated.", "A1": "Beginner CEFR A1 proficiency is required.", "A2": "Elementary CEFR A2 proficiency is required.", "B1": "Intermediate CEFR B1 proficiency is required.", "B2": "Upper intermediate CEFR B2 proficiency is required.", "C1": "Advanced CEFR C1 proficiency is required.", "C2": "Proficient CEFR C2 proficiency is required.", "native": "Native language proficiency is required."}},
		} {
			if e := add(fmt.Sprintf("line:%d:%s", i, v.key), choice(v.question, v.options)); e != nil {
				return nil, e
			}
		}
	}
	if e := add("seniority", choice("What seniority does `posting` require?", map[string]string{"not_specified": "No seniority level is stated.", "entry": "An entry level role requiring no prior professional experience.", "junior": "A junior role working under guidance.", "mid": "An independently contributing professional role.", "senior": "A senior role requiring substantial expertise.", "lead": "A lead or management role directing a team.", "executive": "An executive role setting organization strategy."})); e != nil {
		return nil, e
	}
	for i := range s.Skills {
		q := systemone.Question{Type: "noul", Instructions: systemone.Raw(map[string]string{"question": "Does `posting` require or value `skill`?", "skill": s.Skills[i].Name})}
		if e := add("skill:"+s.Skills[i].ID, q); e != nil {
			return nil, e
		}
	}
	return result, nil
}
func Requirements(s Snapshot, a map[string]systemone.Answer, corrections ...map[string]string) ([]Requirement, bool) {
	ignored := func(hash string) bool { return len(corrections) > 0 && corrections[0][hash] == "ignore" }
	requirements := []Requirement{}
	seen := map[string]bool{}
	incomplete := false
	missing := map[string]bool{}
	for i, line := range s.Lines {
		prefix := fmt.Sprintf("line:%d:", i)
		c, ok := a[prefix+"class"]
		if strings.TrimSpace(s.Description) == "" && i == 0 {
			c.Choice = "must"
			ok = true
		}
		if !ok {
			hash, _ := systemone.Hash(strings.ToLower(strings.Join(strings.Fields(line.Text), " ")))
			if ignored(hash) {
				continue
			}
			incomplete = true
			continue
		}
		if c.Choice == "other" {
			continue
		}
		norm := strings.ToLower(strings.Join(strings.Fields(line.Text), " "))
		if seen[norm] {
			continue
		}
		seen[norm] = true
		h, _ := systemone.Hash(norm)
		r := Requirement{Text: line.Text, TextHash: h, Class: c.Choice, OriginalClass: c.Choice, Line: i, SourceStart: line.Start, SourceEnd: line.End, MinYears: YearsIn(line.Text)}
		t, ok := a[prefix+"type"]
		if !ok {
			missing[h] = true
		}
		r.Type = t.Choice
		if r.Type == "education" {
			x, ok := a[prefix+"degree"]
			if !ok {
				missing[h] = true
			}
			r.Degree = x.Choice
		}
		if r.Type == "language" {
			x, ok := a[prefix+"language"]
			if !ok {
				missing[h] = true
			}
			r.Language = x.Choice
		}
		requirements = append(requirements, r)
	}
	sort.SliceStable(requirements, func(i, j int) bool { return requirements[i].Class == "must" && requirements[j].Class != "must" })
	if len(requirements) > 40 {
		requirements = requirements[:40]
	}
	for _, r := range requirements {
		if missing[r.TextHash] && !ignored(r.TextHash) {
			incomplete = true
		}
	}
	return requirements, incomplete
}

var strengthCriteria = []string{
	"The supplied content provides no evidence of the qualification or work described in the requirement.",
	"The supplied content concerns a related area but provides no concrete example of doing the work or demonstrating the qualification described in the requirement.",
	"The supplied content shows concrete comparable work or capability that could support the requirement, without explicitly demonstrating the specific qualification or work it describes.",
	"The supplied content explicitly demonstrates the specific qualification or work described in the requirement.",
}

func Pair(evidence, requirement string) string { return "evidence:" + evidence + ":" + requirement }
func HighlightPair(highlight, requirement string) string {
	return "highlight:" + highlight + ":" + requirement
}
func ScoreQuestions(s Snapshot, requirements []Requirement) ([]decide.Pending, error) {
	result := []decide.Pending{}
	for _, e := range s.Evidence {
		if e.Kind == "career_break" {
			continue
		}
		state := systemone.Raw(map[string]any{"job_context": map[string]string{"title": s.Title, "company": s.Company}, "evidence": scoringEvidence(e)})
		for _, r := range requirements {
			subjects := []struct{ key, path string }{{Pair(e.ID, r.TextHash), "evidence"}}
			for i, h := range e.Highlights {
				subjects = append(subjects, struct{ key, path string }{HighlightPair(h.ID, r.TextHash), fmt.Sprintf("evidence.highlights[%d]", i)})
			}
			for _, subject := range subjects {
				q := systemone.Question{Type: "score", Instructions: systemone.Raw(map[string]string{"question": "How strongly does `" + subject.path + "` demonstrate `requirement`? Judge only the supplied content.", "requirement": r.Text}), Criteria: systemone.Raw(strengthCriteria)}
				p, err := decide.New(subject.key, state, q)
				if err != nil {
					return nil, err
				}
				result = append(result, p)
			}
		}
	}
	return result, nil
}
func Strength(a systemone.Answer) float64 {
	return math.Min(1, .25*a.Probabilities["1"]+.6*a.Probabilities["2"]+a.Probabilities["3"])
}
func parsedDate(s *string) time.Time {
	if s == nil {
		return time.Time{}
	}
	t, _ := time.Parse("2006-01-02", *s)
	return t
}
func Recency(e Evidence, asOf time.Time) float64 {
	if e.IsCurrent || e.EndDate == nil {
		return 1
	}
	age := asOf.Sub(parsedDate(e.EndDate)).Hours() / 24 / 365.2425
	return math.Max(.5, 1-.1*math.Max(0, age-3))
}

// Years returns the union of date ranges; overlap never counts twice.
func Years(evidence []Evidence, asOf time.Time) float64 {
	type interval struct{ start, end time.Time }
	ranges := []interval{}
	for _, e := range evidence {
		start := parsedDate(e.StartDate)
		end := parsedDate(e.EndDate)
		if e.IsCurrent {
			end = asOf
		}
		if start.IsZero() || end.IsZero() || !end.After(start) {
			continue
		}
		if end.After(asOf) {
			end = asOf
		}
		if !end.After(start) {
			continue
		}
		ranges = append(ranges, interval{start, end})
	}
	sort.Slice(ranges, func(i, j int) bool { return ranges[i].start.Before(ranges[j].start) })
	var total time.Duration
	var last interval
	for i, r := range ranges {
		if i == 0 {
			last = r
			continue
		}
		if !r.start.After(last.end) {
			if r.end.After(last.end) {
				last.end = r.end
			}
		} else {
			total += last.end.Sub(last.start)
			last = r
		}
	}
	if len(ranges) > 0 {
		total += last.end.Sub(last.start)
	}
	return total.Hours() / 24 / 365.2425
}

type Citation struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

type Coverage struct {
	Citations []Citation `json:"citations"`
	Requirement
	Status        string            `json:"status"`
	Strength      float64           `json:"strength"`
	Provisional   bool              `json:"provisional"`
	EvidenceID    string            `json:"evidence_id,omitempty"`
	EvidenceTitle string            `json:"evidence_title,omitempty"`
	HighlightIDs  []string          `json:"highlight_ids"`
	Answer        *systemone.Answer `json:"answer,omitempty"`
	Check         string            `json:"check,omitempty"`
	Review        bool              `json:"review"`
}
type Selection struct {
	EvidenceID   string   `json:"evidence_id"`
	Relevance    float64  `json:"relevance"`
	HighlightIDs []string `json:"highlight_ids"`
	Title        string   `json:"title"`
}
type RankedSkill struct {
	SkillID   string  `json:"skill_id"`
	Name      string  `json:"name"`
	Relevance float64 `json:"relevance"`
}
type Insight struct {
	Type       string `json:"type"`
	TextHash   string `json:"text_hash,omitempty"`
	Text       string `json:"text"`
	EvidenceID string `json:"evidence_id,omitempty"`
}
type Result struct {
	Score          *float64      `json:"score"`
	MustMet        int           `json:"must_met"`
	MustTotal      int           `json:"must_total"`
	Partial        bool          `json:"partial"`
	Seniority      string        `json:"seniority"`
	RelevantYears  float64       `json:"relevant_years"`
	ScoringVersion int           `json:"scoring_version"`
	Requirements   []Coverage    `json:"requirements"`
	Evidence       []Selection   `json:"evidence"`
	Selection      []Selection   `json:"selection"`
	Suggestions    []Selection   `json:"suggestions"`
	Skills         []RankedSkill `json:"skills"`
	Insights       []Insight     `json:"insights"`
}

var degrees = []string{"not_specified", "secondary", "associate", "bachelor", "master", "doctorate"}
var languages = []string{"not_specified", "A1", "A2", "B1", "B2", "C1", "C2", "native"}

func level(v string, levels []string) int {
	for i, x := range levels {
		if x == v {
			return i
		}
	}
	return 0
}
func Compute(s Snapshot, requirements []Requirement, a map[string]systemone.Answer, overrides map[string]string, asOf time.Time, readIncomplete bool) Result {
	eligibleEvidence := make([]Evidence, 0, len(s.Evidence))
	for _, item := range s.Evidence {
		if item.Kind != "career_break" {
			eligibleEvidence = append(eligibleEvidence, item)
		}
	}
	s.Evidence = eligibleEvidence
	out := Result{ScoringVersion: ScoringVersion, Partial: readIncomplete, Requirements: []Coverage{}, Evidence: []Selection{}, Selection: []Selection{}, Suggestions: []Selection{}, Skills: []RankedSkill{}, Insights: []Insight{}}
	out.Seniority = a["seniority"].Choice
	if out.Seniority == "" {
		out.Partial = true
	}
	ranks := map[string]float64{}
	bests := map[string]float64{}
	hranks := map[string]float64{}
	hknown := map[string]bool{}
	relevant := map[string]bool{}
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
		w := 1.
		if r.Class == "must" {
			w = 2
			out.MustTotal++
		}
		denom += w
		c := Coverage{Requirement: r, Status: "gap", HighlightIDs: []string{}, Citations: []Citation{}}
		bestWeighted := 0.
		eligible := []Evidence{}
		for _, e := range s.Evidence {
			answer, ok := a[Pair(e.ID, r.TextHash)]
			if !ok {
				c.Provisional = true
				out.Partial = true
				continue
			}
			strength := Strength(answer)
			if answer.Probabilities["2"]+answer.Probabilities["3"] >= .5 {
				eligible = append(eligible, e)
				relevant[e.ID] = true
			}
			ranks[e.ID] += strength * Recency(e, asOf) * w
			bests[e.ID] = math.Max(bests[e.ID], strength)
			if strength >= .4 {
				for _, h := range e.Highlights {
					ha, ok := a[HighlightPair(h.ID, r.TextHash)]
					if !ok {
						out.Partial = true
						continue
					}
					hranks[h.ID] += Strength(ha) * w
					hknown[h.ID] = true
				}
			}
			checked := strength
			if r.Degree != "" && r.Degree != "not_specified" {
				degree, _ := e.Details["degree_type"].(string)
				status, _ := e.Details["status"].(string)
				if status != "completed" || level(degree, degrees) < level(r.Degree, degrees) {
					checked = math.Min(.6, checked)
				}
			}
			if r.Language != "" && r.Language != "not_specified" {
				met := false
				for _, sk := range s.Skills {
					if sk.Type == "language" && strings.Contains(strings.ToLower(r.Text), strings.ToLower(sk.Name)) && level(sk.Proficiency, languages) >= level(r.Language, languages) {
						met = true
					}
				}
				if !met {
					checked = math.Min(.6, checked)
				}
			}
			if checked > c.Strength || c.Answer == nil {
				c.Strength = checked
				c.EvidenceID = e.ID
				c.EvidenceTitle = e.Title
				copy := answer
				c.Answer = &copy
			}
			bestWeighted = math.Max(bestWeighted, checked*Recency(e, asOf))
		}
		if r.MinYears > 0 {
			years := Years(eligible, asOf)
			c.Check = fmt.Sprintf("%.1f of %.1f years", years, r.MinYears)
			if years < r.MinYears {
				c.Strength = math.Min(.6, c.Strength)
				bestWeighted = math.Min(.6, bestWeighted)
			}
		}
		if r.Degree != "" && r.Degree != "not_specified" {
			c.Check = "Required degree: " + r.Degree
			for _, e := range s.Evidence {
				if e.ID == c.EvidenceID {
					degree, _ := e.Details["degree_type"].(string)
					status, _ := e.Details["status"].(string)
					if degree != "" {
						c.Check += "; " + degree + " " + strings.ReplaceAll(status, "_", " ")
					}
				}
			}
		}
		if r.Language != "" && r.Language != "not_specified" {
			c.Check = "Required language level: " + r.Language
			for _, skill := range s.Skills {
				if skill.Type == "language" && strings.Contains(strings.ToLower(r.Text), strings.ToLower(skill.Name)) {
					c.Check += "; " + skill.Name + " " + skill.Proficiency
				}
			}
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
		if c.Answer != nil {
			maxp := 0.
			for _, p := range c.Answer.Probabilities {
				maxp = math.Max(maxp, p)
			}
			c.Review = maxp <= .5
		}
		if c.Answer != nil && Strength(*c.Answer) >= .4 {
			for _, e := range s.Evidence {
				if e.ID == c.EvidenceID {
					for _, h := range e.Highlights {
						if ha, ok := a[HighlightPair(h.ID, r.TextHash)]; ok && Strength(ha) >= .4 {
							c.HighlightIDs = append(c.HighlightIDs, h.ID)
							c.Citations = append(c.Citations, Citation{h.ID, h.Text})
						}
					}
				}
			}
		}
		if c.Status == "gap" {
			out.Insights = append(out.Insights, Insight{Type: "gap", TextHash: r.TextHash, Text: r.Text})
			out.Insights = append(out.Insights, Insight{Type: "library_prompt", TextHash: r.TextHash, Text: "Add evidence or a highlight for this requirement."})
		} else if c.Status == "adjacent" || c.Check != "" && c.Strength < .8 {
			out.Insights = append(out.Insights, Insight{Type: "near_miss", TextHash: r.TextHash, Text: c.Check})
		}
		out.Requirements = append(out.Requirements, c)
		num += w * bestWeighted
	}
	for _, e := range s.Evidence {
		selection := Selection{EvidenceID: e.ID, Relevance: ranks[e.ID], HighlightIDs: []string{}, Title: e.Title}
		highlights := append([]Highlight{}, e.Highlights...)
		sort.SliceStable(highlights, func(i, j int) bool {
			if hknown[highlights[i].ID] != hknown[highlights[j].ID] {
				return hknown[highlights[i].ID]
			}
			return hranks[highlights[i].ID] > hranks[highlights[j].ID]
		})
		for _, h := range highlights {
			if h.AutoInclude {
				selection.HighlightIDs = append(selection.HighlightIDs, h.ID)
			}
		}
		out.Evidence = append(out.Evidence, selection)
		if bests[e.ID] >= .4 {
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
					rank += ranks[e.ID] / float64(max(1, len(e.Skills)))
				}
			}
		}
		out.Skills = append(out.Skills, RankedSkill{SkillID: sk.ID, Name: sk.Name, Relevance: rank})
	}
	sort.SliceStable(out.Skills, func(i, j int) bool { return out.Skills[i].Relevance > out.Skills[j].Relevance })
	rel := []Evidence{}
	for _, e := range s.Evidence {
		if relevant[e.ID] {
			rel = append(rel, e)
		}
	}
	out.RelevantYears = Years(rel, asOf)
	sort.SliceStable(out.Insights, func(i, j int) bool { return out.Insights[i].Type == "gap" && out.Insights[j].Type != "gap" })
	if denom > 0 && !out.Partial {
		score := 100 * num / denom
		out.Score = &score
	}
	return out
}

// Model state contains claims, not CV selection preferences or bookkeeping.
// Keep the full snapshot separately for deterministic selection and staleness.
type evidenceState struct {
	ID           string           `json:"id"`
	Kind         string           `json:"kind"`
	Category     string           `json:"category"`
	Title        string           `json:"title"`
	Summary      string           `json:"summary"`
	Organization any              `json:"organization"`
	StartDate    *string          `json:"start_date"`
	EndDate      *string          `json:"end_date"`
	IsCurrent    bool             `json:"is_current"`
	Details      map[string]any   `json:"details"`
	Highlights   []highlightState `json:"highlights"`
	Skills       []Skill          `json:"skills"`
}
type highlightState struct {
	ID          string   `json:"id"`
	Text        string   `json:"text"`
	MetricValue *float64 `json:"metric_value"`
	MetricUnit  string   `json:"metric_unit"`
	Skills      []Skill  `json:"skills"`
}

func scoringEvidence(e Evidence) evidenceState {
	highlights := make([]highlightState, 0, len(e.Highlights))
	for _, h := range e.Highlights {
		highlights = append(highlights, highlightState{h.ID, h.Text, h.MetricValue, h.MetricUnit, h.Skills})
	}
	return evidenceState{e.ID, e.Kind, e.Category, e.Title, e.Summary, e.Organization, e.StartDate, e.EndDate, e.IsCurrent, e.Details, highlights, e.Skills}
}
