package render

import (
	"container/heap"
	"math"
	"sort"
	"strings"
	"unicode/utf8"
)

type trimCandidate struct {
	id                string
	parent            *trimCandidate
	children          []*trimCandidate
	duplicate         *duplicateGroup
	value, priority   float64
	depth, index      int
	active, optional  bool
	pinned, protected bool
}

// A group contributes one copy of its wording. Members are ordered by pin,
// relevance per line, then display order. Removed copies never re-enter.
type duplicateGroup struct {
	members []*trimCandidate
	first   int
}

type trimQueue []*trimCandidate

func (q trimQueue) Len() int { return len(q) }
func (q trimQueue) Less(i, j int) bool {
	a, b := q[i], q[j]
	if a.priority != b.priority {
		return a.priority < b.priority
	}
	if a.depth != b.depth {
		return a.depth > b.depth // Refill containers before their descendants.
	}
	return a.id < b.id
}
func (q trimQueue) Swap(i, j int) { q[i], q[j] = q[j], q[i]; q[i].index, q[j].index = i, j }
func (q *trimQueue) Push(x any) {
	n := x.(*trimCandidate)
	n.index = len(*q)
	*q = append(*q, n)
}
func (q *trimQueue) Pop() any {
	n := (*q)[len(*q)-1]
	*q = (*q)[:len(*q)-1]
	n.index = -1
	return n
}

// scoredTrimOrder greedily removes the least useful optional material per
// estimated line. Actual page counts remain the browser compiler's decision.
// Containers inherit their strongest surviving detail's priority, so removing
// an otherwise weak role cannot accidentally remove a highly relevant bullet.
// Repeated wording contributes once; its best remaining copy is recomputed
// after removal. This is deliberately lexical, not semantic deduplication.
func scoredTrimOrder(c Content, relevance, highlights map[string]float64, pinned, supports map[string]bool) []string {
	nodes := []*trimCandidate{}
	groups := map[string]*duplicateGroup{}
	var walk func(Entry, *trimCandidate, int)
	walk = func(e Entry, parent *trimCandidate, depth int) {
		value := relevance[e.ID]
		if supports[e.ID] {
			value += 1000 // Retain historical must-have support.
		}
		lines := math.Max(1, textLines(e.Title+" "+e.Organization)) + 1 + textLines(e.Summary)
		n := &trimCandidate{id: e.ID, parent: parent, depth: depth, index: -1, value: value / lines,
			active: true, optional: true, pinned: pinned[e.ID]}
		nodes = append(nodes, n)
		if parent != nil {
			parent.children = append(parent.children, n)
		}
		for i, h := range e.Highlights {
			value, known := highlights[h.ID]
			if !known {
				// Partial/stale rankings may not know a newly added bullet.
				value = relevance[e.ID] * math.Pow(.7, float64(i))
			}
			child := &trimCandidate{id: h.ID, parent: n, depth: depth + 1, index: -1,
				value: value / math.Max(1, textLines(h.Text)), active: true, optional: i > 0, pinned: pinned[h.ID]}
			n.children = append(n.children, child)
			nodes = append(nodes, child)
			key := strings.ToLower(strings.Join(strings.Fields(h.Text), " "))
			if key != "" {
				if groups[key] == nil {
					groups[key] = &duplicateGroup{}
				}
				child.duplicate = groups[key]
				groups[key].members = append(groups[key].members, child)
			}
		}
		for _, child := range e.Children {
			walk(child, n, depth+1)
		}
	}
	for _, section := range c.Sections {
		for _, e := range section.Entries {
			walk(e, nil, 0)
		}
	}
	for _, group := range groups {
		sort.SliceStable(group.members, func(i, j int) bool {
			a, b := group.members[i], group.members[j]
			if a.pinned != b.pinned {
				return a.pinned // A pinned copy already guarantees this wording survives.
			}
			return a.value > b.value
		})
	}
	priority := func(n *trimCandidate) float64 {
		value := n.value
		if n.duplicate != nil && n.duplicate.members[n.duplicate.first] != n {
			value = 0
		}
		for _, child := range n.children {
			if child.active {
				value = math.Max(value, child.priority)
			}
		}
		return value
	}
	// Preorder construction lets a reverse pass propagate pins and priorities.
	for i := len(nodes) - 1; i >= 0; i-- {
		n := nodes[i]
		n.priority = priority(n)
		n.protected = n.protected || n.pinned
		if n.parent != nil {
			n.parent.protected = n.parent.protected || n.protected
		}
	}
	q := &trimQueue{}
	for _, n := range nodes {
		if n.optional && !n.protected {
			n.index = len(*q)
			*q = append(*q, n)
		}
	}
	heap.Init(q)
	// Only affected ancestors and a newly promoted duplicate need new scores.
	// Avoid rescanning the entire library for every item removed.
	update := func(n *trimCandidate) {
		for n != nil && n.active {
			value := priority(n)
			if value == n.priority {
				break
			}
			n.priority = value
			if n.index >= 0 {
				heap.Fix(q, n.index)
			}
			n = n.parent
		}
	}
	out := []string{}
	// Keep the existing skills policy: languages and the first eight remain.
	var skills []string
	for _, s := range c.Skills {
		if s.Type != "language" {
			skills = append(skills, s.ID)
		}
	}
	for i := len(skills) - 1; i >= KeptSkills; i-- {
		out = append(out, skills[i])
	}
	for q.Len() > 0 {
		n := heap.Pop(q).(*trimCandidate)
		out = append(out, n.id)
		n.active = false
		// Descendants leave before containers at equal priority. All optional
		// descendants have already gone; deactivate retained anchor bullets.
		for _, child := range n.children {
			if child.active {
				child.active = false
				if group := child.duplicate; group != nil && group.members[group.first] == child {
					for group.first < len(group.members) && !group.members[group.first].active {
						group.first++
					}
					if group.first < len(group.members) {
						update(group.members[group.first])
					}
				}
			}
		}
		if group := n.duplicate; group != nil && group.members[group.first] == n {
			for group.first < len(group.members) && !group.members[group.first].active {
				group.first++
			}
			if group.first < len(group.members) {
				update(group.members[group.first])
			}
		}
		update(n.parent)
	}
	return out
}

// A text-only estimate, shared across locales. It is a priority heuristic,
// never a substitute for compiling the PDF with the chosen template.
func textLines(text string) float64 {
	lines := 0.
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		lines += math.Ceil(float64(utf8.RuneCountInString(line)) / 90)
	}
	return lines
}
