// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package profiles

import (
	"math"
	"sort"
)

// Stack is one unique call stack (root → leaf) with its summed value.
type Stack struct {
	Frames []string
	Value  int64
}

// maxDepth bounds a single stack (eBPF unwinders stop far earlier).
const maxDepth = 1024

// node is one frame in the merged call tree. Nodes are identified by
// their path from the root, so a recursive function appears once per
// distinct call path — the standard flamegraph shape.
type node struct {
	name        string
	self, total int64
	// Baseline values (diff mode), before normalisation.
	bself, btotal float64
	children      map[string]*node
	depth         int
}

func newNode(name string, depth int) *node {
	return &node{name: name, depth: depth, children: map[string]*node{}}
}

// Tree is a merged call tree for one profile (and optionally a
// baseline for diffs).
type Tree struct {
	root          *node
	nodes         int
	Total         int64
	BaselineTotal int64
}

// NewTree returns an empty tree whose root is the synthetic "total".
func NewTree() *Tree { return &Tree{root: newNode("total", 0)} }

// Add merges a stack into the main profile.
func (t *Tree) Add(frames []string, v int64) { t.add(frames, v, false) }

// AddBaseline merges a stack into the baseline profile.
func (t *Tree) AddBaseline(frames []string, v int64) { t.add(frames, v, true) }

func (t *Tree) add(frames []string, v int64, baseline bool) {
	if v <= 0 {
		return
	}
	if len(frames) > maxDepth {
		frames = append(append([]string(nil), frames[:maxDepth-1]...), "[truncated]")
	}
	n := t.root
	if baseline {
		t.BaselineTotal += v
		n.btotal += float64(v)
	} else {
		t.Total += v
		n.total += v
	}
	for _, f := range frames {
		c := n.children[f]
		if c == nil {
			c = newNode(f, n.depth+1)
			n.children[f] = c
			t.nodes++
		}
		if baseline {
			c.btotal += float64(v)
		} else {
			c.total += v
		}
		n = c
	}
	if baseline {
		n.bself += float64(v)
	} else {
		n.self += v
	}
}

// FlatNode is one emitted frame (mirrors the proto FlameNode).
type FlatNode struct {
	Name          string
	Parent        int32
	Depth         int32
	Self, Total   int64
	BaselineSelf  int64
	BaselineTotal int64
}

// Flatten prunes the tree to at most maxNodes frames and emits it in
// pre-order with parent indices; nodes[0] is the root.
//
// Pruning: a frame's weight is max(total, normalised baseline total);
// frames below root/maxNodes are dropped, and at most maxNodes-1 of the
// heaviest survive (ties break toward shallower frames, so a kept
// frame's parent is always kept). A dropped subtree's time is folded
// into its parent's self, so every total still adds up.
//
// Diff mode: baseline values are scaled by Total/BaselineTotal so both
// profiles describe the same amount of work, then attached per path.
func (t *Tree) Flatten(maxNodes int) []FlatNode {
	if maxNodes < 2 {
		maxNodes = 2
	}
	scale := 0.0
	if t.BaselineTotal > 0 && t.Total > 0 {
		scale = float64(t.Total) / float64(t.BaselineTotal)
	}
	weight := func(n *node) float64 { return math.Max(float64(n.total), n.btotal*scale) }

	var all []*node
	var collect func(n *node)
	collect = func(n *node) {
		for _, c := range n.children {
			all = append(all, c)
			collect(c)
		}
	}
	collect(t.root)
	threshold := weight(t.root) / float64(maxNodes)
	sort.Slice(all, func(i, j int) bool {
		wi, wj := weight(all[i]), weight(all[j])
		if wi != wj {
			return wi > wj
		}
		if all[i].depth != all[j].depth {
			return all[i].depth < all[j].depth
		}
		return all[i].name < all[j].name
	})
	keep := map[*node]bool{t.root: true}
	for i, n := range all {
		if i >= maxNodes-1 || weight(n) < threshold {
			break
		}
		keep[n] = true
	}

	out := make([]FlatNode, 0, len(keep))
	var emit func(n *node, parent int32, self int64, bself float64)
	emit = func(n *node, parent int32, self int64, bself float64) {
		idx := int32(len(out))
		out = append(out, FlatNode{Name: n.name, Parent: parent, Depth: int32(n.depth), Total: n.total,
			BaselineTotal: int64(math.Round(n.btotal * scale))})
		// Fold pruned children into this frame's self.
		names := make([]string, 0, len(n.children))
		for name, c := range n.children {
			if keep[c] {
				names = append(names, name)
			} else {
				self += c.total
				bself += c.btotal
			}
		}
		out[idx].Self = self
		out[idx].BaselineSelf = int64(math.Round(bself * scale))
		sort.Strings(names)
		for _, name := range names {
			c := n.children[name]
			emit(c, idx, c.self, c.bself)
		}
	}
	emit(t.root, -1, t.root.self, t.root.bself)
	return out
}

// Func is one function's self/total across a profile.
type Func struct {
	Name        string
	Self, Total int64
}

// TopFunctions ranks functions across stacks. A function's self is the
// time with it on top of the stack; its total counts every stack it
// appears in once, even when it recurses. by is "self" or "total".
func TopFunctions(stacks []Stack, by string, limit int) []Func {
	self := map[string]int64{}
	total := map[string]int64{}
	seen := map[string]bool{}
	for _, s := range stacks {
		if s.Value <= 0 || len(s.Frames) == 0 {
			continue
		}
		self[s.Frames[len(s.Frames)-1]] += s.Value
		clear(seen)
		for _, f := range s.Frames {
			if !seen[f] {
				seen[f] = true
				total[f] += s.Value
			}
		}
	}
	out := make([]Func, 0, len(total))
	for name, tot := range total {
		out = append(out, Func{Name: name, Self: self[name], Total: tot})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].Self, out[j].Self
		if by == "total" {
			a, b = out[i].Total, out[j].Total
		}
		if a != b {
			return a > b
		}
		return out[i].Name < out[j].Name
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}
