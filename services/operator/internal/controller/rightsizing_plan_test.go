// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1/kuberov1connect"
	kubeherov1 "github.com/kubehero-io/platform/services/operator/api/v1"
)

func testContainer(cpuReq, memReq, cpuLim, memLim string) corev1.Container {
	c := corev1.Container{Name: "app", Resources: corev1.ResourceRequirements{
		Requests: corev1.ResourceList{}, Limits: corev1.ResourceList{},
	}}
	set := func(rl corev1.ResourceList, name corev1.ResourceName, v string) {
		if v != "" {
			rl[name] = resource.MustParse(v)
		}
	}
	set(c.Resources.Requests, corev1.ResourceCPU, cpuReq)
	set(c.Resources.Requests, corev1.ResourceMemory, memReq)
	set(c.Resources.Limits, corev1.ResourceCPU, cpuLim)
	set(c.Resources.Limits, corev1.ResourceMemory, memLim)
	return c
}

func testRec(cpu float64, mem, memMax uint64) *kuberov1.RightsizingRecommendation {
	return &kuberov1.RightsizingRecommendation{
		Workload: "api", WorkloadKind: "Deployment", Container: "app",
		CpuRecommendedCores: cpu, MemRecommendedBytes: mem, MemMaxBytes: memMax,
		SavingsUsdMonth: 100, Confidence: "high", Direction: "downsize",
	}
}

const gi = uint64(1) << 30

func TestPlanContainer(t *testing.T) {
	w := workload{Kind: "Deployment", Namespace: "ns", Name: "api", Replicas: 2}
	tests := []struct {
		name         string
		c            corev1.Container
		rec          *kuberov1.RightsizingRecommendation
		auto         autoscalerInfo
		adjustLimits bool
		wantNil      bool
		wantBlocked  bool
		wantCPU      string // "" = unchanged
		wantMem      string
		wantCPULim   string
		wantMemLim   string
		wantReason   string
	}{
		{
			name:    "plain downsize within bounds",
			c:       testContainer("2", "4Gi", "", ""),
			rec:     testRec(1.2, 3*gi, 3*gi),
			wantCPU: "1200m", wantMem: "3Gi",
		},
		{
			name:    "shrink clamped to 50%",
			c:       testContainer("4", "8Gi", "", ""),
			rec:     testRec(0.5, 1*gi, 1*gi),
			wantCPU: "2", wantMem: "4Gi",
			wantReason: "decrease limited to 50% per step",
		},
		{
			name:    "growth clamped to 100%",
			c:       testContainer("500m", "1Gi", "", ""),
			rec:     &kuberov1.RightsizingRecommendation{Container: "app", CpuRecommendedCores: 3, MemRecommendedBytes: 1 * gi, MemMaxBytes: gi, Confidence: "high", Direction: "upsize"},
			wantCPU: "1", wantReason: "increase limited to 100% per step",
		},
		{
			name:    "direction ok is a no-op",
			c:       testContainer("2", "4Gi", "", ""),
			rec:     &kuberov1.RightsizingRecommendation{Container: "app", CpuRecommendedCores: 1, Direction: "ok"},
			wantNil: true,
		},
		{
			name:    "sub-10% change is noise",
			c:       testContainer("1", "1Gi", "", ""),
			rec:     testRec(0.95, 1*gi, gi),
			wantNil: true,
		},
		{
			name:        "OOM blocks the only (memory) change",
			c:           testContainer("1", "4Gi", "", ""),
			rec:         func() *kuberov1.RightsizingRecommendation { r := testRec(1, 2*gi, 2*gi); r.OomKills = 2; return r }(),
			wantBlocked: true,
			wantReason:  "memory decrease blocked: 2 OOM kills",
		},
		{
			name:    "OOM still allows a cpu cut",
			c:       testContainer("2", "4Gi", "", ""),
			rec:     func() *kuberov1.RightsizingRecommendation { r := testRec(1.2, 2*gi, 2*gi); r.OomKills = 1; return r }(),
			wantCPU: "1200m", wantReason: "memory decrease blocked",
		},
		{
			name:    "memory never below the observed max",
			c:       testContainer("1", "4Gi", "", ""),
			rec:     testRec(1, 2*gi, 3*gi),
			wantMem: "3Gi", wantReason: "memory raised to the observed max",
		},
		{
			name:        "unknown observed max blocks a memory cut",
			c:           testContainer("1", "4Gi", "", ""),
			rec:         testRec(1, 2*gi, 0),
			wantBlocked: true,
			wantReason:  "observed max unknown",
		},
		{
			name:    "memory increase allowed without a known max",
			c:       testContainer("1", "1Gi", "", ""),
			rec:     testRec(1, 2*gi, 0),
			wantMem: "2Gi",
		},
		{
			name:       "request capped at the existing limit",
			c:          testContainer("500m", "1Gi", "800m", ""),
			rec:        &kuberov1.RightsizingRecommendation{Container: "app", CpuRecommendedCores: 0.9, Confidence: "high", Direction: "upsize"},
			wantCPU:    "800m",
			wantReason: "capped at the cpu limit",
		},
		{
			name:         "adjustLimits keeps the limit:request ratio",
			c:            testContainer("2", "4Gi", "4", "8Gi"),
			rec:          testRec(1.2, 3*gi, 3*gi),
			adjustLimits: true,
			wantCPU:      "1200m", wantMem: "3Gi", wantCPULim: "2400m", wantMemLim: "6Gi",
		},
		{
			name:    "no request set: never add one",
			c:       testContainer("", "4Gi", "", ""),
			rec:     testRec(1.2, 3*gi, 3*gi),
			wantMem: "3Gi",
		},
		{
			name:       "HPA on cpu utilization leaves cpu alone",
			c:          testContainer("2", "4Gi", "", ""),
			rec:        testRec(1.2, 3*gi, 3*gi),
			auto:       autoscalerInfo{HPAResources: map[string]string{"cpu": "api-hpa"}},
			wantMem:    "3Gi",
			wantReason: "HPA api-hpa scales on cpu utilization",
		},
		{
			name:        "container-resource HPA on memory blocks memory",
			c:           testContainer("1", "4Gi", "", ""),
			rec:         testRec(1, 3*gi, 3*gi),
			auto:        autoscalerInfo{HPAResources: map[string]string{"app/memory": "mem-hpa"}},
			wantBlocked: true,
			wantReason:  "HPA mem-hpa scales on memory utilization",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cp := planContainer(w, &tc.c, tc.rec, tc.auto, tc.adjustLimits)
			if tc.wantNil {
				if cp != nil {
					t.Fatalf("want no plan, got %+v", cp.Change)
				}
				return
			}
			if cp == nil {
				t.Fatal("want a plan, got nil")
			}
			if got := cp.Change.Outcome == kubeherov1.ChangeOutcomeBlocked; got != tc.wantBlocked {
				t.Errorf("blocked = %v, want %v (reason %q)", got, tc.wantBlocked, cp.Change.Reason)
			}
			check := func(label string, rl corev1.ResourceList, name corev1.ResourceName, want string) {
				t.Helper()
				q, ok := rl[name]
				switch {
				case want == "" && ok:
					t.Errorf("%s changed to %s, want unchanged", label, q.String())
				case want != "" && !ok:
					t.Errorf("%s unchanged, want %s", label, want)
				case want != "" && q.Cmp(resource.MustParse(want)) != 0:
					t.Errorf("%s = %s, want %s", label, q.String(), want)
				}
			}
			check("cpu request", cp.Requests, corev1.ResourceCPU, tc.wantCPU)
			check("memory request", cp.Requests, corev1.ResourceMemory, tc.wantMem)
			check("cpu limit", cp.Limits, corev1.ResourceCPU, tc.wantCPULim)
			check("memory limit", cp.Limits, corev1.ResourceMemory, tc.wantMemLim)
			if tc.wantReason != "" && !strings.Contains(cp.Change.Reason, tc.wantReason) {
				t.Errorf("reason = %q, want it to mention %q", cp.Change.Reason, tc.wantReason)
			}
			for name, q := range cp.Requests {
				if prev, ok := cp.PrevRequests[name]; !ok || prev.Cmp(tc.c.Resources.Requests[name]) != 0 {
					t.Errorf("previous %s not recorded for undo (got %v)", name, cp.PrevRequests)
				}
				_ = q
			}
		})
	}
}

func TestPlanWorkloadGuards(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	base := func() workload {
		return workload{Kind: "Deployment", Namespace: "ns", Name: "api", Replicas: 2,
			Annotations: map[string]string{}, Containers: []corev1.Container{testContainer("2", "4Gi", "", "")}}
	}
	params := func(mut func(*planParams)) planParams {
		p := planParams{Mode: "apply", Armed: true, ClusterKnown: true, MinConfidence: "medium",
			MinReplicas: 1, MaxChangePerDay: 1,
			ChangesToday: func(w workload) int32 { return changesToday(w.Annotations, now) }}
		if mut != nil {
			mut(&p)
		}
		return p
	}
	recs := map[string]*kuberov1.RightsizingRecommendation{"app": testRec(1.2, 3*gi, 3*gi)}

	tests := []struct {
		name   string
		w      func() workload
		recs   map[string]*kuberov1.RightsizingRecommendation
		auto   autoscalerInfo
		vpaErr error
		p      planParams
		want   string // "" = passes
	}{
		{name: "all guards pass", w: base, p: params(nil)},
		{name: "unarmed", w: base, p: params(func(p *planParams) { p.Armed = false }), want: "not armed"},
		{name: "shadow ignores arming", w: base, p: params(func(p *planParams) { p.Mode = "shadow"; p.Armed = false })},
		{name: "unknown cluster", w: base, p: params(func(p *planParams) { p.ClusterKnown = false }), want: "CLUSTER_ID is unset"},
		{name: "min replicas", w: base, p: params(func(p *planParams) { p.MinReplicas = 3 }), want: "2 replicas < safety.minReplicas 3"},
		{name: "rollout", w: func() workload { w := base(); w.RolloutBlocker = "rollout in progress"; return w }, p: params(nil), want: "rollout in progress"},
		{name: "vpa lookup failed", w: base, vpaErr: errors.New("forbidden"), p: params(nil), want: "cannot verify VerticalPodAutoscaler"},
		{name: "vpa manages container", w: base, auto: autoscalerInfo{VPAs: []vpaTarget{{Name: "v", OptedOut: map[string]bool{}}}}, p: params(nil), want: "VerticalPodAutoscaler v manages"},
		{name: "vpa opted this container out", w: base, auto: autoscalerInfo{VPAs: []vpaTarget{{Name: "v", OptedOut: map[string]bool{"app": true}}}}, p: params(nil)},
		{name: "daily budget zero", w: base, p: params(func(p *planParams) { p.MaxChangePerDay = 0 }), want: "maxChangePerDay is 0"},
		{name: "daily budget spent", w: func() workload { w := base(); w.Annotations[dayKey(now)] = "2"; return w }, p: params(func(p *planParams) { p.MaxChangePerDay = 2 }), want: "(2/2 changes today"},
		{name: "yesterday's changes don't count", w: func() workload { w := base(); w.Annotations[dayKey(now.Add(-24*time.Hour))] = "5"; return w }, p: params(nil)},
		{name: "low confidence", w: base, recs: map[string]*kuberov1.RightsizingRecommendation{"app": func() *kuberov1.RightsizingRecommendation {
			r := testRec(1.2, 3*gi, 3*gi)
			r.Confidence = "low"
			return r
		}()}, p: params(nil), want: "confidence low < minConfidence medium"},
		{name: "reverted", w: func() workload { w := base(); w.Annotations[AnnotationRightsizeReverted] = "t"; return w }, p: params(nil), want: "reverted by a human"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := tc.recs
			if r == nil {
				r = recs
			}
			wp := planWorkload(tc.w(), r, tc.auto, tc.vpaErr, tc.p)
			if wp == nil || len(wp.Containers) != 1 {
				t.Fatalf("want one container plan, got %+v", wp)
			}
			cp := wp.Containers[0]
			if tc.want == "" {
				if !cp.OK || cp.Change.Outcome != kubeherov1.ChangeOutcomePlanned {
					t.Fatalf("want pass, got %s: %s", cp.Change.Outcome, cp.Change.Reason)
				}
				return
			}
			if cp.OK || cp.Change.Outcome != kubeherov1.ChangeOutcomeBlocked {
				t.Fatalf("want blocked, got %s", cp.Change.Outcome)
			}
			if !strings.Contains(cp.Change.Reason, tc.want) {
				t.Errorf("reason = %q, want %q", cp.Change.Reason, tc.want)
			}
		})
	}
}

func TestBoundAndNoise(t *testing.T) {
	cases := []struct {
		cur, want, got int64
		clamped        bool
	}{
		{1000, 400, 500, true},
		{1000, 600, 600, false},
		{1000, 2500, 2000, true},
		{1000, 2000, 2000, false},
		{3, 1, 2, true}, // ceil keeps tiny values from rounding to zero
	}
	for _, c := range cases {
		got, note := bound(c.cur, c.want)
		if got != c.got || (note != "") != c.clamped {
			t.Errorf("bound(%d,%d) = %d %q, want %d clamped=%v", c.cur, c.want, got, note, c.got, c.clamped)
		}
	}
	if meaningful(1000, 1050, 5) {
		t.Error("5% move should be noise")
	}
	if !meaningful(1000, 850, 5) {
		t.Error("15% move should be meaningful")
	}
	if meaningful(40, 30, 16) {
		t.Error("move below the absolute floor should be noise")
	}
}

func TestEstimateSavings(t *testing.T) {
	if got := estimateSavings(100, []float64{1, 0.5}); got != 75 {
		t.Errorf("got %v want 75", got)
	}
	if got := estimateSavings(100, nil); got != 0 {
		t.Errorf("no wanted change should estimate 0, got %v", got)
	}
}

func TestHistoryAndCounters(t *testing.T) {
	now := time.Date(2026, 9, 28, 1, 0, 0, 0, time.UTC)
	h := parseHistory(map[string]string{AnnotationRightsizePrevious: "not json"})
	if len(h.Changes) != 0 || h.Version != 1 {
		t.Fatalf("garbage history should parse empty, got %+v", h)
	}
	for i := 0; i < maxHistory+3; i++ {
		h = h.withChange(RightsizeChange{ChangeID: newChangeID()})
	}
	if len(h.Changes) != maxHistory {
		t.Fatalf("history len = %d, want %d", len(h.Changes), maxHistory)
	}
	first := RightsizeChange{ChangeID: "rs-newest"}
	h = h.withChange(first)
	raw, err := h.marshal()
	if err != nil {
		t.Fatal(err)
	}
	back := parseHistory(map[string]string{AnnotationRightsizePrevious: raw})
	if back.Changes[0].ChangeID != "rs-newest" {
		t.Errorf("newest change should be first, got %s", back.Changes[0].ChangeID)
	}

	ann := map[string]string{
		dayKey(now.Add(-48 * time.Hour)): "4",
		dayKey(now):                      "2",
		"unrelated":                      "x",
	}
	if n := changesToday(ann, now); n != 2 {
		t.Errorf("changesToday = %d, want 2", n)
	}
	patch := counterPatch(ann, now)
	if v := patch[dayKey(now)]; v == nil || *v != "3" {
		t.Errorf("today's counter should become 3, got %v", v)
	}
	if v, ok := patch[dayKey(now.Add(-48*time.Hour))]; !ok || v != nil {
		t.Error("stale day key should be deleted (nil) in the patch")
	}
	if _, ok := patch["unrelated"]; ok {
		t.Error("unrelated annotations must not be touched")
	}
	if !strings.HasPrefix(newChangeID(), "rs-") || len(newChangeID()) != 11 {
		t.Errorf("unexpected change id shape %q", newChangeID())
	}
}

func TestResolveSpec(t *testing.T) {
	base := func(mut func(*kubeherov1.RightsizingPolicy)) *kubeherov1.RightsizingPolicy {
		rp := &kubeherov1.RightsizingPolicy{Spec: kubeherov1.RightsizingPolicySpec{Mode: "apply"}}
		if mut != nil {
			mut(rp)
		}
		return rp
	}
	got, err := resolveSpec(base(nil))
	if err != nil {
		t.Fatal(err)
	}
	if got.Window != "7d" || got.HeadroomPct != 15 || got.MinConfidence != "medium" ||
		got.MaxChangePerDay != 1 || got.MinReplicas != 1 {
		t.Errorf("defaults = %+v", got)
	}
	got, _ = resolveSpec(base(func(rp *kubeherov1.RightsizingPolicy) {
		rp.Spec.TargetUtilization = ptr.To[int32](70)
		rp.Spec.Safety.P95HeadroomPct = ptr.To[int32](5)
	}))
	if got.HeadroomPct < 42.8 || got.HeadroomPct > 42.9 {
		t.Errorf("targetUtilization 70 should win with ~42.86%% headroom, got %v", got.HeadroomPct)
	}
	bad := []func(*kubeherov1.RightsizingPolicy){
		func(rp *kubeherov1.RightsizingPolicy) { rp.Spec.Mode = "yolo" },
		func(rp *kubeherov1.RightsizingPolicy) { rp.Spec.MinConfidence = "certain" },
		func(rp *kubeherov1.RightsizingPolicy) { rp.Spec.Safety.ObservationWindow = "1w" },
		func(rp *kubeherov1.RightsizingPolicy) { rp.Spec.Safety.ObservationWindow = "91d" },
		func(rp *kubeherov1.RightsizingPolicy) { rp.Spec.Safety.P95HeadroomPct = ptr.To[int32](-1) },
		func(rp *kubeherov1.RightsizingPolicy) { rp.Spec.Safety.MaxChangePerDay = ptr.To[int32](-2) },
		func(rp *kubeherov1.RightsizingPolicy) {
			rp.Spec.Scope.NamespaceSelector = &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "a", Operator: "Bogus"}}}
		},
	}
	for i, mut := range bad {
		if _, err := resolveSpec(base(mut)); err == nil {
			t.Errorf("bad spec %d accepted", i)
		}
	}
}

func TestClusterScope(t *testing.T) {
	labels := ParseClusterLabels(" env=prod , cloud=aws,broken, =x", "eks-1")
	if labels["env"] != "prod" || labels["cloud"] != "aws" || labels["kubehero.io/cluster-id"] != "eks-1" || len(labels) != 3 {
		t.Fatalf("labels = %v", labels)
	}
	ok, err := ClusterInScope(nil, labels)
	if !ok || err != nil {
		t.Error("nil clusterSelector must match")
	}
	ok, _ = ClusterInScope(&metav1.LabelSelector{MatchLabels: map[string]string{"env": "prod"}}, labels)
	if !ok {
		t.Error("env=prod should match")
	}
	ok, _ = ClusterInScope(&metav1.LabelSelector{MatchLabels: map[string]string{"env": "dev"}}, labels)
	if ok {
		t.Error("env=dev should not match")
	}
}

// noMatchReader simulates a cluster without the VPA CRD.
type noMatchReader struct{ client.Reader }

func (noMatchReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "autoscaling.k8s.io", Kind: "VerticalPodAutoscaler"}}
}

type failingReader struct{ client.Reader }

func (failingReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return errors.New("forbidden")
}

func TestVPALookupToleratesMissingCRD(t *testing.T) {
	v := &vpaLookup{reader: noMatchReader{}}
	got, err := v.forNamespace(context.Background(), "ns")
	if err != nil || len(got) != 0 || !v.absent {
		t.Fatalf("missing CRD should be an empty, remembered result: %v %v %v", got, err, v.absent)
	}
	v = &vpaLookup{reader: failingReader{}}
	if _, err := v.forNamespace(context.Background(), "ns"); err == nil {
		t.Fatal("a failed lookup must surface so apply fails closed")
	}
}

// fakeCost implements the generated CostService handler for the client test.
type fakeCost struct {
	kuberov1connect.UnimplementedCostServiceHandler
	gotAuth string
	gotReq  *kuberov1.ListRightsizingRequest
}

func (f *fakeCost) ListRightsizing(
	_ context.Context,
	req *connect.Request[kuberov1.ListRightsizingRequest],
) (*connect.Response[kuberov1.ListRightsizingResponse], error) {
	f.gotAuth = req.Header().Get("Authorization")
	f.gotReq = req.Msg
	return connect.NewResponse(&kuberov1.ListRightsizingResponse{
		Source:          "live",
		Recommendations: []*kuberov1.RightsizingRecommendation{{Workload: "api", Container: "app", Confidence: "high"}},
	}), nil
}

func TestControlPlaneRightsizingClient(t *testing.T) {
	fake := &fakeCost{}
	mux := http.NewServeMux()
	mux.Handle(kuberov1connect.NewCostServiceHandler(fake))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	src := NewControlPlaneRightsizing(srv.URL+"/", "tok-123")
	resp, err := src.ListRightsizing(context.Background(), &kuberov1.ListRightsizingRequest{
		ClusterId: "eks-1", Namespace: "payments", Window: "7d", HeadroomPct: 25,
	})
	if err != nil {
		t.Fatal(err)
	}
	if fake.gotAuth != "Bearer tok-123" {
		t.Errorf("Authorization = %q, want the bearer token", fake.gotAuth)
	}
	if fake.gotReq.GetNamespace() != "payments" || fake.gotReq.GetHeadroomPct() != 25 {
		t.Errorf("request = %+v", fake.gotReq)
	}
	if len(resp.GetRecommendations()) != 1 || resp.GetSource() != "live" {
		t.Errorf("response = %+v", resp)
	}

	// No token: no header at all (anonymous dev control planes).
	src = NewControlPlaneRightsizing(srv.URL, "")
	if _, err := src.ListRightsizing(context.Background(), &kuberov1.ListRightsizingRequest{}); err != nil {
		t.Fatal(err)
	}
	if fake.gotAuth != "" {
		t.Errorf("Authorization = %q, want none without a token", fake.gotAuth)
	}
}

func TestPlanHashIgnoresBlocked(t *testing.T) {
	planned := kubeherov1.PlannedChange{Namespace: "a", Workload: "w", Kind: "Deployment", Container: "c",
		Outcome:    kubeherov1.ChangeOutcomePlanned,
		CPURequest: &kubeherov1.QuantityChange{From: resource.MustParse("2"), To: resource.MustParse("1")}}
	blocked := planned
	blocked.Outcome = kubeherov1.ChangeOutcomeBlocked
	blocked.Workload = "other"
	h1 := planHash([]kubeherov1.PlannedChange{planned})
	h2 := planHash([]kubeherov1.PlannedChange{blocked, planned})
	if h1 == "" || h1 != h2 {
		t.Errorf("hash should only cover planned changes: %q vs %q", h1, h2)
	}
	if planHash([]kubeherov1.PlannedChange{blocked}) != "" {
		t.Error("nothing planned should hash to empty")
	}
	b, _ := json.Marshal(planned)
	if !strings.Contains(string(b), `"cpuRequest":{"from":"2","to":"1"}`) {
		t.Errorf("unexpected wire shape %s", b)
	}
}
