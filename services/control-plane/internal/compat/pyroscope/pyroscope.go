// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Package pyroscope accepts profiles on Pyroscope's ingest API
// (POST /ingest?name=app.cpu{k=v}&from=&until=&format=&sampleRate=),
// so the Pyroscope SDKs and agents can profile straight into KubeHero.
//
// Formats: folded/collapsed ("a;b;c 12" per line), lines (one stack
// per line, count 1) and pprof — a raw (optionally gzipped) body or the
// multipart form the SDKs send (profile, prev_profile for cumulative
// sample types, sample_type_config). Every sample type of a pprof
// upload becomes its own KubeHero profile type (cpu, alloc_space,
// inuse_objects, goroutines, mutex, block …). Folded CPU counts are
// converted to nanoseconds at the upload's sample rate so all CPU
// profiles share one unit.
package pyroscope

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/pprof/profile"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/services/control-plane/internal/auth"
	"github.com/kubehero-io/platform/services/control-plane/internal/compat/httpauth"
	"github.com/kubehero-io/platform/services/control-plane/internal/logschema"
	"github.com/kubehero-io/platform/services/control-plane/internal/telemetry"
)

// Limits per upload.
const (
	MaxBodyBytes  = 32 << 20
	MaxStacks     = 100_000 // unique stacks per sample type
	maxFoldedLine = 1 << 20
)

// ProfileWriter is the telemetry entry point uploads land in.
type ProfileWriter interface {
	WriteProfiles(cluster string, profiles []*kuberov1.Profile) (telemetry.ProfileWrite, error)
}

var _ ProfileWriter = (*telemetry.Service)(nil)

// Handler serves POST /ingest.
type Handler struct {
	Writer         ProfileWriter
	Auth           *httpauth.Authenticator
	DefaultCluster string
	Log            *slog.Logger
	Now            func() time.Time
}

type badRequest struct {
	status int
	msg    string
}

func (b *badRequest) Error() string { return b.msg }

func bad(status int, format string, args ...any) error {
	return &badRequest{status: status, msg: fmt.Sprintf(format, args...)}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}
	ctx, aerr := h.Auth.Require(r.Context(), r.Header, auth.RoleMember)
	if aerr != nil {
		httpauth.WriteError(w, aerr)
		return
	}
	profiles, cluster, err := h.parse(r)
	if err != nil {
		status := http.StatusBadRequest
		var br *badRequest
		if errors.As(err, &br) {
			status = br.status
		}
		http.Error(w, err.Error(), status)
		return
	}
	resolved, err := telemetry.ResolveCluster(ctx, cluster)
	if err != nil {
		http.Error(w, err.Error(), connectStatus(err))
		return
	}
	if resolved == "" {
		resolved = h.DefaultCluster
	}
	if _, err := h.Writer.WriteProfiles(resolved, profiles); err != nil {
		status := connectStatus(err)
		if status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable {
			w.Header().Set("Retry-After", "5")
		}
		http.Error(w, err.Error(), status)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func connectStatus(err error) int {
	var ce *connect.Error
	if !errors.As(err, &ce) {
		return http.StatusInternalServerError
	}
	switch ce.Code() {
	case connect.CodeInvalidArgument:
		return http.StatusBadRequest
	case connect.CodePermissionDenied:
		return http.StatusForbidden
	case connect.CodeResourceExhausted:
		return http.StatusTooManyRequests
	case connect.CodeUnavailable:
		return http.StatusServiceUnavailable
	}
	return http.StatusInternalServerError
}

// upload is what the query string says about an upload.
type upload struct {
	app        string
	typ        string // type suffix of name ("" when absent)
	labels     map[string]string
	from       time.Time
	until      time.Time
	sampleRate float64
	units      string
	spy        string
	now        time.Time
}

// window resolves the profile's start and duration: query parameters
// win, then the pprof's own time / duration, then now.
func (u upload) window(pTime, pDur int64) (int64, int64) {
	from := u.from
	if from.IsZero() {
		from = u.now
		if pTime > 0 {
			from = time.Unix(0, pTime)
		}
	}
	dur := int64(0)
	switch {
	case !u.until.IsZero():
		dur = u.until.Sub(from).Nanoseconds()
	case pDur > 0:
		dur = pDur
	}
	if dur < 0 {
		dur = 0
	}
	return from.UnixNano(), dur
}

func (h *Handler) parse(r *http.Request) ([]*kuberov1.Profile, string, error) {
	q := r.URL.Query()
	app, typ, labels, err := ParseName(q.Get("name"))
	if err != nil {
		return nil, "", bad(http.StatusBadRequest, "name: %v", err)
	}
	now := time.Now()
	if h.Now != nil {
		now = h.Now()
	}
	u := upload{app: app, typ: typ, labels: labels, units: q.Get("units"), spy: q.Get("spyName"), sampleRate: 100}
	// from/until default to the pprof's own timestamp, else now.
	if u.from, err = unixParam(q.Get("from"), time.Time{}); err != nil {
		return nil, "", err
	}
	if u.until, err = unixParam(q.Get("until"), time.Time{}); err != nil {
		return nil, "", err
	}
	if !u.from.IsZero() && !u.until.IsZero() && u.until.Before(u.from) {
		return nil, "", bad(http.StatusBadRequest, "until is before from")
	}
	u.now = now
	if v := q.Get("sampleRate"); v != "" {
		if u.sampleRate, err = strconv.ParseFloat(v, 64); err != nil || u.sampleRate <= 0 || u.sampleRate > 1e6 {
			return nil, "", bad(http.StatusBadRequest, "bad sampleRate %q", v)
		}
	}
	body := http.MaxBytesReader(nil, r.Body, MaxBodyBytes)
	var profiles []*kuberov1.Profile
	switch format := strings.ToLower(q.Get("format")); format {
	case "", "folded", "collapsed", "lines":
		data, err := readAll(body)
		if err != nil {
			return nil, "", err
		}
		p, err := u.folded(data, format == "lines")
		if err != nil {
			return nil, "", err
		}
		profiles = []*kuberov1.Profile{p}
	case "pprof":
		cur, prev, cfg, err := readPprof(r, body)
		if err != nil {
			return nil, "", err
		}
		if profiles, err = u.pprof(cur, prev, cfg); err != nil {
			return nil, "", err
		}
	default:
		return nil, "", bad(http.StatusUnsupportedMediaType, "format %q is not supported (folded, lines, pprof)", format)
	}
	cluster := labels["cluster"]
	if cluster == "" {
		cluster = strings.TrimSpace(r.Header.Get("X-Scope-OrgID"))
	}
	return profiles, cluster, nil
}

func readAll(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, bad(http.StatusRequestEntityTooLarge, "body larger than %d bytes", MaxBodyBytes)
		}
		return nil, bad(http.StatusBadRequest, "read body: %v", err)
	}
	return b, nil
}

func unixParam(v string, def time.Time) (time.Time, error) {
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return time.Time{}, bad(http.StatusBadRequest, "bad unix timestamp %q", v)
	}
	switch {
	case n > 1e17: // nanoseconds
		return time.Unix(0, n), nil
	case n > 1e14: // microseconds
		return time.UnixMicro(n), nil
	case n > 1e11: // milliseconds
		return time.UnixMilli(n), nil
	}
	return time.Unix(n, 0), nil
}

// knownTypes are the profile-type suffixes Pyroscope clients append to
// the application name.
var knownTypes = map[string]bool{
	"cpu": true, "itimer": true, "wall": true, "alloc_objects": true, "alloc_space": true,
	"inuse_objects": true, "inuse_space": true, "goroutines": true, "goroutine": true,
	"mutex_count": true, "mutex_duration": true, "block_count": true, "block_duration": true,
	"contentions": true, "delay": true, "lock_count": true, "lock_duration": true, "exceptions": true,
}

// ParseName splits Pyroscope's `app.type{k=v,…}` into the app name, the
// profile-type suffix ("" when the last dotted segment is not a known
// type) and the labels.
func ParseName(name string) (app, typ string, labels map[string]string, err error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", "", nil, errors.New("required")
	}
	base, rest, hasLabels := strings.Cut(name, "{")
	labels = map[string]string{}
	if hasLabels {
		inner, ok := strings.CutSuffix(rest, "}")
		if !ok {
			return "", "", nil, errors.New("labels must end with }")
		}
		for _, kv := range strings.Split(inner, ",") {
			kv = strings.TrimSpace(kv)
			if kv == "" {
				continue
			}
			k, v, ok := strings.Cut(kv, "=")
			if !ok || strings.TrimSpace(k) == "" {
				return "", "", nil, fmt.Errorf("label %q is not key=value", kv)
			}
			labels[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"`)
		}
	}
	app = base
	if i := strings.LastIndexByte(base, '.'); i >= 0 && knownTypes[base[i+1:]] {
		app, typ = base[:i], base[i+1:]
	}
	if app == "" {
		return "", "", nil, errors.New("application name is empty")
	}
	return app, typ, labels, nil
}

// kind maps a name suffix onto a KubeHero profile type and unit.
func kind(typ string) (string, string) {
	switch typ {
	case "", "cpu", "itimer":
		return "cpu", "nanoseconds"
	case "wall":
		return "wall", "nanoseconds"
	case "alloc_space", "inuse_space":
		return typ, "bytes"
	case "alloc_objects", "inuse_objects", "exceptions":
		return typ, "count"
	case "goroutine", "goroutines":
		return "goroutines", "count"
	case "mutex_duration", "delay":
		return "mutex", "nanoseconds"
	case "block_duration":
		return "block", "nanoseconds"
	case "mutex_count", "contentions":
		return "mutex_count", "count"
	case "block_count":
		return "block_count", "count"
	case "lock_duration":
		return "lock", "nanoseconds"
	case "lock_count":
		return "lock_count", "count"
	}
	return typ, "count"
}

func (u upload) base(typ, unit string, pTime, pDur int64) *kuberov1.Profile {
	src := &kuberov1.PodRef{}
	extra := map[string]string{}
	for k, v := range u.labels {
		switch logschema.SanitizeLabelName(k) {
		case "namespace", "k8s_namespace_name", "kubernetes_namespace":
			src.Namespace = v
		case "pod", "pod_name", "k8s_pod_name":
			src.Pod = v
		case "container", "container_name", "k8s_container_name":
			src.Container = v
		case "node", "node_name", "k8s_node_name":
			src.Node = v
		case "workload", "k8s_deployment_name":
			src.Workload = v
		case "cluster", "service_name", "__name__":
			// cluster picks the tenant; service_name duplicates the app
		default:
			extra[k] = v
		}
	}
	if u.spy != "" {
		extra["spy"] = u.spy
	}
	if src.Workload == "" {
		src.Workload = u.app
	}
	ts, dur := u.window(pTime, pDur)
	return &kuberov1.Profile{
		TsUnixNano:   ts,
		DurationNano: dur,
		Type:         typ,
		Unit:         unit,
		Service:      u.app,
		Source:       src,
		Labels:       extra,
		Origin:       "pyroscope",
	}
}

// folded parses `frame;frame;frame count` lines (or, for the lines
// format, one stack per line counted once).
func (u upload) folded(data []byte, lines bool) (*kuberov1.Profile, error) {
	typ, unit := kind(u.typ)
	p := u.base(typ, unit, 0, 0)
	// Folded CPU and wall counts are samples at sampleRate Hz; store
	// nanoseconds like every other CPU profile.
	scale := int64(1)
	if unit == "nanoseconds" && (u.units == "" || u.units == "samples") {
		scale = int64(float64(time.Second) / u.sampleRate)
		p.Period = scale
	}
	idx := map[string]int{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64<<10), maxFoldedLine)
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		stack, count := line, int64(1)
		if !lines {
			i := strings.LastIndexByte(line, ' ')
			if i < 0 {
				return nil, bad(http.StatusBadRequest, "line %d: want \"frame;frame count\"", n)
			}
			c, err := strconv.ParseInt(line[i+1:], 10, 64)
			if err != nil || c < 0 {
				return nil, bad(http.StatusBadRequest, "line %d: bad count %q", n, line[i+1:])
			}
			stack, count = strings.TrimSpace(line[:i]), c
		}
		if count == 0 || stack == "" {
			continue
		}
		if i, ok := idx[stack]; ok {
			p.Samples[i].Value += count * scale
			continue
		}
		if len(idx) >= MaxStacks {
			return nil, bad(http.StatusRequestEntityTooLarge, "more than %d distinct stacks in one upload", MaxStacks)
		}
		idx[stack] = len(p.Samples)
		p.Samples = append(p.Samples, &kuberov1.StackSample{Frames: strings.Split(stack, ";"), Value: count * scale})
	}
	if err := sc.Err(); err != nil {
		return nil, bad(http.StatusBadRequest, "read folded profile: %v", err)
	}
	return p, nil
}

type typeConfig struct {
	Cumulative bool `json:"cumulative"`
}

// readPprof returns the profile (and previous profile and sample type
// config) from a raw body or the SDKs' multipart form.
func readPprof(r *http.Request, body io.Reader) (cur, prev []byte, cfg map[string]typeConfig, err error) {
	mt, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mt != "multipart/form-data" {
		cur, err = readAll(body)
		return cur, nil, nil, err
	}
	mr := multipart.NewReader(body, params["boundary"])
	for {
		part, perr := mr.NextPart()
		if perr == io.EOF {
			break
		}
		if perr != nil {
			return nil, nil, nil, bad(http.StatusBadRequest, "multipart: %v", perr)
		}
		data, rerr := readAll(part)
		if rerr != nil {
			return nil, nil, nil, rerr
		}
		switch part.FormName() {
		case "profile":
			cur = data
		case "prev_profile":
			prev = data
		case "sample_type_config":
			if err := json.Unmarshal(data, &cfg); err != nil {
				return nil, nil, nil, bad(http.StatusBadRequest, "sample_type_config: %v", err)
			}
		}
	}
	if cur == nil {
		return nil, nil, nil, bad(http.StatusBadRequest, "multipart upload without a profile part")
	}
	return cur, prev, cfg, nil
}

// frames renders a sample's stack root → leaf, expanding inlined
// functions (a Location's Line[0] is the innermost call).
func frames(s *profile.Sample) []string {
	var out []string
	for i := len(s.Location) - 1; i >= 0; i-- {
		loc := s.Location[i]
		if len(loc.Line) == 0 {
			out = append(out, fmt.Sprintf("0x%x", loc.Address))
			continue
		}
		for j := len(loc.Line) - 1; j >= 0; j-- {
			name := "[unknown]"
			if fn := loc.Line[j].Function; fn != nil && fn.Name != "" {
				name = fn.Name
			}
			out = append(out, name)
		}
	}
	return out
}

// sampleKind maps a pprof sample type onto a KubeHero type and unit.
// ok=false skips it (cpu "samples" duplicate "cpu" nanoseconds).
func sampleKind(st *profile.ValueType, family string) (string, string, bool) {
	switch {
	case st.Type == "cpu" && st.Unit == "nanoseconds":
		return "cpu", "nanoseconds", true
	case st.Type == "samples" && st.Unit == "count":
		return "", "", false // counted again in nanoseconds (or converted below)
	case st.Type == "wall":
		return "wall", "nanoseconds", true
	case st.Type == "goroutine" || st.Type == "goroutines":
		return "goroutines", "count", true
	case st.Type == "contentions":
		return family + "_count", "count", true
	case st.Type == "delay":
		return family, "nanoseconds", true
	}
	t := logschema.SanitizeLabelName(strings.ToLower(st.Type))
	unit := strings.ToLower(st.Unit)
	if unit == "objects" {
		unit = "count"
	}
	return t, unit, t != ""
}

func (u upload) pprof(curData, prevData []byte, cfg map[string]typeConfig) ([]*kuberov1.Profile, error) {
	cur, err := profile.ParseData(curData)
	if err != nil {
		return nil, bad(http.StatusBadRequest, "pprof: %v", err)
	}
	var prev *profile.Profile
	if prevData != nil {
		if prev, err = profile.ParseData(prevData); err != nil {
			return nil, bad(http.StatusBadRequest, "prev_profile: %v", err)
		}
	}
	family := "mutex"
	if strings.Contains(u.typ, "block") {
		family = "block"
	}
	hasCPUNanos := false
	for _, st := range cur.SampleType {
		if st.Type == "cpu" && st.Unit == "nanoseconds" {
			hasCPUNanos = true
		}
	}
	var out []*kuberov1.Profile
	for i, st := range cur.SampleType {
		typ, unit, ok := sampleKind(st, family)
		scale := int64(1)
		if !ok && st.Type == "samples" && !hasCPUNanos && cur.Period > 0 && cur.PeriodType != nil && cur.PeriodType.Unit == "nanoseconds" {
			// Only sample counts: turn them into CPU time with the period.
			typ, unit, ok, scale = "cpu", "nanoseconds", true, cur.Period
		}
		if !ok {
			continue
		}
		values := map[string]int64{}
		var order []string
		stacks := map[string][]string{}
		add := func(p *profile.Profile, idx int, sign int64) error {
			for _, s := range p.Sample {
				if idx >= len(s.Value) || s.Value[idx] == 0 {
					continue
				}
				fr := frames(s)
				key := strings.Join(fr, "\x00")
				if _, seen := values[key]; !seen {
					if len(values) >= MaxStacks {
						return bad(http.StatusRequestEntityTooLarge, "more than %d distinct stacks in one upload", MaxStacks)
					}
					order = append(order, key)
					stacks[key] = fr
				}
				values[key] += sign * s.Value[idx] * scale
			}
			return nil
		}
		if err := add(cur, i, 1); err != nil {
			return nil, err
		}
		if prev != nil && (cfg[st.Type].Cumulative || cfg == nil && strings.HasPrefix(st.Type, "alloc_")) {
			// Cumulative types (heap allocations) arrive as running
			// totals; the upload's own share is cur − prev.
			for j, pst := range prev.SampleType {
				if pst.Type == st.Type && pst.Unit == st.Unit {
					if err := add(prev, j, -1); err != nil {
						return nil, err
					}
				}
			}
		}
		p := u.base(typ, unit, cur.TimeNanos, cur.DurationNanos)
		if typ == "cpu" {
			p.Period = cur.Period
		}
		for _, key := range order {
			if v := values[key]; v > 0 {
				p.Samples = append(p.Samples, &kuberov1.StackSample{Frames: stacks[key], Value: v})
			}
		}
		if len(p.Samples) > 0 {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil, bad(http.StatusBadRequest, "pprof upload has no samples")
	}
	return out, nil
}
