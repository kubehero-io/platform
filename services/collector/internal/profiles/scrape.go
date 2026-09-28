// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

// Package profiles scrapes Go-style pprof endpoints of annotated pods on
// this node — Pyroscope / Grafana Alloy pull mode, same annotations —
// and ships the stacks through IngestProfiles (origin "pprof-scrape").
//
// Opt-in annotations (either convention):
//
//	profiles.grafana.com/<type>.scrape: "true"   type: cpu | memory | goroutine | block | mutex
//	profiles.grafana.com/<type>.port: "6060"     or .port_name: "http-metrics"
//	profiles.grafana.com/<type>.path: "/debug/pprof/profile"   (optional, full endpoint path)
//
//	kubehero.io/profile: "true"                  cpu + heap + goroutines
//	kubehero.io/profile-port: "6060"
//	kubehero.io/profile-path: "/debug/pprof"     (prefix, default /debug/pprof)
//
// Every --profile-interval the scraper fetches CPU (profile?seconds=N),
// heap (emitted as inuse_space and alloc_space) and goroutine profiles,
// merges identical stacks, keeps the heaviest stacks up to a cap (the
// remainder folded into one "[truncated]" stack so totals stay exact),
// and names the profile's service after kubehero.io/service or the
// workload.
//
// Safety: requests go only to the pod's own IP on a validated port with
// a validated path, plain HTTP, no proxy, bounded time and body size. A
// node-local scraper never reaches pods on other nodes.
package profiles

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/pprof/profile"
	corev1 "k8s.io/api/core/v1"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/services/collector/internal/kube"
	"github.com/kubehero-io/platform/services/collector/internal/metrics"
)

// Config tunes the scraper.
type Config struct {
	ClusterID  string
	NodeName   string        // "" = every node (dev)
	Interval   time.Duration // 60s
	CPUSeconds int           // 15
	// MaxBodyBytes caps one profile download (16 MiB).
	MaxBodyBytes int64
	// Concurrency bounds simultaneous scrapes (16; CPU scrapes mostly wait).
	Concurrency int
	// MaxStacks caps unique stacks per profile (10k).
	MaxStacks int
	Logger    *slog.Logger
}

func (c *Config) defaults() {
	if c.Interval <= 0 {
		c.Interval = 60 * time.Second
	}
	if c.CPUSeconds <= 0 {
		c.CPUSeconds = 15
	}
	if c.MaxBodyBytes <= 0 {
		c.MaxBodyBytes = 16 << 20
	}
	if c.Concurrency <= 0 {
		c.Concurrency = 16
	}
	if c.MaxStacks <= 0 {
		c.MaxStacks = 10_000
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

// Inventory lists the node's pods.
type Inventory interface {
	Pods() []*corev1.Pod
	Node(name string) *corev1.Node
}

// Owners resolves pods to workloads.
type Owners interface {
	Resolve(ctx context.Context, p *corev1.Pod) kube.Workload
}

// Per-request limits the control plane enforces (cp-ingest).
const (
	maxProfilesPerRequest = 2000
	maxSamplesPerRequest  = 20_000
	maxFrames             = 512
	maxFrameLen           = 1024
)

// Scraper runs scrape rounds.
type Scraper struct {
	cfg    Config
	inv    Inventory
	owners Owners
	emit   func(*kuberov1.IngestProfilesRequest)
	http   *http.Client
	log    *slog.Logger
}

// New builds a Scraper.
func New(cfg Config, inv Inventory, owners Owners, emit func(*kuberov1.IngestProfilesRequest)) *Scraper {
	cfg.defaults()
	return &Scraper{
		cfg: cfg, inv: inv, owners: owners, emit: emit, log: cfg.Logger.With("component", "profiles"),
		http: &http.Client{Transport: &http.Transport{
			Proxy:                 nil, // pod IPs must never go through an egress proxy
			DialContext:           (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
			MaxIdleConnsPerHost:   1,
			IdleConnTimeout:       90 * time.Second,
			ResponseHeaderTimeout: time.Duration(cfg.CPUSeconds+15) * time.Second,
			DisableCompression:    true, // pprof bodies are already gzipped
		}},
	}
}

// Run scrapes every interval until ctx ends. A round that overruns the
// interval delays the next one instead of stacking up.
func (s *Scraper) Run(ctx context.Context) {
	t := time.NewTicker(s.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.Round(ctx)
	}
}

// Round scrapes every target once and emits the result.
func (s *Scraper) Round(ctx context.Context) {
	targets := s.Targets()
	if len(targets) == 0 {
		return
	}
	var (
		mu  sync.Mutex
		out []*kuberov1.Profile
		wg  sync.WaitGroup
	)
	sem := make(chan struct{}, s.cfg.Concurrency)
	for _, tg := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			ps, err := s.scrape(ctx, tg)
			if err != nil {
				if ctx.Err() == nil {
					s.log.Debug("pprof scrape failed", "pod", tg.pod.Namespace+"/"+tg.pod.Name, "type", tg.kind, "url", tg.url, "err", err)
				}
				return
			}
			mu.Lock()
			out = append(out, ps...)
			mu.Unlock()
		}()
	}
	wg.Wait()
	if s.emit == nil || len(out) == 0 {
		return
	}
	for _, req := range Split(s.cfg.ClusterID, out) {
		s.emit(req)
	}
}

// ── targets ───────────────────────────────────────────────────────────

type target struct {
	pod       *corev1.Pod
	kind      string // cpu | memory | goroutine | block | mutex
	url       string
	container string
}

const grafanaPrefix = "profiles.grafana.com/"

// defaultPaths are the net/http/pprof endpoints per profile kind.
var defaultPaths = map[string]string{
	"cpu":       "/debug/pprof/profile",
	"memory":    "/debug/pprof/heap",
	"goroutine": "/debug/pprof/goroutine",
	"block":     "/debug/pprof/block",
	"mutex":     "/debug/pprof/mutex",
}

// Targets derives scrape targets from the node's pod annotations.
func (s *Scraper) Targets() []target {
	var out []target
	for _, p := range s.inv.Pods() {
		if p.Status.Phase != corev1.PodRunning || p.Status.PodIP == "" {
			continue
		}
		if s.cfg.NodeName != "" && p.Spec.NodeName != s.cfg.NodeName {
			continue
		}
		host := p.Status.PodIP
		seen := map[string]bool{}
		// Grafana / Pyroscope convention, per profile kind.
		for _, kind := range []string{"cpu", "memory", "goroutine", "block", "mutex"} {
			if p.Annotations[grafanaPrefix+kind+".scrape"] != "true" {
				continue
			}
			port, container, ok := resolvePort(p, p.Annotations[grafanaPrefix+kind+".port"], p.Annotations[grafanaPrefix+kind+".port_name"])
			if !ok {
				continue
			}
			path := defaultPaths[kind]
			if v := p.Annotations[grafanaPrefix+kind+".path"]; v != "" {
				if !validPath(v) {
					continue
				}
				path = v
			}
			out = append(out, s.target(p, kind, host, port, path, container))
			seen[kind] = true
		}
		// KubeHero's own opt-in: cpu + heap + goroutines under one prefix.
		if p.Annotations["kubehero.io/profile"] == "true" {
			port, container, ok := resolvePort(p, p.Annotations["kubehero.io/profile-port"], "")
			if !ok {
				continue
			}
			prefix := "/debug/pprof"
			if v := p.Annotations["kubehero.io/profile-path"]; v != "" {
				if !validPath(v) {
					continue
				}
				prefix = strings.TrimRight(v, "/")
			}
			for kind, suffix := range map[string]string{"cpu": "/profile", "memory": "/heap", "goroutine": "/goroutine"} {
				if !seen[kind] {
					out = append(out, s.target(p, kind, host, port, prefix+suffix, container))
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].url < out[j].url })
	return out
}

func (s *Scraper) target(p *corev1.Pod, kind, host string, port int, path, container string) target {
	u := "http://" + net.JoinHostPort(host, strconv.Itoa(port)) + path
	if kind == "cpu" {
		u += "?seconds=" + strconv.Itoa(s.cfg.CPUSeconds)
	}
	return target{pod: p, kind: kind, url: u, container: container}
}

// resolvePort picks the port from a number, a named container port, or
// the pod's only declared port; it also reports which container owns it.
func resolvePort(p *corev1.Pod, number, name string) (int, string, bool) {
	if number != "" {
		n, err := strconv.Atoi(strings.TrimSpace(number))
		if err != nil || n < 1 || n > 65535 {
			return 0, "", false
		}
		for _, c := range p.Spec.Containers {
			for _, cp := range c.Ports {
				if int(cp.ContainerPort) == n {
					return n, c.Name, true
				}
			}
		}
		return n, "", true
	}
	var only []struct {
		port      int
		container string
	}
	for _, c := range p.Spec.Containers {
		for _, cp := range c.Ports {
			if name != "" && cp.Name == name {
				return int(cp.ContainerPort), c.Name, true
			}
			only = append(only, struct {
				port      int
				container string
			}{int(cp.ContainerPort), c.Name})
		}
	}
	if name == "" && len(only) == 1 {
		return only[0].port, only[0].container, true
	}
	return 0, "", false
}

func validPath(p string) bool {
	return len(p) <= 256 && strings.HasPrefix(p, "/") && !strings.Contains(p, "..") &&
		!strings.ContainsAny(p, "?#@\\ \t\r\n") && !strings.Contains(p, "//")
}

// ── scrape + convert ──────────────────────────────────────────────────

var errTooLarge = errors.New("profile exceeds the size limit")

func (s *Scraper) scrape(ctx context.Context, tg target) ([]*kuberov1.Profile, error) {
	timeout := 10 * time.Second
	if tg.kind == "cpu" {
		timeout = time.Duration(s.cfg.CPUSeconds)*time.Second + 10*time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, tg.url, nil)
	if err != nil {
		return nil, s.fail(tg, "error", err)
	}
	req.Header.Set("User-Agent", "kubehero-collector")
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, s.fail(tg, "error", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, s.fail(tg, "error", fmt.Errorf("http %d", resp.StatusCode))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, s.cfg.MaxBodyBytes+1))
	if err != nil {
		return nil, s.fail(tg, "error", err)
	}
	if int64(len(body)) > s.cfg.MaxBodyBytes {
		return nil, s.fail(tg, "too_large", errTooLarge)
	}
	prof, err := profile.ParseData(body)
	if err != nil {
		return nil, s.fail(tg, "error", fmt.Errorf("parse pprof: %w", err))
	}

	w := kube.Workload{Name: tg.pod.Name, Kind: "Pod"}
	if s.owners != nil {
		w = s.owners.Resolve(ctx, tg.pod)
	}
	ref := kube.PodRefFor(tg.pod, s.inv.Node(tg.pod.Spec.NodeName), w)
	ref.Container = tg.container
	service := tg.pod.Annotations[kube.AnnotationService]
	if service == "" {
		service = w.Name
	}
	out := Convert(prof, tg.kind, s.cfg.MaxStacks)
	for _, p := range out {
		if p.TsUnixNano == 0 {
			p.TsUnixNano = start.UnixNano()
		}
		p.Service = service
		p.Source = ref
		p.Origin = "pprof-scrape"
	}
	metrics.ProfileScrapes.With(tg.kind, "ok").Inc()
	return out, nil
}

func (s *Scraper) fail(tg target, outcome string, err error) error {
	metrics.ProfileScrapes.With(tg.kind, outcome).Inc()
	return err
}

// view is one sample type of a pprof profile turned into a KubeHero
// profile type.
type view struct{ sampleType, outType string }

// views maps the scraped kind to the pprof sample types we emit.
var views = map[string][]view{
	"cpu":       {{"cpu", "cpu"}},
	"memory":    {{"inuse_space", "inuse_space"}, {"alloc_space", "alloc_space"}},
	"goroutine": {{"goroutine", "goroutines"}},
	"block":     {{"delay", "block"}},
	"mutex":     {{"delay", "mutex"}},
}

// Convert turns a parsed pprof profile into KubeHero profiles (one per
// emitted sample type): stacks root→leaf, identical stacks merged,
// zero-value stacks dropped, at most maxStacks stacks (the lightest
// folded into "[truncated]").
func Convert(p *profile.Profile, kind string, maxStacks int) []*kuberov1.Profile {
	var out []*kuberov1.Profile
	for _, v := range views[kind] {
		idx := -1
		for i, st := range p.SampleType {
			if st.Type == v.sampleType {
				idx = i
				break
			}
		}
		if idx < 0 && len(p.SampleType) == 1 && (kind == "goroutine") {
			idx = 0 // older runtimes name it "goroutines"
		}
		if idx < 0 {
			continue
		}
		merged := map[string]*kuberov1.StackSample{}
		for _, smp := range p.Sample {
			if idx >= len(smp.Value) || smp.Value[idx] == 0 {
				continue
			}
			frames := stackFrames(smp)
			key := strings.Join(frames, "\x00")
			if m, ok := merged[key]; ok {
				m.Value += smp.Value[idx]
				continue
			}
			merged[key] = &kuberov1.StackSample{Frames: frames, Value: smp.Value[idx]}
		}
		samples := make([]*kuberov1.StackSample, 0, len(merged))
		for _, m := range merged {
			samples = append(samples, m)
		}
		sort.Slice(samples, func(i, j int) bool {
			if samples[i].Value != samples[j].Value {
				return samples[i].Value > samples[j].Value
			}
			return strings.Join(samples[i].Frames, ";") < strings.Join(samples[j].Frames, ";")
		})
		if maxStacks > 0 && len(samples) > maxStacks {
			var rest int64
			for _, sm := range samples[maxStacks-1:] {
				rest += sm.Value
			}
			samples = append(samples[:maxStacks-1], &kuberov1.StackSample{Frames: []string{"[truncated]"}, Value: rest})
		}
		unit := p.SampleType[idx].Unit
		if unit == "" || unit == "goroutine" || unit == "goroutines" {
			unit = "count"
		}
		prof := &kuberov1.Profile{
			TsUnixNano:   p.TimeNanos,
			DurationNano: p.DurationNanos,
			Type:         v.outType,
			Unit:         unit,
			Samples:      samples,
			Period:       p.Period,
		}
		out = append(out, prof)
	}
	return out
}

// stackFrames renders a sample's stack root→leaf. pprof stores locations
// leaf-first, and within a location the inlined callee first.
func stackFrames(smp *profile.Sample) []string {
	frames := make([]string, 0, len(smp.Location)+2)
	for i := len(smp.Location) - 1; i >= 0; i-- {
		loc := smp.Location[i]
		if len(loc.Line) == 0 {
			if loc.Address == 0 {
				frames = append(frames, "[unknown]")
			} else {
				frames = append(frames, "0x"+strconv.FormatUint(loc.Address, 16))
			}
			continue
		}
		for j := len(loc.Line) - 1; j >= 0; j-- {
			name := "[unknown]"
			if f := loc.Line[j].Function; f != nil && f.Name != "" {
				name = f.Name
			}
			if len(name) > maxFrameLen {
				name = name[:maxFrameLen]
			}
			frames = append(frames, name)
		}
	}
	if len(frames) > maxFrames {
		// Keep the leaf side (where time is spent) under a marker root.
		frames = append([]string{"[truncated]"}, frames[len(frames)-maxFrames+1:]...)
	}
	return frames
}

// Split packs profiles into requests within the control plane's limits.
func Split(clusterID string, ps []*kuberov1.Profile) []*kuberov1.IngestProfilesRequest {
	var out []*kuberov1.IngestProfilesRequest
	cur := &kuberov1.IngestProfilesRequest{ClusterId: clusterID}
	samples := 0
	for _, p := range ps {
		n := len(p.Samples)
		if len(cur.Profiles) > 0 && (len(cur.Profiles)+1 > maxProfilesPerRequest || samples+n > maxSamplesPerRequest) {
			out = append(out, cur)
			cur = &kuberov1.IngestProfilesRequest{ClusterId: clusterID}
			samples = 0
		}
		cur.Profiles = append(cur.Profiles, p)
		samples += n
	}
	if len(cur.Profiles) > 0 {
		out = append(out, cur)
	}
	return out
}
