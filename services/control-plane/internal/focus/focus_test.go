// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package focus

import (
	"encoding/csv"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kubehero-io/platform/services/control-plane/internal/auth"
	"github.com/kubehero-io/platform/services/control-plane/internal/clusters"
	"github.com/kubehero-io/platform/services/control-plane/internal/cost"
	"github.com/kubehero-io/platform/services/control-plane/internal/timewin"
)

var now = time.Date(2026, 9, 30, 14, 25, 0, 0, time.UTC)

func handler() *Handler {
	clock := func() time.Time { return now }
	return &Handler{Cost: &cost.Service{Now: clock}, Now: clock}
}

func export(t *testing.T, h *Handler, query string) (*httptest.ResponseRecorder, [][]string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, Path+"?"+query, nil)
	req = req.WithContext(auth.WithPrincipal(req.Context(), auth.Principal{Role: auth.RoleViewer}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		return rec, nil
	}
	rows, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
	if err != nil {
		t.Fatalf("CSV does not parse: %v", err)
	}
	return rec, rows
}

func col(name string) int {
	for i, c := range Columns {
		if c == name {
			return i
		}
	}
	panic(name)
}

func TestExportHeaderRowsAndTotals(t *testing.T) {
	rec, rows := export(t, handler(), "window=3d&aggregate=workload")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Fatalf("content type %q", ct)
	}
	if !strings.Contains(rec.Header().Get("Content-Disposition"), "kubehero-focus-20260928-20260930.csv") {
		t.Fatalf("disposition %q", rec.Header().Get("Content-Disposition"))
	}
	if strings.Join(rows[0], ",") != strings.Join(Columns, ",") {
		t.Fatalf("header = %v", rows[0])
	}
	var billed float64
	days := map[string]bool{}
	sawIdle, sawNet, sawSpot := false, false, false
	for _, r := range rows[1:] {
		if len(r) != len(Columns) {
			t.Fatalf("row has %d columns, header %d", len(r), len(Columns))
		}
		v, err := strconv.ParseFloat(r[col("BilledCost")], 64)
		if err != nil {
			t.Fatalf("BilledCost %q", r[col("BilledCost")])
		}
		billed += v
		if r[col("EffectiveCost")] != r[col("BilledCost")] || r[col("ContractedCost")] != r[col("EffectiveCost")] {
			t.Fatal("allocated estimate must fill Billed/Effective/List/Contracted identically")
		}
		days[r[col("ChargePeriodStart")]] = true
		var tags map[string]string
		if err := json.Unmarshal([]byte(r[col("Tags")]), &tags); err != nil || tags["k8s.cluster"] == "" {
			t.Fatalf("Tags must be a JSON object with k8s.cluster: %q", r[col("Tags")])
		}
		switch {
		case r[col("ResourceType")] == "Kubernetes Idle Capacity":
			sawIdle = true
		case r[col("ServiceCategory")] == "Networking":
			sawNet = true
			if r[col("PricingCategory")] != "" {
				t.Fatal("network rows carry no node pricing category")
			}
		default:
			if r[col("ServiceCategory")] != "Compute" || r[col("ConsumedUnit")] != "Core-Hours" ||
				r[col("InvoiceIssuerName")] != "KubeHero (allocated)" || r[col("x_CostSource")] != "KubeHero demo data" {
				t.Fatalf("compute row: %v", r)
			}
			if r[col("PricingCategory")] == "Dynamic" {
				sawSpot = true
			}
		}
		if r[col("BillingCurrency")] != "USD" || r[col("ChargeCategory")] != "Usage" || r[col("ChargeFrequency")] != "Usage-Based" {
			t.Fatal("fixed FOCUS values")
		}
	}
	if len(days) != 3 || !sawIdle || !sawNet || !sawSpot {
		t.Fatalf("days=%d idle=%v net=%v spot=%v", len(days), sawIdle, sawNet, sawSpot)
	}

	// Every exported dollar reconciles with the allocation engine.
	alloc, _, _ := handler().Cost.Allocator("t")
	w, _ := timewin.Parse("3d", now)
	var want float64
	for day := w.Start; day.Before(w.QueryEnd()); day = day.Add(timewin.Day) {
		end := day.Add(timewin.Day)
		if end.After(now) {
			end = now
		}
		sets, err := alloc.Allocate(t.Context(), cost.AllocationQuery{
			Window: timewin.Window{Start: day, End: end, Now: now}, Dims: aggregations["workload"].dims, IncludeIdle: true})
		if err != nil {
			t.Fatal(err)
		}
		want += sets[0].Total.TotalCost()
	}
	if math.Abs(billed-want) > 1e-6 {
		t.Fatalf("Σ BilledCost %v != allocation total %v", billed, want)
	}
}

func TestExportParamsAndErrors(t *testing.T) {
	h := handler()
	_, ns := export(t, h, "window=yesterday&aggregate=namespace&idle=none")
	for _, r := range ns[1:] {
		if r[col("ResourceType")] == "Kubernetes Idle Capacity" {
			t.Fatal("idle=none must not export idle rows")
		}
		if r[col("ServiceCategory")] == "Compute" && r[col("ResourceType")] != "Kubernetes Namespace" {
			t.Fatalf("namespace aggregation resource type: %v", r[col("ResourceType")])
		}
	}
	_, shared := export(t, h, "window=yesterday&aggregate=cluster&idle=share")
	var idleCol float64
	for _, r := range shared[1:] {
		v, _ := strconv.ParseFloat(r[col("x_IdleCost")], 64)
		idleCol += v
	}
	if idleCol <= 0 {
		t.Fatal("idle=share must carry shared idle in x_IdleCost")
	}
	for _, q := range []string{"aggregate=pod", "window=400d", "idle=maybe", "window=bogus"} {
		if rec, _ := export(t, h, q); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d", q, rec.Code)
		}
	}
	off := &Handler{Cost: &cost.Service{DemoDisabled: true}}
	if rec, _ := export(t, off, ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("demo disabled: %d", rec.Code)
	}
	req := httptest.NewRequest(http.MethodGet, Path+"?cluster_id=gke-usc1-prod", nil)
	req = req.WithContext(auth.WithPrincipal(req.Context(), auth.Principal{Role: auth.RoleMember, ClusterID: "eks-use1-prod"}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cluster token exporting another cluster: %d", rec.Code)
	}
}

func TestRowsEscapingAndMappings(t *testing.T) {
	a := &cost.Alloc{Name: "c/ns/wl", Key: []string{"c", "ns", "wl", "Deployment", "team, \"quoted\"", "", "aws", "us-east-1", "us-east-1a", "spot"},
		Props: map[string]string{}}
	a.Cost, a.CPUCost, a.RAMCost, a.NetCost, a.NetInternetCost = 10, 6, 4, 2, 2
	rows := Rows(a, aggregations["workload"], now.Truncate(timewin.Day), now, cost.SourceLive,
		clusters.NewSnapshot([]clusters.Info{{ID: "c", Slug: "c", Name: "Prod East"}}), nil)
	if len(rows) != 2 {
		t.Fatalf("want compute + network rows, got %d", len(rows))
	}
	r := rows[0]
	if r[col("ProviderName")] != "AWS" || r[col("PricingCategory")] != "Dynamic" || r[col("SubAccountName")] != "Prod East" ||
		r[col("ResourceId")] != "c/ns/wl" || r[col("AvailabilityZone")] != "us-east-1a" {
		t.Fatalf("mappings: %v", r)
	}
	var sb strings.Builder
	w := csv.NewWriter(&sb)
	_ = w.WriteAll(rows)
	back, err := csv.NewReader(strings.NewReader(sb.String())).ReadAll()
	if err != nil || back[0][col("Tags")] != r[col("Tags")] {
		t.Fatalf("CSV round trip must preserve quotes/commas: %v", err)
	}
	var tags map[string]string
	_ = json.Unmarshal([]byte(back[0][col("Tags")]), &tags)
	if tags["kubehero.io/team"] != `team, "quoted"` {
		t.Fatalf("tags = %v", tags)
	}
	for in, want := range map[string]string{"gcp": "Google Cloud", "azure": "Microsoft Azure", "": "Kubernetes"} {
		if providerName(in) != want {
			t.Errorf("provider %q", in)
		}
	}
	if pricingCategory("savings-plan") != "Committed" || pricingCategory("unknown") != "" {
		t.Error("pricing category")
	}
}
