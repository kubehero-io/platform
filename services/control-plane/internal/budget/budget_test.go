// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package budget

import (
	"math"
	"reflect"
	"testing"
)

func TestParseCeiling(t *testing.T) {
	for in, want := range map[string]float64{
		"$100000/mo": 100000, "150000": 150000, "$100,000 / month": 100000,
		"$300/hr": 300 * 720, "$2k/day": 60000, "$1.5m/mo": 1.5e6, "$700/week": 3000,
	} {
		got, ok := ParseCeiling(in)
		if !ok || math.Abs(got-want) > 1e-6 {
			t.Errorf("ParseCeiling(%q) = %v, %v; want %v", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "lots", "$-5/mo", "$0", "100/fortnight"} {
		if _, ok := ParseCeiling(bad); ok {
			t.Errorf("ParseCeiling(%q) should fail", bad)
		}
	}
}

func TestSpecScope(t *testing.T) {
	cases := []struct {
		spec  string
		ns    []string
		exact bool
		desc  string
	}{
		{`{"ceiling":"$1000/mo"}`, nil, true, "all namespaces · c1"},
		{`{"scope":{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"ml"}}}}`, []string{"ml"}, true, "ns=ml · c1"},
		{`{"scope":{"namespaceSelector":{"matchExpressions":[{"key":"kubernetes.io/metadata.name","operator":"In","values":["b","a"]}]}}}`,
			[]string{"a", "b"}, true, "ns=a,b · c1"},
		{`{"scope":{"clusterSelector":{"matchLabels":{"env":"prod"}},"namespaceSelector":{"matchLabels":{"team":"x"}}}}`,
			nil, false, "env=prod · ns selector team=x (measured cluster-wide) · c1"},
	}
	for _, c := range cases {
		s, err := ParseSpec([]byte(c.spec))
		if err != nil {
			t.Fatal(err)
		}
		ns, exact := s.Namespaces()
		if !reflect.DeepEqual(ns, c.ns) || exact != c.exact {
			t.Errorf("%s: namespaces %v exact %v", c.spec, ns, exact)
		}
		if got := s.Describe("c1"); got != c.desc {
			t.Errorf("%s: describe %q, want %q", c.spec, got, c.desc)
		}
	}
	if _, err := ParseSpec([]byte("{bad")); err == nil {
		t.Error("bad JSON must error")
	}
}
