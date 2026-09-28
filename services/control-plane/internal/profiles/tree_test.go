// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package profiles

import (
	"fmt"
	"testing"
)

func build(stacks ...Stack) *Tree {
	t := NewTree()
	for _, s := range stacks {
		t.Add(s.Frames, s.Value)
	}
	return t
}

func st(v int64, frames ...string) Stack { return Stack{Frames: frames, Value: v} }

func find(nodes []FlatNode, path ...string) *FlatNode {
	idx := int32(0)
	for _, p := range path {
		found := int32(-1)
		for i := range nodes {
			if nodes[i].Parent == idx && nodes[i].Name == p {
				found = int32(i)
			}
		}
		if found < 0 {
			return nil
		}
		idx = found
	}
	return &nodes[idx]
}

// checkConsistency: every node's total = self + Σ children totals, and
// pre-order parents precede children.
func checkConsistency(t *testing.T, nodes []FlatNode) {
	t.Helper()
	sum := make([]int64, len(nodes))
	for i, n := range nodes {
		if i == 0 {
			if n.Parent != -1 || n.Name != "total" {
				t.Fatalf("root = %+v", n)
			}
			continue
		}
		if n.Parent < 0 || int(n.Parent) >= i {
			t.Fatalf("node %d parent %d not before it", i, n.Parent)
		}
		if n.Depth != nodes[n.Parent].Depth+1 {
			t.Fatalf("depth mismatch at %d", i)
		}
		sum[n.Parent] += n.Total
	}
	for i, n := range nodes {
		if n.Self+sum[i] != n.Total {
			t.Fatalf("node %q: self %d + children %d != total %d", n.Name, n.Self, sum[i], n.Total)
		}
	}
}

func TestTreeMergeAndLayout(t *testing.T) {
	tr := build(st(10, "a", "b"), st(5, "a", "c"), st(1, "a"), st(4, "d"))
	nodes := tr.Flatten(100)
	checkConsistency(t, nodes)
	if tr.Total != 20 || nodes[0].Total != 20 || len(nodes) != 5 {
		t.Fatalf("total %d, %d nodes", tr.Total, len(nodes))
	}
	a := find(nodes, "a")
	if a == nil || a.Total != 16 || a.Self != 1 {
		t.Fatalf("a = %+v", a)
	}
	// Pre-order, children alphabetical: total, a, b, c, d.
	want := []string{"total", "a", "b", "c", "d"}
	for i, n := range nodes {
		if n.Name != want[i] {
			t.Fatalf("order %d = %s, want %s", i, n.Name, want[i])
		}
	}
}

func TestTreePruneFoldsIntoParentSelf(t *testing.T) {
	tr := build(st(10, "a", "b"), st(5, "a", "c"), st(1, "a"), st(4, "d"))
	nodes := tr.Flatten(3) // root + a + b
	checkConsistency(t, nodes)
	if len(nodes) != 3 {
		t.Fatalf("want 3 nodes, got %d", len(nodes))
	}
	if a := find(nodes, "a"); a.Self != 6 { // 1 own + 5 from pruned c
		t.Fatalf("a.self = %d", a.Self)
	}
	if nodes[0].Self != 4 { // pruned d folds into root
		t.Fatalf("root.self = %d", nodes[0].Self)
	}
	// Threshold pruning: with 1000 frames each worth 1 and one worth
	// 10000, maxNodes=10 keeps at most 10.
	big := NewTree()
	big.Add([]string{"hot"}, 10000)
	for i := 0; i < 1000; i++ {
		big.Add([]string{"cold", fmt.Sprintf("f%d", i)}, 1)
	}
	n := big.Flatten(10)
	checkConsistency(t, n)
	if len(n) > 10 {
		t.Fatalf("maxNodes exceeded: %d", len(n))
	}
}

func TestTreeDiffNormalises(t *testing.T) {
	tr := build(st(30, "main", "json.Marshal"), st(10, "main", "db.Query"))
	// Baseline covered twice the work: 40 marshal… no — 20 marshal, 60
	// query over 80 units; normalised to 40 → marshal 10, query 30.
	tr.AddBaseline([]string{"main", "json.Marshal"}, 20)
	tr.AddBaseline([]string{"main", "db.Query"}, 60)
	tr.AddBaseline([]string{"main", "gone.Func"}, 0) // ignored
	nodes := tr.Flatten(100)
	checkConsistency(t, nodes)
	if nodes[0].BaselineTotal != 40 || tr.BaselineTotal != 80 {
		t.Fatalf("root baseline %d (raw %d)", nodes[0].BaselineTotal, tr.BaselineTotal)
	}
	m := find(nodes, "main", "json.Marshal")
	if m.Total != 30 || m.BaselineTotal != 10 || m.BaselineSelf != 10 {
		t.Fatalf("regression node = %+v", m)
	}
	q := find(nodes, "main", "db.Query")
	if q.BaselineTotal != 30 {
		t.Fatalf("improvement node = %+v", q)
	}
	// A path that only exists in the baseline still shows (total 0).
	tr.AddBaseline([]string{"main", "removed.Path"}, 40)
	nodes = tr.Flatten(100)
	if r := find(nodes, "main", "removed.Path"); r == nil || r.Total != 0 || r.BaselineTotal == 0 {
		t.Fatalf("baseline-only path: %+v", r)
	}
}

func TestTopFunctionsCountsRecursionOnce(t *testing.T) {
	stacks := []Stack{
		st(10, "main", "walk", "visit", "walk"),
		st(6, "main", "encode"),
		st(4, "main", "walk", "encode"),
	}
	top := TopFunctions(stacks, "self", 0)
	by := map[string]Func{}
	for _, f := range top {
		by[f.Name] = f
	}
	if by["walk"].Total != 14 || by["walk"].Self != 10 {
		t.Fatalf("walk = %+v (recursion must count once per stack)", by["walk"])
	}
	if by["encode"].Self != 10 || by["main"].Total != 20 || by["main"].Self != 0 {
		t.Fatalf("encode %+v main %+v", by["encode"], by["main"])
	}
	if top[0].Name != "encode" && top[0].Name != "walk" {
		t.Fatalf("self order: %+v", top)
	}
	tot := TopFunctions(stacks, "total", 1)
	if len(tot) != 1 || tot[0].Name != "main" {
		t.Fatalf("total order: %+v", tot)
	}
}

func TestDepthCap(t *testing.T) {
	frames := make([]string, 2000)
	for i := range frames {
		frames[i] = fmt.Sprintf("f%d", i)
	}
	tr := build(Stack{Frames: frames, Value: 1})
	nodes := tr.Flatten(5000)
	if len(nodes) != maxDepth+1 || nodes[len(nodes)-1].Name != "[truncated]" {
		t.Fatalf("depth cap: %d nodes, last %q", len(nodes), nodes[len(nodes)-1].Name)
	}
}
