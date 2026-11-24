package render

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/diegohh0411/zapply/internal/match"
)

func scoredLibrary() (Library, *match.Result) {
	lib := Library{Profile: Profile{Name: "Ada"}, Evidence: []Evidence{
		{ID: "recent", Kind: "job", Category: "paid", Title: "Recent role", AutoInclude: true, IsCurrent: true,
			Highlights: []Highlight{{"generic", "Worked on internal tools", true}, {"weak", "Attended planning meetings", true}}},
		{ID: "older", Kind: "job", Category: "paid", Title: "Earlier role", AutoInclude: true,
			Highlights: []Highlight{{"anchor", "Owned production reliability", true}, {"strong", "Reduced incident recovery time by 40%", true}}},
	}}
	ranking := &match.Result{Evidence: []match.Selection{
		{EvidenceID: "recent", Relevance: .9, HighlightIDs: []string{"generic", "weak"}, HighlightScores: map[string]float64{"generic": .3, "weak": .2}},
		{EvidenceID: "older", Relevance: .2, HighlightIDs: []string{"anchor", "strong"}, HighlightScores: map[string]float64{"anchor": 1, "strong": .9}},
	}}
	return lib, ranking
}

func TestScoredTrimWeighsRoleRelevanceOverOneStrongBullet(t *testing.T) {
	lib, ranking := scoredLibrary()
	b := Build(lib, ranking, Picks{}, "en", today)
	// The weak role (.2) leaves before the strong one (.9), even though one of
	// its bullets scored well: its own relevance counts twice.
	if slices.Index(b.Trim, "older") >= slices.Index(b.Trim, "recent") {
		t.Fatalf("weak role outlasted a relevant one on one bullet: %v", b.Trim)
	}
	// A strong bullet still outlasts weak bullets of the kept role.
	if slices.Index(b.Trim, "strong") <= slices.Index(b.Trim, "weak") {
		t.Fatalf("strong bullet trimmed before a weak one: %v", b.Trim)
	}
	cut := slices.Index(b.Trim, "older") + 1
	remaining := section(Apply(b.Content, b.Trim[:cut]), "experience").Entries
	if len(remaining) != 1 || remaining[0].ID != "recent" {
		t.Fatalf("relevant role should survive: %+v", remaining)
	}
	if got := ids(section(b.Content, "experience").Entries); !slices.Equal(got, []string{"recent", "older"}) {
		t.Fatalf("scoring changed chronological display order: %v", got)
	}
}

func TestScoredTrimUsesSpaceWithoutPenalizingUnicodeBytes(t *testing.T) {
	lib, ranking := scoredLibrary()
	// Both optional bullets have the same relevance. A verbose detail should
	// leave before a concise one, regardless of which role supplied it.
	ranking.Evidence[0].HighlightScores["weak"] = .9
	for _, text := range []string{strings.Repeat("long detail ", 35), strings.Repeat("界", 350)} {
		lib.Evidence[0].Highlights[1].Text = text
		b := Build(lib, ranking, Picks{}, "en", today)
		if slices.Index(b.Trim, "weak") >= slices.Index(b.Trim, "strong") {
			t.Fatalf("long equally relevant detail should trim first: %v", b.Trim)
		}
	}
	if textLines(strings.Repeat("界", 90)) != textLines(strings.Repeat("a", 90)) {
		t.Fatal("line estimates must count characters, not UTF-8 bytes")
	}
}

func TestScoredTrimDropsRepeatedWordingBeforeDistinctEvidence(t *testing.T) {
	lib, ranking := scoredLibrary()
	lib.Evidence[0].Highlights[1].Text = "  OWNED  production\nreliability  "
	ranking.Evidence[0].HighlightScores["weak"] = 1
	b := Build(lib, ranking, Picks{}, "en", today)
	if slices.Index(b.Trim, "weak") >= slices.Index(b.Trim, "strong") {
		t.Fatalf("redundant copy should leave before distinct evidence: %v", b.Trim)
	}
	cut := slices.Index(b.Trim, "weak") + 1
	text, _ := json.Marshal(Apply(b.Content, b.Trim[:cut]))
	if !strings.Contains(string(text), `"id":"anchor"`) {
		t.Fatalf("deduplication lost the remaining proof: %s", text)
	}
	// Different metrics describe different claims, even with similar wording.
	lib.Evidence[0].Highlights[1].Text = "Reduced incident recovery time by 20%"
	ranking.Evidence[0].HighlightScores["weak"] = .95
	b = Build(lib, ranking, Picks{}, "en", today)
	if slices.Index(b.Trim, "weak") <= slices.Index(b.Trim, "strong") {
		t.Fatalf("distinct metrics must not be collapsed: %v", b.Trim)
	}
}

func TestScoredTrimHonorsPinsExclusionsAndNestedContainers(t *testing.T) {
	lib, ranking := scoredLibrary()
	lib.Evidence[1].ParentID = ptr("recent")
	for _, pinned := range []string{"strong", "older"} {
		b := Build(lib, ranking, Picks{Include: []string{pinned}, Exclude: []string{"weak"}}, "en", today)
		for k := 0; k <= len(b.Trim); k++ {
			text, _ := json.Marshal(Apply(b.Content, b.Trim[:k]))
			for _, id := range []string{pinned, "recent", "older"} {
				if !strings.Contains(string(text), `"id":"`+id+`"`) {
					t.Fatalf("pin %s lost %s at prefix %d: %v", pinned, id, k, b.Trim)
				}
			}
			if strings.Contains(string(text), `"id":"weak"`) {
				t.Fatal("excluded bullet reappeared")
			}
		}
	}
	// Without pins, every optional descendant must precede its container so
	// the browser's reverse-order refill can restore the container first.
	b := Build(lib, ranking, Picks{}, "en", today)
	if slices.Index(b.Trim, "strong") >= slices.Index(b.Trim, "older") || slices.Index(b.Trim, "older") >= slices.Index(b.Trim, "recent") {
		t.Fatalf("container dependencies are not respected: %v", b.Trim)
	}
}

func TestScoredTrimKeepsHistoricalFallbackAndUnknownBullets(t *testing.T) {
	lib, ranking := scoredLibrary()
	for i := range ranking.Evidence {
		ranking.Evidence[i].HighlightScores = nil
	}
	legacy := Build(lib, ranking, Picks{}, "en", today)
	if slices.Index(legacy.Trim, "strong") >= slices.Index(legacy.Trim, "weak") {
		t.Fatalf("historical ordering changed without bullet scores: %v", legacy.Trim)
	}
	// Zero is a known score, not an absent score. Unknown/new content remains
	// available and receives the fallback priority rather than a fabricated zero.
	ranking.Evidence[0].HighlightScores = map[string]float64{"weak": 0}
	b := Build(lib, ranking, Picks{}, "en", today)
	if b.Trim[0] != "weak" || !slices.Contains(b.Trim, "strong") {
		t.Fatalf("known zero and unknown bullet were confused: %v", b.Trim)
	}
	if again := Build(lib, ranking, Picks{}, "en", today); !slices.Equal(b.Trim, again.Trim) {
		t.Fatal("identical inputs produced different fitting orders")
	}
}

func TestScoredTrimPreservesTreeDependenciesWithDuplicatesAndPins(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 11))
	for range 100 {
		content := Content{Sections: []Section{{Key: "experience"}}}
		relevance, scores := map[string]float64{}, map[string]float64{}
		pins := map[string]bool{}
		var entry func(int, int) Entry
		entry = func(id, depth int) Entry {
			e := Entry{ID: fmt.Sprintf("e%d", id), Title: "Role"}
			relevance[e.ID] = float64(rng.IntN(5)) / 4
			for j := range 3 {
				h := ContentHighlight{ID: fmt.Sprintf("h%d-%d", id, j), Text: fmt.Sprintf("Proof %d", rng.IntN(6))}
				scores[h.ID] = float64(rng.IntN(5)) / 4
				pins[h.ID] = rng.IntN(12) == 0
				e.Highlights = append(e.Highlights, h)
			}
			if depth > 0 {
				e.Children = []Entry{entry(id*2, depth-1), entry(id*2+1, depth-1)}
			}
			return e
		}
		content.Sections[0].Entries = []Entry{entry(1, 3)}
		trim := scoredTrimOrder(content, relevance, scores, pins, nil)
		positions := map[string]int{}
		for i, id := range trim {
			if _, exists := positions[id]; exists || pins[id] {
				t.Fatalf("duplicate or pinned ID in trim: %s", id)
			}
			positions[id] = i
		}
		var check func(Entry) bool
		check = func(e Entry) bool {
			protected := false
			position, dropped := positions[e.ID]
			for i, h := range e.Highlights {
				protected = protected || pins[h.ID]
				if p, ok := positions[h.ID]; i == 0 && ok || i > 0 && !pins[h.ID] && (!ok || dropped && p >= position) {
					t.Fatalf("bullet %s lost or scheduled after its entry: %v", h.ID, trim)
				}
			}
			for _, child := range e.Children {
				protected = check(child) || protected
				if p, ok := positions[child.ID]; dropped && (!ok || p >= position) {
					t.Fatalf("child %s scheduled after its parent: %v", child.ID, trim)
				}
			}
			if dropped == protected {
				t.Fatalf("entry %s has inconsistent pin protection: %v", e.ID, trim)
			}
			return protected
		}
		check(content.Sections[0].Entries[0])
	}
}

func BenchmarkScoredTrimLargeLibrary(b *testing.B) {
	content := Content{Sections: []Section{{Key: "experience"}}}
	relevance, scores := map[string]float64{}, map[string]float64{}
	// The matching limit is 300 entries; each entry permits 50 highlights.
	for i := range 300 {
		e := Entry{ID: fmt.Sprintf("e%d", i), Title: "Role"}
		relevance[e.ID] = float64(i%5) / 4
		for j := range 50 {
			h := ContentHighlight{ID: fmt.Sprintf("h%d-%d", i, j), Text: fmt.Sprintf("Concrete proof %d-%d", i, j)}
			scores[h.ID] = float64(j%5) / 4
			e.Highlights = append(e.Highlights, h)
		}
		content.Sections[0].Entries = append(content.Sections[0].Entries, e)
	}
	for b.Loop() {
		if trim := scoredTrimOrder(content, relevance, scores, nil, nil); len(trim) != 15000 {
			b.Fatalf("optional candidates lost: %d", len(trim))
		}
	}
}
