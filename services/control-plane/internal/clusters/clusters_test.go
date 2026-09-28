// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package clusters

import (
	"context"
	"reflect"
	"testing"
)

func TestSnapshotAliasesAndDisplay(t *testing.T) {
	const id = "0b6c5c4e-8f3a-4c1e-9d55-1d2f3a4b5c6d"
	s := NewSnapshot([]Info{
		{ID: id, Slug: "eks-use1-prod", Name: "EKS prod"},
		// A second cluster whose display name collides with the first's
		// slug must not hijack it.
		{ID: "11111111-2222-3333-4444-555555555555", Slug: "gke", Name: "eks-use1-prod"},
	})
	for _, in := range []string{id, "eks-use1-prod", "EKS prod"} {
		if got := s.Aliases(in); !reflect.DeepEqual(got, []string{id, "eks-use1-prod"}) {
			t.Errorf("Aliases(%q) = %v", in, got)
		}
	}
	if got := s.Aliases("unregistered"); !reflect.DeepEqual(got, []string{"unregistered"}) {
		t.Errorf("unknown alias passthrough = %v", got)
	}
	if s.Aliases("") != nil {
		t.Error("empty id must expand to no filter")
	}
	if got := s.Display(id); got != "eks-use1-prod" {
		t.Errorf("Display(uuid) = %q", got)
	}
	if got := s.Display("raw-ch-id"); got != "raw-ch-id" {
		t.Errorf("Display passthrough = %q", got)
	}
	if _, ok := s.Lookup("gke"); !ok {
		t.Error("Lookup by slug failed")
	}
	if len(s.List()) != 2 {
		t.Error("List")
	}
}

func TestNilResolverIsIdentity(t *testing.T) {
	var r *Resolver
	s := r.Snapshot(context.Background())
	if got := s.Aliases("x"); !reflect.DeepEqual(got, []string{"x"}) {
		t.Fatalf("identity aliases = %v", got)
	}
	if s.Display("x") != "x" {
		t.Fatal("identity display")
	}
	var nilSnap *Snapshot
	if nilSnap.Display("y") != "y" || nilSnap.Aliases("y")[0] != "y" {
		t.Fatal("nil snapshot must be identity")
	}
}
