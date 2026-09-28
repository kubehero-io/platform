// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package opencost

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kubehero-io/platform/services/control-plane/internal/auth"
	"github.com/kubehero-io/platform/services/control-plane/internal/cost"
)

// openCostFixture is one /allocation/compute response in OpenCost's
// exact shape: the envelope of pkg/costmodel WrapData and an allocation
// with every key AllocationJSON (core/pkg/opencost/allocation_json.go)
// emits, with the JSON kinds OpenCost produces (numbers from *float64,
// null for the nil pvs / lbAllocations / gpuAllocation maps). Upstream
// docs elide the fields, so the fixture is reconstructed from the
// struct tags rather than captured from a live cluster.
const openCostFixture = `{
  "code": 200,
  "status": "success",
  "data": [
    {
      "kube-system": {
        "name": "kube-system",
        "properties": {"cluster": "cluster-one", "namespace": "kube-system"},
        "window": {"start": "2026-09-29T00:00:00Z", "end": "2026-09-30T00:00:00Z"},
        "start": "2026-09-29T00:00:00Z",
        "end": "2026-09-30T00:00:00Z",
        "minutes": 1440,
        "cpuCores": 0.53,
        "cpuCoreRequestAverage": 0.449881,
        "cpuCoreLimitAverage": 0.2,
        "cpuCoreUsageAverage": 0.009417,
        "cpuCoreHours": 12.72,
        "cpuCost": 0.403184,
        "cpuCostAdjustment": 0,
        "cpuCostIdle": 0,
        "cpuEfficiency": 0.020932,
        "gpuCount": 0,
        "gpuHours": 0,
        "gpuCost": 0,
        "gpuCostAdjustment": 0,
        "gpuCostIdle": 0,
        "gpuEfficiency": 0,
        "networkTransferBytes": 0,
        "networkReceiveBytes": 0,
        "networkCost": 0,
        "networkCrossZoneCost": 0,
        "networkCrossRegionCost": 0,
        "networkInternetCost": 0,
        "networkNatGatewayEgressCost": 0,
        "networkNatGatewayIngressCost": 0,
        "networkCostAdjustment": 0,
        "loadBalancerCost": 0,
        "loadBalancerCostAdjustment": 0,
        "pvBytes": 0,
        "pvByteHours": 0,
        "pvCost": 0,
        "pvs": null,
        "pvCostAdjustment": 0,
        "ramBytes": 146761671.651553,
        "ramByteRequestAverage": 146761671.651553,
        "ramByteLimitAverage": 356515840,
        "ramByteUsageAverage": 120456123.1,
        "ramByteHours": 3522280119.637272,
        "ramCost": 0.016387,
        "ramCostAdjustment": 0,
        "ramCostIdle": 0,
        "ramEfficiency": 0.820753,
        "externalCost": 0,
        "sharedCost": 0,
        "totalCost": 0.419571,
        "totalEfficiency": 0.051865,
        "lbAllocations": null,
        "gpuAllocation": null
      }
    }
  ]
}`

var now = time.Date(2026, 9, 30, 14, 25, 0, 0, time.UTC)

func handler() *Handler {
	return &Handler{Cost: &cost.Service{Now: func() time.Time { return now }}, Now: func() time.Time { return now }}
}

type envelope struct {
	Code    int                                     `json:"code"`
	Status  string                                  `json:"status"`
	Data    []map[string]map[string]json.RawMessage `json:"data"`
	Warning string                                  `json:"warning"`
	Message string                                  `json:"message"`
}

func get(t *testing.T, h http.Handler, query string) (*httptest.ResponseRecorder, envelope) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/allocation/compute?"+query, nil)
	req = req.WithContext(auth.WithPrincipal(req.Context(), auth.Principal{Role: auth.RoleViewer}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var env envelope
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("decode: %v\n%s", err, rec.Body.String())
		}
	}
	return rec, env
}

// jsonKinds maps each key of a JSON object to its JSON kind.
func jsonKinds(t *testing.T, obj map[string]json.RawMessage) map[string]string {
	t.Helper()
	out := map[string]string{}
	for k, raw := range obj {
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatal(err)
		}
		switch v.(type) {
		case nil:
			out[k] = "null"
		case float64:
			out[k] = "number"
		case string:
			out[k] = "string"
		case map[string]any:
			out[k] = "object"
		case []any:
			out[k] = "array"
		case bool:
			out[k] = "bool"
		}
	}
	return out
}

func TestResponseMatchesOpenCostShape(t *testing.T) {
	var want envelope
	if err := json.Unmarshal([]byte(openCostFixture), &want); err != nil {
		t.Fatal(err)
	}
	wantKinds := jsonKinds(t, want.Data[0]["kube-system"])

	rec, got := get(t, handler(), "window=yesterday&aggregate=namespace&accumulate=true")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if got.Code != 200 || got.Status != "success" || len(got.Data) != 1 {
		t.Fatalf("envelope: code=%d status=%q sets=%d", got.Code, got.Status, len(got.Data))
	}
	if rec.Header().Get("Content-Type") != "application/json" || rec.Header().Get("X-KubeHero-Source") != "demo" || got.Warning == "" {
		t.Fatal("demo responses must be labelled")
	}
	alloc, ok := got.Data[0]["kube-system"]
	if !ok {
		t.Fatalf("no kube-system allocation; keys %v", keysOf(got.Data[0]))
	}
	gotKinds := jsonKinds(t, alloc)
	for k, kind := range wantKinds {
		if gotKinds[k] != kind {
			t.Errorf("field %q: kind %q, OpenCost emits %q", k, gotKinds[k], kind)
		}
	}
	for k := range gotKinds {
		if _, ok := wantKinds[k]; !ok {
			t.Errorf("field %q is not part of OpenCost's AllocationJSON", k)
		}
	}
	var props map[string]string
	_ = json.Unmarshal(alloc["properties"], &props)
	if props["namespace"] != "kube-system" {
		t.Fatalf("properties = %v", props)
	}
	var w map[string]string
	_ = json.Unmarshal(alloc["window"], &w)
	if w["start"] != "2026-09-29T00:00:00Z" || w["end"] != "2026-09-30T00:00:00Z" {
		t.Fatalf("window = %v", w)
	}
}

func TestStepsIdleKeysAndControllers(t *testing.T) {
	h := handler()
	// accumulate=false: one set per day.
	_, env := get(t, h, "window=7d&aggregate=namespace")
	if len(env.Data) != 7 {
		t.Fatalf("7d unaccumulated = %d sets, want 7", len(env.Data))
	}
	// Idle across clusters merges into one __idle__ …
	_, env = get(t, h, "window=yesterday&aggregate=namespace&accumulate=true&includeIdle=true")
	if _, ok := env.Data[0]["__idle__"]; !ok {
		t.Fatalf("want merged __idle__, keys %v", keysOf(env.Data[0]))
	}
	// … and stays per cluster when aggregating by cluster.
	_, env = get(t, h, "window=yesterday&aggregate=cluster&accumulate=true&idle=true")
	if _, ok := env.Data[0]["eks-use1-prod/__idle__"]; !ok {
		t.Fatalf("want per-cluster idle, keys %v", keysOf(env.Data[0]))
	}
	_, env = get(t, h, "window=yesterday&aggregate=controller&accumulate=true")
	if _, ok := env.Data[0]["deployment:frontend-gateway"]; !ok {
		t.Fatalf("controller keys must be kind:name, got %v", keysOf(env.Data[0]))
	}
	var props map[string]string
	_ = json.Unmarshal(env.Data[0]["statefulset:prometheus"]["properties"], &props)
	if props["controllerKind"] != "statefulset" || props["controller"] != "prometheus" {
		t.Fatalf("controller properties = %v", props)
	}
	// Shared idle folds into cpu/ram cost and reports the idle part.
	_, env = get(t, h, "window=yesterday&aggregate=namespace&accumulate=true&shareIdle=true")
	if _, ok := env.Data[0]["__idle__"]; ok {
		t.Fatal("shared idle must not appear as a row")
	}
	var idle float64
	_ = json.Unmarshal(env.Data[0]["edge"]["cpuCostIdle"], &idle)
	if idle <= 0 {
		t.Fatal("cpuCostIdle must carry the shared idle")
	}
}

func TestTotalsMatchConnectAPI(t *testing.T) {
	h := handler()
	const query = "window=yesterday&aggregate=namespace&accumulate=true&includeIdle=true"
	_, env := get(t, h, query)
	var sum float64
	for _, a := range env.Data[0] {
		var v float64
		_ = json.Unmarshal(a["totalCost"], &v)
		sum += v
	}
	q, _, err := h.parse(httptest.NewRequest(http.MethodGet, "/allocation?"+query, nil))
	if err != nil {
		t.Fatal(err)
	}
	alloc, _, _ := h.Cost.Allocator("t")
	res, err := alloc.Allocate(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	// Rounding to six decimals per row bounds the difference.
	if want := res[0].Total.TotalCost(); math.Abs(sum-want) > 1e-4 {
		t.Fatalf("Σ totalCost %v != engine total %v", sum, want)
	}
}

func TestFiltersAndErrors(t *testing.T) {
	h := handler()
	_, env := get(t, h, "window=yesterday&aggregate=namespace&accumulate=true&filterNamespaces=checkout,edge")
	if len(env.Data[0]) != 2 {
		t.Fatalf("filterNamespaces: %v", keysOf(env.Data[0]))
	}
	_, env = get(t, h, "window=yesterday&aggregate=controller&accumulate=true&filterControllers=deployment:cart")
	if len(env.Data[0]) != 1 {
		t.Fatalf("filterControllers: %v", keysOf(env.Data[0]))
	}
	_, env = get(t, h, "window=yesterday&aggregate=label:app&accumulate=true&filterLabels=app:checkout")
	if _, ok := env.Data[0]["checkout"]; !ok || len(env.Data[0]) != 1 {
		t.Fatalf("filterLabels: %v", keysOf(env.Data[0]))
	}
	for _, q := range []string{
		"aggregate=namespace",                     // window required
		"window=nope",                             // bad window
		"window=1d&aggregate=service",             // unsupported
		"window=1d&filter=namespace:%22x%22",      // v2 filters rejected
		"window=1d&accumulate=maybe",              // bad bool
		"window=1d&filterLabels=novalue",          // malformed label filter
		"window=1d&shareIdle=true&shareSplit=odd", // bad split
	} {
		rec, _ := get(t, h, q)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", q, rec.Code)
		}
		var e map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || e["status"] != "error" {
			t.Errorf("%s: error envelope %s", q, rec.Body.String())
		}
	}
	off := &Handler{Cost: &cost.Service{DemoDisabled: true}}
	if rec, _ := get(t, off, "window=1d"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("demo disabled: %d", rec.Code)
	}
}

func TestNumRounding(t *testing.T) {
	if v := num(1.23456789); *v != 1.234568 {
		t.Fatalf("round: %v", *v)
	}
	if num(math.NaN()) != nil || num(math.Inf(1)) != nil {
		t.Fatal("NaN/Inf must be null")
	}
	if KindFromOpenCost("statefulset") != "StatefulSet" || KindFromOpenCost("rollout") != "Rollout" || KindFromOpenCost("widget") != "Widget" {
		t.Fatal("kind mapping")
	}
	if !strings.HasPrefix(Paths[0], "/allocation") {
		t.Fatal("paths")
	}
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
