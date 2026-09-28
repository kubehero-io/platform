// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package events

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/services/collector/internal/kube"
	"github.com/kubehero-io/platform/services/collector/internal/metrics"
)

// ClusterConfig tunes the leader-only sources.
type ClusterConfig struct {
	// Interval between pending-pod and node-condition scans. 30s.
	Interval time.Duration
	// Lookback: Warning events last seen before (list time − Lookback)
	// are history — baselined, not reported. 5m.
	Lookback time.Duration
	// PendingReemit re-reports a still-unschedulable pod. 5m.
	PendingReemit time.Duration
	// WarningThrottle bounds how often one aggregated Warning event
	// (kubelet bumps .count on repeats) is re-reported. 5m.
	WarningThrottle time.Duration
	Logger          *slog.Logger
}

func (c *ClusterConfig) defaults() {
	if c.Interval <= 0 {
		c.Interval = 30 * time.Second
	}
	if c.Lookback <= 0 {
		c.Lookback = 5 * time.Minute
	}
	if c.PendingReemit <= 0 {
		c.PendingReemit = 5 * time.Minute
	}
	if c.WarningThrottle <= 0 {
		c.WarningThrottle = 5 * time.Minute
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

const (
	maxWarningStates = 20_000
	maxPodLookups    = 10_000
	// maxPendingPerScan bounds one scan's output: a batch system with
	// tens of thousands of queued pods would otherwise emit them all
	// every re-emit window. The oldest pending pods are reported first.
	maxPendingPerScan = 5_000
)

// ClusterWatcher runs the cluster-scoped event sources. Run it only on
// the elected leader: each source is O(cluster), so running it on every
// node would multiply both API load and reported events by N.
type ClusterWatcher struct {
	cfg    ClusterConfig
	client kubernetes.Interface
	owners Owners
	add    func(source string, evs ...*kuberov1.ClusterEvent)
	now    func() time.Time

	pending   map[types.UID]time.Time // last report per unschedulable pod
	warnings  map[types.UID]*warnState
	nodeConds map[string]bool         // node/type/transition already reported
	pods      map[types.UID]podLookup // Warning-event pod → ref
	nodes     cache.Store
}

type warnState struct {
	count    int32
	pending  int32
	lastEmit time.Time
	lastSeen time.Time
	ev       *kuberov1.ClusterEvent // template for aggregated re-reports
	skip     bool                   // suppressed kind: track counts only
}

type podLookup struct {
	ref     *kuberov1.PodRef
	expires time.Time
}

// NewClusterWatcher builds a watcher; add receives events (Batcher.Add).
func NewClusterWatcher(cfg ClusterConfig, client kubernetes.Interface, owners Owners, add func(string, ...*kuberov1.ClusterEvent)) *ClusterWatcher {
	cfg.defaults()
	return &ClusterWatcher{
		cfg: cfg, client: client, owners: owners, add: add, now: time.Now,
		pending:   map[types.UID]time.Time{},
		warnings:  map[types.UID]*warnState{},
		nodeConds: map[string]bool{},
		pods:      map[types.UID]podLookup{},
	}
}

// Run blocks until ctx ends (leadership lost or shutdown).
func (w *ClusterWatcher) Run(ctx context.Context) {
	factory := informers.NewSharedInformerFactory(w.client, 0)
	nodeInf := factory.Core().V1().Nodes().Informer()
	_ = nodeInf.SetTransform(nodeConditionsOnly)
	w.nodes = nodeInf.GetStore()
	factory.Start(ctx.Done())

	warnDone := make(chan struct{})
	go func() {
		defer close(warnDone)
		w.watchWarnings(ctx)
	}()

	synced := cache.WaitForCacheSync(ctx.Done(), nodeInf.HasSynced)
	t := time.NewTicker(w.cfg.Interval)
	defer t.Stop()
	for {
		if synced {
			w.ScanNodes()
		}
		w.ScanPending(ctx)
		select {
		case <-ctx.Done():
			<-warnDone
			factory.Shutdown()
			return
		case <-t.C:
		}
	}
}

// ── pending pods ──────────────────────────────────────────────────────

// ScanPending reports unschedulable pods.
func (w *ClusterWatcher) ScanPending(ctx context.Context) {
	// resourceVersion=0 is served from the API server's watch cache.
	list, err := w.client.CoreV1().Pods("").List(ctx, metav1.ListOptions{FieldSelector: "status.phase=Pending", ResourceVersion: "0"})
	if err != nil {
		if ctx.Err() == nil {
			metrics.ScanErrors.With("pending", "list").Inc()
			w.cfg.Logger.Warn("listing pending pods failed", "err", err)
		}
		return
	}
	now := w.now()
	still := map[types.UID]bool{}
	var out []*kuberov1.ClusterEvent
	items := list.Items
	sort.Slice(items, func(i, j int) bool { return items[i].CreationTimestamp.Before(&items[j].CreationTimestamp) })
	capped := 0
	for i := range items {
		p := &items[i]
		if p.Status.Phase != corev1.PodPending || p.Spec.NodeName != "" {
			continue
		}
		cond := unschedulable(p)
		if cond == nil {
			continue
		}
		still[p.UID] = true
		if last, ok := w.pending[p.UID]; ok && now.Sub(last) < w.cfg.PendingReemit {
			continue
		}
		if len(out) >= maxPendingPerScan {
			capped++
			continue
		}
		w.pending[p.UID] = now
		out = append(out, w.pendingEvent(ctx, now, p, cond))
	}
	if capped > 0 {
		w.cfg.Logger.Warn("more unschedulable pods than one scan reports — the rest follow in later scans", "reported", len(out), "deferred", capped)
	}
	for uid := range w.pending {
		if !still[uid] {
			delete(w.pending, uid)
		}
	}
	w.add("pending", out...)
}

func unschedulable(p *corev1.Pod) *corev1.PodCondition {
	for i := range p.Status.Conditions {
		c := &p.Status.Conditions[i]
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Reason == corev1.PodReasonUnschedulable {
			return c
		}
	}
	return nil
}

func (w *ClusterWatcher) pendingEvent(ctx context.Context, now time.Time, p *corev1.Pod, cond *corev1.PodCondition) *kuberov1.ClusterEvent {
	wl := kube.Workload{Name: p.Name, Kind: "Pod"}
	if w.owners != nil {
		wl = w.owners.Resolve(ctx, p)
	}
	r := kube.PodRequests(p)
	age := now.Sub(p.CreationTimestamp.Time)
	if p.CreationTimestamp.IsZero() || age < 0 {
		age = 0
	}
	attrs := map[string]string{
		"cpu_millicores": strconv.FormatInt(r.CPUMillis, 10),
		"mem_bytes":      strconv.FormatInt(r.MemBytes, 10),
		"gpu":            strconv.FormatInt(r.GPUs, 10),
		"workload":       wl.Name,
		"workload_kind":  wl.Kind,
		"age_sec":        strconv.FormatInt(int64(age.Seconds()), 10),
		"message":        truncate(cond.Message, maxMessage),
	}
	if np := nodepoolSelector(p); np != "" {
		attrs["nodepool"] = np
	}
	if p.Spec.PriorityClassName != "" {
		attrs["priority_class"] = p.Spec.PriorityClassName
	}
	return &kuberov1.ClusterEvent{
		TsUnixMs:   now.UnixMilli(),
		Kind:       KindUnschedulable,
		Severity:   SeverityWarn,
		Source:     kube.PodRefFor(p, nil, wl),
		Reason:     cond.Reason,
		Message:    truncate(cond.Message, maxMessage),
		Attributes: attrs,
		Count:      1,
	}
}

// nodepoolSelector returns the nodepool a pod pins itself to, if any.
func nodepoolSelector(p *corev1.Pod) string {
	for _, k := range []string{"kubehero.io/nodepool", "eks.amazonaws.com/nodegroup", "cloud.google.com/gke-nodepool",
		"kubernetes.azure.com/agentpool", "agentpool", "karpenter.sh/nodepool"} {
		if v := p.Spec.NodeSelector[k]; v != "" {
			return v
		}
	}
	return ""
}

// ── node conditions ───────────────────────────────────────────────────

// ScanNodes reports NotReady nodes and resource pressure, once per
// condition episode (keyed by its lastTransitionTime).
func (w *ClusterWatcher) ScanNodes() {
	if w.nodes == nil {
		return
	}
	now := w.now()
	live := map[string]bool{}
	var out []*kuberov1.ClusterEvent
	for _, obj := range w.nodes.List() {
		n, ok := obj.(*corev1.Node)
		if !ok {
			continue
		}
		for _, c := range n.Status.Conditions {
			var kind, severity string
			switch {
			case c.Type == corev1.NodeReady && c.Status != corev1.ConditionTrue:
				kind, severity = KindNodeNotReady, SeverityCritical
			case (c.Type == corev1.NodeMemoryPressure || c.Type == corev1.NodeDiskPressure || c.Type == corev1.NodePIDPressure) &&
				c.Status == corev1.ConditionTrue:
				kind, severity = KindNodePressure, SeverityWarn
			default:
				continue
			}
			key := n.Name + "/" + string(c.Type) + "/" + strconv.FormatInt(c.LastTransitionTime.Unix(), 10)
			live[key] = true
			if w.nodeConds[key] {
				continue
			}
			w.nodeConds[key] = true
			reason := c.Reason
			if reason == "" {
				reason = string(c.Type)
			}
			out = append(out, &kuberov1.ClusterEvent{
				TsUnixMs: now.UnixMilli(),
				Kind:     kind,
				Severity: severity,
				Source:   &kuberov1.PodRef{Node: n.Name, Zone: kube.Zone(n)},
				Reason:   reason,
				Message:  truncate(c.Message, maxMessage),
				Attributes: map[string]string{
					"condition":       string(c.Type),
					"status":          string(c.Status),
					"transition_time": c.LastTransitionTime.UTC().Format(time.RFC3339),
				},
				Count: 1,
			})
		}
	}
	for k := range w.nodeConds {
		if !live[k] {
			delete(w.nodeConds, k)
		}
	}
	w.add("node", out...)
}

func nodeConditionsOnly(obj any) (any, error) {
	n, ok := obj.(*corev1.Node)
	if !ok {
		return obj, nil
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: n.Name, UID: n.UID, ResourceVersion: n.ResourceVersion,
			Labels: map[string]string{"topology.kubernetes.io/zone": kube.Zone(n)},
		},
		Status: corev1.NodeStatus{Conditions: n.Status.Conditions},
	}, nil
}

// ── Warning events ────────────────────────────────────────────────────

var errExpired = errors.New("watch resource version expired")

// watchWarnings owns all Warning-event state (warnings, pods): it is only
// touched from this goroutine, including the throttled re-reports.
func (w *ClusterWatcher) watchWarnings(ctx context.Context) {
	flush := time.NewTicker(w.cfg.Interval)
	defer flush.Stop()
	cutoff := w.now().Add(-w.cfg.Lookback)
	rv := ""
	backoff := time.Second
	for ctx.Err() == nil {
		var err error
		if rv == "" {
			rv, err = w.listWarnings(ctx, cutoff)
		}
		if err == nil {
			rv, err = w.watchFrom(ctx, rv, flush.C)
		}
		if ctx.Err() != nil {
			return
		}
		switch {
		case err == nil:
			backoff = time.Second // server closed the watch; resume from rv
			continue
		case errors.Is(err, errExpired):
			// Too far behind: relist, reporting only what's recent.
			rv, cutoff = "", w.now().Add(-w.cfg.Lookback)
			continue
		}
		metrics.ScanErrors.With("warning_events", "watch").Inc()
		w.cfg.Logger.Warn("watching Warning events failed — retrying", "err", err, "retry_in", backoff.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
		rv = ""
	}
}

func (w *ClusterWatcher) listWarnings(ctx context.Context, cutoff time.Time) (string, error) {
	opts := metav1.ListOptions{FieldSelector: "type=Warning", Limit: 500}
	var rv string
	for {
		list, err := w.client.CoreV1().Events("").List(ctx, opts)
		if err != nil {
			return "", err
		}
		for i := range list.Items {
			w.observe(ctx, &list.Items[i], cutoff)
		}
		rv = list.ResourceVersion
		if list.Continue == "" {
			return rv, nil
		}
		opts.Continue = list.Continue
	}
}

func (w *ClusterWatcher) watchFrom(ctx context.Context, rv string, flush <-chan time.Time) (string, error) {
	timeout := int64(300)
	wi, err := w.client.CoreV1().Events("").Watch(ctx, metav1.ListOptions{
		FieldSelector: "type=Warning", ResourceVersion: rv, AllowWatchBookmarks: true, TimeoutSeconds: &timeout,
	})
	if err != nil {
		if apierrors.IsResourceExpired(err) || apierrors.IsGone(err) {
			return rv, errExpired
		}
		return rv, err
	}
	defer wi.Stop()
	for {
		select {
		case <-ctx.Done():
			return rv, nil
		case <-flush:
			w.flushWarnings()
		case e, ok := <-wi.ResultChan():
			if !ok {
				return rv, nil
			}
			switch e.Type {
			case watch.Added, watch.Modified:
				if ev, ok := e.Object.(*corev1.Event); ok {
					rv = ev.ResourceVersion
					// Anything arriving on the watch is new: no cutoff.
					w.observe(ctx, ev, time.Time{})
				}
			case watch.Bookmark:
				if m, err := metaAccessor(e.Object); err == nil {
					rv = m.GetResourceVersion()
				}
			case watch.Error:
				st := apierrors.FromObject(e.Object)
				if apierrors.IsResourceExpired(st) || apierrors.IsGone(st) {
					return rv, errExpired
				}
				return rv, st
			}
		}
	}
}

func metaAccessor(obj any) (metav1.Object, error) {
	m, ok := obj.(metav1.Object)
	if !ok {
		return nil, errors.New("object has no metadata")
	}
	return m, nil
}

// observe processes one Warning event: first sight reports it (unless
// it predates cutoff), count increments aggregate and are re-reported at
// most every WarningThrottle.
func (w *ClusterWatcher) observe(ctx context.Context, ev *corev1.Event, cutoff time.Time) {
	if ev.Type != corev1.EventTypeWarning {
		return
	}
	now := w.now()
	lastSeen := eventTime(ev)
	count := ev.Count
	if ev.Series != nil && ev.Series.Count > count {
		count = ev.Series.Count
	}
	if count < 1 {
		count = 1
	}
	st := w.warnings[ev.UID]
	if st == nil {
		w.evictWarnings()
		st = &warnState{count: count, lastSeen: lastSeen}
		w.warnings[ev.UID] = st
		if !cutoff.IsZero() && lastSeen.Before(cutoff) {
			st.lastEmit = lastSeen // history: baseline only
			return
		}
		kind, severity := Classify(ev.Reason, ev.Message)
		if SuppressedByOtherSource(kind, ev.InvolvedObject.Kind) {
			st.skip = true
			return
		}
		st.ev = w.warningEvent(ctx, ev, kind, severity)
		n := count
		if first := firstTime(ev); !cutoff.IsZero() && first.Before(cutoff) {
			// Aggregated over a long time: only the recent repeat is news.
			n = 1
		}
		st.lastEmit = now
		w.emitWarning(st, lastSeen, n)
		return
	}
	st.lastSeen = lastSeen
	if count > st.count {
		st.pending += count - st.count
		st.count = count
	}
	if st.skip || st.ev == nil || st.pending == 0 || now.Sub(st.lastEmit) < w.cfg.WarningThrottle {
		return
	}
	st.lastEmit = now
	n := st.pending
	st.pending = 0
	w.emitWarning(st, lastSeen, n)
}

func (w *ClusterWatcher) emitWarning(st *warnState, at time.Time, count int32) {
	ev := cloneEvent(st.ev)
	ev.TsUnixMs = at.UnixMilli()
	ev.Count = count
	w.add("k8s", ev)
}

// flushWarnings re-reports aggregated repeats that are past the throttle.
func (w *ClusterWatcher) flushWarnings() {
	now := w.now()
	for _, st := range w.warnings {
		if st.skip || st.ev == nil || st.pending == 0 || now.Sub(st.lastEmit) < w.cfg.WarningThrottle {
			continue
		}
		st.lastEmit = now
		n := st.pending
		st.pending = 0
		w.emitWarning(st, st.lastSeen, n)
	}
}

func (w *ClusterWatcher) evictWarnings() {
	if len(w.warnings) < maxWarningStates {
		return
	}
	type kv struct {
		uid types.UID
		t   time.Time
	}
	all := make([]kv, 0, len(w.warnings))
	for uid, st := range w.warnings {
		all = append(all, kv{uid, st.lastSeen})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].t.Before(all[j].t) })
	for _, v := range all[:len(all)/4] {
		delete(w.warnings, v.uid)
	}
}

func (w *ClusterWatcher) warningEvent(ctx context.Context, ev *corev1.Event, kind, severity string) *kuberov1.ClusterEvent {
	obj := ev.InvolvedObject
	attrs := map[string]string{"object_kind": obj.Kind, "object_name": obj.Name}
	if c := ev.Source.Component; c != "" {
		attrs["reporting_component"] = c
	} else if ev.ReportingController != "" {
		attrs["reporting_component"] = ev.ReportingController
	}
	var src *kuberov1.PodRef
	switch obj.Kind {
	case "Pod":
		src = w.podRef(ctx, obj)
	case "Node":
		src = &kuberov1.PodRef{Node: obj.Name}
		if ev.Source.Host != "" && src.Node == "" {
			src.Node = ev.Source.Host
		}
	case "Deployment", "StatefulSet", "DaemonSet", "ReplicaSet", "Job", "CronJob":
		src = &kuberov1.PodRef{Namespace: obj.Namespace, Workload: obj.Name, WorkloadKind: obj.Kind}
	default:
		src = &kuberov1.PodRef{Namespace: obj.Namespace}
	}
	return &kuberov1.ClusterEvent{
		Kind: kind, Severity: severity, Source: src, Reason: ev.Reason,
		Message: truncate(ev.Message, maxMessage), Attributes: attrs,
	}
}

// podRef resolves an event's pod to a full ref (workload, team) with
// one GET per pod UID, cached; deleted pods fall back to name only.
func (w *ClusterWatcher) podRef(ctx context.Context, obj corev1.ObjectReference) *kuberov1.PodRef {
	now := w.now()
	if l, ok := w.pods[obj.UID]; ok && now.Before(l.expires) {
		return clonePodRef(l.ref)
	}
	if len(w.pods) >= maxPodLookups {
		w.pods = map[types.UID]podLookup{}
	}
	ref := &kuberov1.PodRef{Namespace: obj.Namespace, Pod: obj.Name, PodUid: string(obj.UID)}
	gctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	p, err := w.client.CoreV1().Pods(obj.Namespace).Get(gctx, obj.Name, metav1.GetOptions{})
	cancel()
	if err == nil && (obj.UID == "" || p.UID == obj.UID) {
		wl := kube.Workload{Name: p.Name, Kind: "Pod"}
		if w.owners != nil {
			wl = w.owners.Resolve(ctx, p)
		}
		ref = kube.PodRefFor(p, nil, wl)
	}
	if fp := obj.FieldPath; fp != "" {
		// e.g. spec.containers{app}
		if i, j := strings.IndexByte(fp, '{'), strings.IndexByte(fp, '}'); i >= 0 && j > i {
			ref.Container = fp[i+1 : j]
		}
	}
	w.pods[obj.UID] = podLookup{ref: ref, expires: now.Add(10 * time.Minute)}
	return clonePodRef(ref)
}

func eventTime(ev *corev1.Event) time.Time {
	switch {
	case ev.Series != nil && !ev.Series.LastObservedTime.IsZero():
		return ev.Series.LastObservedTime.Time
	case !ev.LastTimestamp.IsZero():
		return ev.LastTimestamp.Time
	case !ev.EventTime.IsZero():
		return ev.EventTime.Time
	case !ev.FirstTimestamp.IsZero():
		return ev.FirstTimestamp.Time
	}
	return ev.CreationTimestamp.Time
}

func firstTime(ev *corev1.Event) time.Time {
	switch {
	case !ev.FirstTimestamp.IsZero():
		return ev.FirstTimestamp.Time
	case !ev.EventTime.IsZero():
		return ev.EventTime.Time
	}
	return ev.CreationTimestamp.Time
}
