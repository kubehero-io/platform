// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package profiles

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/services/control-plane/internal/auth"
)

var now = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

func viewer() context.Context {
	return auth.WithPrincipal(context.Background(), auth.Principal{Role: auth.RoleViewer})
}

func demo() *Service { return &Service{Now: func() time.Time { return now }} }

func code(err error) connect.Code {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return ce.Code()
	}
	return connect.CodeUnknown
}

func TestDemoTargets(t *testing.T) {
	r, err := demo().ListProfileTargets(viewer(), connect.NewRequest(&kuberov1.ListProfileTargetsRequest{}))
	if err != nil || len(r.Msg.GetTargets()) != 5 {
		t.Fatalf("targets: %v %d", err, len(r.Msg.GetTargets()))
	}
	r, _ = demo().ListProfileTargets(viewer(), connect.NewRequest(&kuberov1.ListProfileTargetsRequest{Namespace: "payments"}))
	if len(r.Msg.GetTargets()) != 1 || r.Msg.GetTargets()[0].GetOrigin() != "pprof-scrape" {
		t.Fatalf("namespace filter: %+v", r.Msg.GetTargets())
	}
}

func TestDemoFlamegraphAndDiff(t *testing.T) {
	sel := &kuberov1.ProfileSelector{Service: "checkout-api"}
	r, err := demo().GetFlamegraph(viewer(), connect.NewRequest(&kuberov1.GetFlamegraphRequest{Selector: sel}))
	if err != nil {
		t.Fatal(err)
	}
	m := r.Msg
	if m.GetNodes()[0].GetName() != "total" || m.GetNodes()[0].GetTotal() != m.GetTotal() || m.GetUnit() != "nanoseconds" ||
		m.GetCostUsdMonth() != demoCPUCostMonth || m.GetType() != "cpu" {
		t.Fatalf("flamegraph header: total %d unit %q cost %v", m.GetTotal(), m.GetUnit(), m.GetCostUsdMonth())
	}
	diff, err := demo().GetFlamegraph(viewer(), connect.NewRequest(&kuberov1.GetFlamegraphRequest{Selector: sel,
		StartUnixMs: now.Add(-time.Hour).UnixMilli(), EndUnixMs: now.UnixMilli(),
		BaselineStartUnixMs: now.Add(-25 * time.Hour).UnixMilli(), BaselineEndUnixMs: now.Add(-24 * time.Hour).UnixMilli()}))
	if err != nil {
		t.Fatal(err)
	}
	// The checkout path regressed; cart's identical work shrinks in
	// share once the baseline is normalised to the new total.
	nodes := diff.Msg.GetNodes()
	var marshal *kuberov1.FlameNode
	for _, n := range nodes {
		if n.GetName() == "encoding/json.Marshal" && nodes[n.GetParent()].GetName() == "main.(*api).handleCheckout" {
			marshal = n
		}
	}
	if marshal == nil || marshal.GetBaselineTotal() >= marshal.GetTotal() {
		t.Fatalf("the demo regression must light up: %+v", marshal)
	}
	if diff.Msg.GetNodes()[0].GetBaselineTotal() != diff.Msg.GetTotal() {
		t.Fatal("baseline must be normalised to the same total")
	}
	small, _ := demo().GetFlamegraph(viewer(), connect.NewRequest(&kuberov1.GetFlamegraphRequest{Selector: sel, MaxNodes: 16}))
	if n := len(small.Msg.GetNodes()); n > 16 {
		t.Fatalf("max_nodes: %d", n)
	}
}

func TestDemoTopFunctions(t *testing.T) {
	r, err := demo().GetTopFunctions(viewer(), connect.NewRequest(&kuberov1.GetTopFunctionsRequest{
		Selector: &kuberov1.ProfileSelector{Service: "checkout-api"}, Limit: 5}))
	if err != nil {
		t.Fatal(err)
	}
	fs := r.Msg.GetFunctions()
	if len(fs) != 5 || fs[0].GetName() != "encoding/json.stringEncoder" {
		t.Fatalf("top self: %+v", fs)
	}
	var costSum float64
	for i, f := range fs {
		if i > 0 && f.GetSelf() > fs[i-1].GetSelf() {
			t.Fatal("not sorted by self")
		}
		costSum += f.GetSelfCostUsdMonth()
	}
	if costSum <= 0 || costSum > demoCPUCostMonth {
		t.Fatalf("self cost %v", costSum)
	}
	tot, _ := demo().GetTopFunctions(viewer(), connect.NewRequest(&kuberov1.GetTopFunctionsRequest{
		Selector: &kuberov1.ProfileSelector{Service: "checkout-api"}, OrderBy: "total", Limit: 1}))
	if f := tot.Msg.GetFunctions()[0]; f.GetName() != "runtime.goexit" || f.GetTotalPct() < 50 {
		t.Fatalf("top total: %+v", f)
	}
}

func TestProfilesValidation(t *testing.T) {
	s := demo()
	bad := []func() error{
		func() error {
			_, err := s.GetFlamegraph(viewer(), connect.NewRequest(&kuberov1.GetFlamegraphRequest{}))
			return err
		},
		func() error {
			_, err := s.GetFlamegraph(viewer(), connect.NewRequest(&kuberov1.GetFlamegraphRequest{
				Selector: &kuberov1.ProfileSelector{Service: "x", Type: "CPU; DROP"}}))
			return err
		},
		func() error {
			_, err := s.GetTopFunctions(viewer(), connect.NewRequest(&kuberov1.GetTopFunctionsRequest{
				Selector: &kuberov1.ProfileSelector{Service: "x"}, OrderBy: "random"}))
			return err
		},
		func() error {
			_, err := s.ListProfileTargets(viewer(), connect.NewRequest(&kuberov1.ListProfileTargetsRequest{
				StartUnixMs: now.Add(-30 * 24 * time.Hour).UnixMilli()}))
			return err
		},
		func() error {
			_, err := s.GetFlamegraph(viewer(), connect.NewRequest(&kuberov1.GetFlamegraphRequest{
				Selector: &kuberov1.ProfileSelector{Service: "x", Labels: map[string]string{"bad key!": "v"}}}))
			return err
		},
	}
	for i, f := range bad {
		if err := f(); code(err) != connect.CodeInvalidArgument {
			t.Errorf("case %d: want InvalidArgument, got %v", i, err)
		}
	}
	off := &Service{DemoDisabled: true}
	if _, err := off.ListProfileTargets(viewer(), connect.NewRequest(&kuberov1.ListProfileTargetsRequest{})); code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("demo disabled: %v", err)
	}
	if _, err := s.GetFlamegraph(context.Background(), connect.NewRequest(&kuberov1.GetFlamegraphRequest{})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("anonymous: %v", err)
	}
	if !strings.HasPrefix(otherStacks, "[") {
		t.Fatal()
	}
}
