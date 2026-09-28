// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package pyroscope

import (
	"bytes"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/pprof/profile"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/services/control-plane/internal/auth"
	"github.com/kubehero-io/platform/services/control-plane/internal/compat/httpauth"
	"github.com/kubehero-io/platform/services/control-plane/internal/telemetry"
)

func TestParseName(t *testing.T) {
	tests := []struct {
		in, app, typ string
		labels       map[string]string
		err          bool
	}{
		{in: "myapp.cpu", app: "myapp", typ: "cpu"},
		{in: "my.dotted.app.alloc_space{env=prod, pod=api-1}", app: "my.dotted.app", typ: "alloc_space", labels: map[string]string{"env": "prod", "pod": "api-1"}},
		{in: "plainapp", app: "plainapp"},
		{in: "svc.notatype", app: "svc.notatype"},
		{in: `app.wall{k="quoted"}`, app: "app", typ: "wall", labels: map[string]string{"k": "quoted"}},
		{in: "", err: true},
		{in: "app{broken", err: true},
		{in: "app{novalue}", err: true},
		{in: ".cpu", err: true},
	}
	for _, tc := range tests {
		app, typ, labels, err := ParseName(tc.in)
		if tc.err {
			if err == nil {
				t.Errorf("%q: want error", tc.in)
			}
			continue
		}
		if err != nil || app != tc.app || typ != tc.typ || fmt.Sprint(labels) != fmt.Sprint(nonNil(tc.labels)) {
			t.Errorf("%q = %q %q %v %v", tc.in, app, typ, labels, err)
		}
	}
}

func nonNil(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

type fakeWriter struct {
	mu       sync.Mutex
	profiles map[string][]*kuberov1.Profile
}

func (f *fakeWriter) WriteProfiles(cluster string, ps []*kuberov1.Profile) (telemetry.ProfileWrite, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.profiles == nil {
		f.profiles = map[string][]*kuberov1.Profile{}
	}
	f.profiles[cluster] = append(f.profiles[cluster], ps...)
	return telemetry.ProfileWrite{Profiles: len(ps)}, nil
}

func newHandler(w *fakeWriter) *Handler {
	a := httpauth.New(connect.WithInterceptors(auth.NewInterceptor(auth.Config{APIKeys: []string{"k:member"}})))
	return &Handler{Writer: w, Auth: a, DefaultCluster: "default", Now: func() time.Time { return time.Unix(1790596800, 0) }}
}

func ingest(h http.Handler, query string, body []byte, contentType string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/ingest?"+query, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer k")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func stacks(p *kuberov1.Profile) map[string]int64 {
	out := map[string]int64{}
	for _, s := range p.GetSamples() {
		out[strings.Join(s.GetFrames(), ";")] += s.GetValue()
	}
	return out
}

func TestFoldedIngest(t *testing.T) {
	w := &fakeWriter{}
	h := newHandler(w)
	body := "main;handler;json.Marshal 3\nmain;handler;db.Query 5\nmain;handler;json.Marshal 2\n\n"
	rec := ingest(h, "name=checkout.cpu{namespace=shop,pod=checkout-1,env=prod,cluster=c7}&from=1790596800&until=1790596810&sampleRate=100&spyName=gospy&units=samples&format=folded", []byte(body), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	ps := w.profiles["c7"]
	if len(ps) != 1 {
		t.Fatalf("profiles = %v", w.profiles)
	}
	p := ps[0]
	if p.GetType() != "cpu" || p.GetUnit() != "nanoseconds" || p.GetService() != "checkout" || p.GetOrigin() != "pyroscope" ||
		p.GetSource().GetNamespace() != "shop" || p.GetSource().GetPod() != "checkout-1" || p.GetLabels()["env"] != "prod" ||
		p.GetLabels()["spy"] != "gospy" || p.GetTsUnixNano() != 1790596800e9 || p.GetDurationNano() != 10e9 || p.GetPeriod() != 1e7 {
		t.Fatalf("profile = %+v", p)
	}
	// 100 Hz: one sample = 10ms
	got := stacks(p)
	if got["main;handler;json.Marshal"] != 5e7 || got["main;handler;db.Query"] != 5e7 {
		t.Fatalf("stacks = %v", got)
	}

	rec = ingest(h, "name=svc.alloc_objects&format=lines", []byte("a;b\na;b\na;c\n"), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("lines: %d %s", rec.Code, rec.Body)
	}
	lp := w.profiles["default"][0]
	if lp.GetType() != "alloc_objects" || lp.GetUnit() != "count" || stacks(lp)["a;b"] != 2 || lp.GetTsUnixNano() != 1790596800e9 {
		t.Fatalf("lines profile = %+v", lp)
	}

	for _, tc := range []struct {
		query, body string
		code        int
	}{
		{"name=x&format=folded", "a;b notanumber", http.StatusBadRequest},
		{"name=x&format=folded", "a;b", http.StatusBadRequest},
		{"name=x&format=trie", "x", http.StatusUnsupportedMediaType},
		{"format=folded", "a 1", http.StatusBadRequest},
		{"name=x&from=10&until=5", "a 1", http.StatusBadRequest},
		{"name=x&sampleRate=-1", "a 1", http.StatusBadRequest},
		{"name=x&format=pprof", "not a profile", http.StatusBadRequest},
	} {
		if rec := ingest(h, tc.query, []byte(tc.body), ""); rec.Code != tc.code {
			t.Errorf("%s %q: %d %s, want %d", tc.query, tc.body, rec.Code, rec.Body, tc.code)
		}
	}
}

// goProfile builds a small pprof with an inlined frame:
// main → handler → (json.Marshal inlined into encode).
func goProfile(t *testing.T, types []*profile.ValueType, values func(i int) []int64) []byte {
	t.Helper()
	fn := func(id uint64, name string) *profile.Function { return &profile.Function{ID: id, Name: name} }
	fMain, fHandler, fEncode, fMarshal, fQuery := fn(1, "main.main"), fn(2, "main.handler"), fn(3, "main.encode"), fn(4, "encoding/json.Marshal"), fn(5, "database/sql.(*DB).Query")
	locMain := &profile.Location{ID: 1, Address: 0x10, Line: []profile.Line{{Function: fMain}}}
	locHandler := &profile.Location{ID: 2, Address: 0x20, Line: []profile.Line{{Function: fHandler}}}
	// Line[0] is the innermost (inlined) function.
	locEncode := &profile.Location{ID: 3, Address: 0x30, Line: []profile.Line{{Function: fMarshal}, {Function: fEncode}}}
	locQuery := &profile.Location{ID: 4, Address: 0x40, Line: []profile.Line{{Function: fQuery}}}
	locRaw := &profile.Location{ID: 5, Address: 0xdead}
	p := &profile.Profile{
		SampleType:    types,
		PeriodType:    &profile.ValueType{Type: "cpu", Unit: "nanoseconds"},
		Period:        10_000_000,
		TimeNanos:     1790596700e9,
		DurationNanos: 15e9,
		Function:      []*profile.Function{fMain, fHandler, fEncode, fMarshal, fQuery},
		Location:      []*profile.Location{locMain, locHandler, locEncode, locQuery, locRaw},
		Sample: []*profile.Sample{
			{Location: []*profile.Location{locEncode, locHandler, locMain}, Value: values(0)},
			{Location: []*profile.Location{locQuery, locHandler, locMain}, Value: values(1)},
			{Location: []*profile.Location{locRaw, locMain}, Value: values(2)},
		},
	}
	var buf bytes.Buffer
	if err := p.Write(&buf); err != nil { // gzipped
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestPprofIngest(t *testing.T) {
	w := &fakeWriter{}
	h := newHandler(w)
	cpu := goProfile(t, []*profile.ValueType{{Type: "samples", Unit: "count"}, {Type: "cpu", Unit: "nanoseconds"}},
		func(i int) []int64 { return [][]int64{{3, 3e7}, {1, 1e7}, {2, 2e7}}[i] })
	rec := ingest(h, "name=api.cpu{namespace=shop}&format=pprof", cpu, "application/octet-stream")
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	ps := w.profiles["default"]
	if len(ps) != 1 || ps[0].GetType() != "cpu" || ps[0].GetUnit() != "nanoseconds" {
		t.Fatalf("profiles = %+v (the samples/count type must not be stored twice)", ps)
	}
	got := stacks(ps[0])
	want := map[string]int64{
		"main.main;main.handler;main.encode;encoding/json.Marshal": 3e7,
		"main.main;main.handler;database/sql.(*DB).Query":          1e7,
		"main.main;0xdead": 2e7,
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("stacks = %v, want %v", got, want)
	}
	if ps[0].GetTsUnixNano() != 1790596700e9 || ps[0].GetDurationNano() != 15e9 {
		t.Fatalf("pprof time/duration not used: %d %d", ps[0].GetTsUnixNano(), ps[0].GetDurationNano())
	}

	// Heap upload as the SDKs send it: multipart with a previous
	// profile; alloc_* are cumulative, inuse_* are not.
	heapTypes := []*profile.ValueType{{Type: "alloc_objects", Unit: "count"}, {Type: "alloc_space", Unit: "bytes"}, {Type: "inuse_objects", Unit: "count"}, {Type: "inuse_space", Unit: "bytes"}}
	curHeap := goProfile(t, heapTypes, func(i int) []int64 { return [][]int64{{10, 1000, 4, 400}, {5, 500, 1, 100}, {1, 64, 1, 64}}[i] })
	prevHeap := goProfile(t, heapTypes, func(i int) []int64 { return [][]int64{{7, 700, 9, 900}, {5, 500, 9, 900}, {0, 0, 0, 0}}[i] })
	var mp bytes.Buffer
	mw := multipart.NewWriter(&mp)
	part, _ := mw.CreateFormFile("profile", "profile.pprof")
	_, _ = part.Write(curHeap)
	part, _ = mw.CreateFormFile("prev_profile", "profile.pprof")
	_, _ = part.Write(prevHeap)
	part, _ = mw.CreateFormField("sample_type_config")
	_, _ = part.Write([]byte(`{"alloc_objects":{"units":"objects","cumulative":true},"alloc_space":{"units":"bytes","cumulative":true},"inuse_objects":{"units":"objects"},"inuse_space":{"units":"bytes"}}`))
	_ = mw.Close()
	rec = ingest(h, "name=api.alloc_space{cluster=heapc}&format=pprof&from=1790596800&until=1790596810", mp.Bytes(), mw.FormDataContentType())
	if rec.Code != http.StatusOK {
		t.Fatalf("multipart: %d %s", rec.Code, rec.Body)
	}
	byType := map[string]map[string]int64{}
	for _, p := range w.profiles["heapc"] {
		byType[p.GetType()] = stacks(p)
	}
	types := make([]string, 0, len(byType))
	for k := range byType {
		types = append(types, k)
	}
	sort.Strings(types)
	if fmt.Sprint(types) != "[alloc_objects alloc_space inuse_objects inuse_space]" {
		t.Fatalf("types = %v", types)
	}
	marshal := "main.main;main.handler;main.encode;encoding/json.Marshal"
	query := "main.main;main.handler;database/sql.(*DB).Query"
	// cumulative: cur − prev (query's alloc did not change → gone)
	if byType["alloc_space"][marshal] != 300 || byType["alloc_space"][query] != 0 || byType["alloc_objects"]["main.main;0xdead"] != 1 {
		t.Fatalf("alloc deltas = %v / %v", byType["alloc_space"], byType["alloc_objects"])
	}
	// point-in-time: the current value, untouched by prev
	if byType["inuse_space"][marshal] != 400 || byType["inuse_space"][query] != 100 {
		t.Fatalf("inuse = %v", byType["inuse_space"])
	}

	mutex := goProfile(t, []*profile.ValueType{{Type: "contentions", Unit: "count"}, {Type: "delay", Unit: "nanoseconds"}},
		func(i int) []int64 { return [][]int64{{2, 5000}, {1, 100}, {0, 0}}[i] })
	if rec := ingest(h, "name=api.block_duration{cluster=m}&format=pprof", mutex, ""); rec.Code != http.StatusOK {
		t.Fatalf("block: %d %s", rec.Code, rec.Body)
	}
	got2 := map[string]string{}
	for _, p := range w.profiles["m"] {
		got2[p.GetType()] = p.GetUnit()
	}
	if fmt.Sprint(got2) != "map[block:nanoseconds block_count:count]" {
		t.Fatalf("block types = %v", got2)
	}
}

func TestIngestAuth(t *testing.T) {
	h := newHandler(&fakeWriter{})
	req := httptest.NewRequest(http.MethodPost, "/ingest?name=a", strings.NewReader("a 1"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no auth: %d", rec.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "/ingest?name=a", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: %d", rec.Code)
	}
}
