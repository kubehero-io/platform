// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	kubeherov1 "github.com/kubehero-io/platform/services/operator/api/v1"
)

// FieldManager is the field owner stamped on every workload patch, so
// `kubectl get -o yaml --show-managed-fields` shows exactly which
// requests KubeHero set.
const FieldManager = "kubehero-operator"

// Condition types / reasons specific to RightsizingPolicy.
const (
	ConditionDataAvailable = "DataAvailable"

	ReasonInvalidSpec       = "InvalidSpec"
	ReasonOutOfScope        = "ClusterOutOfScope"
	ReasonLiveData          = "LiveRecommendations"
	ReasonControlPlaneUnset = "ControlPlaneUnset"
	ReasonControlPlaneError = "ControlPlaneError"
	ReasonDemoData          = "DemoDataIgnored"
	ReasonNoWorkloads       = "NoWorkloadsInScope"
	ReasonArmNotNeeded      = "ObserveOnlyMode"
)

const (
	defaultRightsizingRequeue = 10 * time.Minute
	defaultObservationWindow  = "7d"
	defaultHeadroomPct        = 15.0
	defaultMaxChangePerDay    = 1
	defaultMinReplicas        = 1
	maxStatusEntries          = 50
	// evaluationTimeout bounds one reconcile end to end (namespace scans,
	// one ListRightsizing per namespace, patches).
	evaluationTimeout = 2 * time.Minute
	// recommendationLimit caps rows requested per namespace.
	recommendationLimit = 1000
)

var windowRE = regexp.MustCompile(`^([1-9][0-9]{0,3})([hd])$`)

// RightsizingPolicyReconciler turns control-plane rightsizing
// recommendations into status (recommend), an audited dry run (shadow),
// or guarded pod-template patches (apply).
//
// Every apply-mode change must pass all of: human arm, recommendation
// confidence, bounded step size (≤50% shrink, ≤100% growth), the
// per-workload daily change budget, memory never below the observed max
// nor shrunk after OOM kills, minReplicas, a settled rollout, no VPA
// managing the container and no HPA scaling on that resource's
// utilization. Each change records its previous values on the workload
// and in the audit log so `kubehero undo <changeId>` can restore them.
type RightsizingPolicyReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// APIReader reads namespaces, workloads and autoscalers straight
	// from the API server: guards must judge fresh state, and caching
	// every Deployment in the cluster isn't worth a 10-minute loop.
	// Defaults to Client.
	APIReader client.Reader
	// Source serves recommendations; nil means no control plane is
	// configured and the policy reports DataAvailable=False.
	Source RightsizingSource
	// Audit defaults to NoopAuditEmitter.
	Audit AuditEmitter
	// Recorder emits Kubernetes Events on patched workloads; optional.
	Recorder events.EventRecorder

	// ClusterID scopes recommendations to this cluster (CLUSTER_ID).
	ClusterID string
	// ClusterLabels are matched by spec.scope.clusterSelector.
	ClusterLabels map[string]string

	// RequeueAfter defaults to 10 minutes.
	RequeueAfter time.Duration
	// Now is injectable for tests.
	Now func() time.Time
}

// +kubebuilder:rbac:groups=kubehero.kubehero.io,resources=rightsizingpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kubehero.kubehero.io,resources=rightsizingpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kubehero.kubehero.io,resources=rightsizingpolicies/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments;statefulsets,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list
// +kubebuilder:rbac:groups=autoscaling.k8s.io,resources=verticalpodautoscalers,verbs=get;list

func (r *RightsizingPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var rp kubeherov1.RightsizingPolicy
	if err := r.Get(ctx, req.NamespacedName, &rp); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	now := r.now()
	rp.Status.ObservedGeneration = rp.Generation
	armed := IsArmed(&rp, rp.Spec.HumanArm)
	setArmedCondition(&rp, armed)

	spec, err := resolveSpec(&rp)
	if err != nil {
		resetEvaluation(&rp)
		meta.SetStatusCondition(&rp.Status.Conditions,
			NewCondition(&rp, ConditionReady, "False", ReasonInvalidSpec, err.Error()))
		// No requeue: nothing changes until the spec does.
		return ctrl.Result{}, r.updateStatus(ctx, &rp)
	}

	inScope, err := ClusterInScope(rp.Spec.Scope.ClusterSelector, r.ClusterLabels)
	if err != nil {
		resetEvaluation(&rp)
		meta.SetStatusCondition(&rp.Status.Conditions,
			NewCondition(&rp, ConditionReady, "False", ReasonInvalidSpec, err.Error()))
		return ctrl.Result{}, r.updateStatus(ctx, &rp)
	}
	if !inScope {
		resetEvaluation(&rp)
		meta.SetStatusCondition(&rp.Status.Conditions, NewCondition(&rp, ConditionReady, "True", ReasonOutOfScope,
			fmt.Sprintf("scope.clusterSelector does not match this cluster (%s)", labelString(r.ClusterLabels))))
		return ctrl.Result{RequeueAfter: r.requeueAfter()}, r.updateStatus(ctx, &rp)
	}

	evalCtx, cancel := context.WithTimeout(ctx, evaluationTimeout)
	defer cancel()
	ev, err := r.evaluate(evalCtx, &rp, spec, armed, now)
	if err != nil {
		// Transient (API server) failure: keep the last good status and
		// retry with backoff.
		log.Error(err, "Failed to evaluate RightsizingPolicy")
		return ctrl.Result{}, err
	}
	r.writeEvaluation(ctx, &rp, spec, ev, now)
	if err := r.updateStatus(ctx, &rp); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: r.requeueAfter()}, nil
}

// resolvedSpec is a policy's spec with defaults applied and validated.
type resolvedSpec struct {
	Mode            string
	Window          string
	HeadroomPct     float64
	MinConfidence   string
	MinReplicas     int32
	MaxChangePerDay int32
}

func resolveSpec(rp *kubeherov1.RightsizingPolicy) (resolvedSpec, error) {
	s := rp.Spec
	out := resolvedSpec{
		Mode:            s.Mode,
		Window:          defaultObservationWindow,
		HeadroomPct:     defaultHeadroomPct,
		MinConfidence:   minConfidenceOrDefault(s.MinConfidence),
		MinReplicas:     defaultMinReplicas,
		MaxChangePerDay: defaultMaxChangePerDay,
	}
	switch s.Mode {
	case kubeherov1.RightsizingModeRecommend, kubeherov1.RightsizingModeShadow, kubeherov1.RightsizingModeApply:
	default:
		return out, fmt.Errorf("spec.mode %q must be one of: recommend, shadow, apply", s.Mode)
	}
	if s.MinConfidence != "" && confidenceRank[s.MinConfidence] == 0 {
		return out, fmt.Errorf("spec.minConfidence %q must be one of: low, medium, high", s.MinConfidence)
	}
	if w := strings.TrimSpace(s.Safety.ObservationWindow); w != "" {
		m := windowRE.FindStringSubmatch(w)
		if m == nil {
			return out, fmt.Errorf("safety.observationWindow %q must look like 7d or 36h", w)
		}
		n, _ := strconv.Atoi(m[1])
		hours := n
		if m[2] == "d" {
			hours = n * 24
		}
		if hours < 1 || hours > 90*24 {
			return out, fmt.Errorf("safety.observationWindow %q must be between 1h and 90d", w)
		}
		out.Window = w
	}
	if s.Safety.P95HeadroomPct != nil {
		h := *s.Safety.P95HeadroomPct
		if h < 0 || h > 500 {
			return out, fmt.Errorf("safety.p95HeadroomPct %d must be within 0..500", h)
		}
		out.HeadroomPct = float64(h)
	}
	if s.TargetUtilization != nil {
		t := *s.TargetUtilization
		if t < 1 || t > 100 {
			return out, fmt.Errorf("spec.targetUtilization %d must be within 1..100", t)
		}
		// Sizing so usage lands at t% of the request means adding
		// 100/t − 1 on top of the observed percentile.
		out.HeadroomPct = math.Round((100.0/float64(t)-1)*100*100) / 100
	}
	if s.Safety.MinReplicas != nil {
		if *s.Safety.MinReplicas < 0 {
			return out, fmt.Errorf("safety.minReplicas %d must be >= 0", *s.Safety.MinReplicas)
		}
		out.MinReplicas = *s.Safety.MinReplicas
	}
	if s.Safety.MaxChangePerDay != nil {
		if *s.Safety.MaxChangePerDay < 0 {
			return out, fmt.Errorf("safety.maxChangePerDay %d must be >= 0", *s.Safety.MaxChangePerDay)
		}
		out.MaxChangePerDay = *s.Safety.MaxChangePerDay
	}
	if s.Scope.NamespaceSelector != nil {
		if _, err := metav1.LabelSelectorAsSelector(s.Scope.NamespaceSelector); err != nil {
			return out, fmt.Errorf("invalid scope.namespaceSelector: %w", err)
		}
	}
	return out, nil
}

// evaluation is one pass over the policy's scope.
type evaluation struct {
	Namespaces   int
	Workloads    int
	Truncated    bool
	Recs         []kubeherov1.ContainerRecommendation
	TotalSavings float64
	Changes      []kubeherov1.PlannedChange
	Applied      int
	DataStatus   string
	DataReason   string
	DataMessage  string
	NoSelector   bool
}

func (r *RightsizingPolicyReconciler) evaluate(
	ctx context.Context,
	rp *kubeherov1.RightsizingPolicy,
	spec resolvedSpec,
	armed bool,
	now time.Time,
) (*evaluation, error) {
	ev := &evaluation{NoSelector: rp.Spec.Scope.NamespaceSelector == nil}
	reader := r.reader()

	namespaces, err := namespacesInScope(ctx, reader, rp.Spec.Scope.NamespaceSelector)
	if err != nil {
		return nil, err
	}
	ev.Namespaces = len(namespaces)

	byNS := map[string][]workload{}
	for _, ns := range namespaces {
		ws, err := listWorkloads(ctx, reader, ns, rp.Spec.Exclude)
		if err != nil {
			return nil, err
		}
		if ev.Workloads+len(ws) > maxWorkloadsPerPolicy {
			ws = ws[:maxWorkloadsPerPolicy-ev.Workloads]
			ev.Truncated = true
		}
		if len(ws) > 0 {
			byNS[ns] = ws
			ev.Workloads += len(ws)
		}
		if ev.Truncated {
			break
		}
	}

	switch {
	case r.Source == nil:
		ev.DataStatus, ev.DataReason, ev.DataMessage = "False", ReasonControlPlaneUnset,
			"CONTROL_PLANE_URL is not set; no recommendations are available"
		return ev, nil
	case ev.Workloads == 0:
		ev.DataStatus, ev.DataReason, ev.DataMessage = "True", ReasonNoWorkloads,
			"no Deployments or StatefulSets in scope"
		return ev, nil
	}

	// ─── recommendations, one control-plane call per namespace ─────────
	nsNames := make([]string, 0, len(byNS))
	for ns := range byNS {
		nsNames = append(nsNames, ns)
	}
	sort.Strings(nsNames)

	recs := map[string]map[string]*kuberov1.RightsizingRecommendation{} // workload key → container → rec
	var failed []string
	demo := false
	live := 0
	for _, ns := range nsNames {
		resp, err := r.Source.ListRightsizing(ctx, &kuberov1.ListRightsizingRequest{
			ClusterId:   r.ClusterID,
			Namespace:   ns,
			Window:      spec.Window,
			HeadroomPct: spec.HeadroomPct,
			Limit:       recommendationLimit,
		})
		if err != nil {
			logf.FromContext(ctx).Error(err, "Failed to list rightsizing recommendations", "namespace", ns)
			failed = append(failed, ns)
			continue
		}
		if strings.EqualFold(resp.GetSource(), "demo") {
			// Demo fixtures describe no real workload; acting on them —
			// or even displaying them against real names — would lie.
			demo = true
			continue
		}
		live++
		joinRecommendations(recs, byNS[ns], ns, resp.GetRecommendations())
	}
	switch {
	case len(failed) > 0:
		ev.DataStatus, ev.DataReason = "False", ReasonControlPlaneError
		ev.DataMessage = fmt.Sprintf("ListRightsizing failed for %d/%d namespaces (%s)",
			len(failed), len(nsNames), strings.Join(firstN(failed, 5), ", "))
	case demo && live == 0:
		ev.DataStatus, ev.DataReason, ev.DataMessage = "False", ReasonDemoData,
			"the control plane is serving demo fixtures; they are never shown or applied as real recommendations"
	default:
		ev.DataStatus, ev.DataReason, ev.DataMessage = "True", ReasonLiveData,
			fmt.Sprintf("live recommendations for %d namespaces (window %s, headroom %.4g%%)",
				live, spec.Window, spec.HeadroomPct)
	}

	// ─── status recommendations ────────────────────────────────────────
	for _, ns := range nsNames {
		for _, w := range byNS[ns] {
			for _, c := range w.Containers {
				rec := recs[w.key()][c.Name]
				if rec == nil || strings.EqualFold(rec.GetDirection(), "ok") {
					continue
				}
				ev.Recs = append(ev.Recs, statusRecommendation(w, c, rec))
				ev.TotalSavings += finite(rec.GetSavingsUsdMonth())
			}
		}
	}

	if spec.Mode == kubeherov1.RightsizingModeRecommend {
		return ev, nil
	}

	// ─── shadow / apply: plan every workload through the guards ────────
	params := planParams{
		Mode:            spec.Mode,
		Armed:           armed,
		ClusterKnown:    r.ClusterID != "",
		MinConfidence:   spec.MinConfidence,
		MinReplicas:     spec.MinReplicas,
		MaxChangePerDay: spec.MaxChangePerDay,
		AdjustLimits:    rp.Spec.AdjustLimits,
		ChangesToday:    func(w workload) int32 { return changesToday(w.Annotations, now) },
	}
	vpas := &vpaLookup{reader: reader}
	for _, ns := range nsNames {
		var nsVPAs map[string][]vpaTarget
		var nsHPAs map[string]map[string]string
		var autoErr error
		lookedUp := false
		for _, w := range byNS[ns] {
			wrecs := recs[w.key()]
			if len(wrecs) == 0 {
				continue
			}
			if !lookedUp {
				lookedUp = true
				var vErr, hErr error
				nsVPAs, vErr = vpas.forNamespace(ctx, ns)
				nsHPAs, hErr = hpaResources(ctx, reader, ns)
				switch {
				case vErr != nil:
					autoErr = vErr
				case hErr != nil:
					autoErr = fmt.Errorf("cannot verify HorizontalPodAutoscaler targets: %w", hErr)
				}
			}
			auto := autoscalerInfo{
				VPAs:         nsVPAs[w.Kind+"/"+w.Name],
				HPAResources: nsHPAs[w.Kind+"/"+w.Name],
			}
			wp := planWorkload(w, wrecs, auto, autoErr, params)
			if wp == nil {
				continue
			}
			if spec.Mode == kubeherov1.RightsizingModeApply && len(wp.passing()) > 0 {
				if r.applyPlan(ctx, rp, wp, now) {
					ev.Applied++
				}
			}
			for _, cp := range wp.Containers {
				ev.Changes = append(ev.Changes, cp.Change)
			}
		}
	}
	return ev, nil
}

// joinRecommendations indexes a namespace's recommendations by the
// workloads that actually exist. Rows naming a workload that isn't in
// scope (deleted, excluded, a different cluster's) are dropped here.
func joinRecommendations(
	into map[string]map[string]*kuberov1.RightsizingRecommendation,
	ws []workload,
	ns string,
	recs []*kuberov1.RightsizingRecommendation,
) {
	byName := map[string][]workload{}
	for _, w := range ws {
		byName[w.Name] = append(byName[w.Name], w)
	}
	for _, rec := range recs {
		if rec == nil {
			continue
		}
		if rn := rec.GetNamespace(); rn != "" && rn != ns {
			continue
		}
		kind := strings.ToLower(rec.GetWorkloadKind())
		for _, w := range byName[rec.GetWorkload()] {
			if kind != "" && kind != strings.ToLower(w.Kind) {
				continue
			}
			if into[w.key()] == nil {
				into[w.key()] = map[string]*kuberov1.RightsizingRecommendation{}
			}
			into[w.key()][rec.GetContainer()] = rec
		}
	}
}

func statusRecommendation(w workload, c corev1.Container, rec *kuberov1.RightsizingRecommendation) kubeherov1.ContainerRecommendation {
	out := kubeherov1.ContainerRecommendation{
		Namespace: w.Namespace, Workload: w.Name, Kind: w.Kind, Container: c.Name,
		SavingsUSDMonth: money(finite(rec.GetSavingsUsdMonth())),
		Confidence:      rec.GetConfidence(),
		Reason:          truncate(rec.GetReason(), 256),
		OOMKills:        rec.GetOomKills(),
	}
	if q, ok := c.Resources.Requests[corev1.ResourceCPU]; ok {
		q := q.DeepCopy()
		out.CurrentCPURequest = &q
	}
	if q, ok := c.Resources.Requests[corev1.ResourceMemory]; ok {
		q := q.DeepCopy()
		out.CurrentMemoryRequest = &q
	}
	if v := rec.GetCpuRecommendedCores(); v > 0 && !math.IsInf(v, 0) {
		q := resource.NewMilliQuantity(int64(math.Ceil(v*1000)), resource.DecimalSI)
		out.RecommendedCPURequest = q
	}
	if v := rec.GetMemRecommendedBytes(); v > 0 {
		q := resource.NewQuantity(roundUp(int64(v), memRoundBytes), resource.BinarySI)
		out.RecommendedMemoryRequest = q
	}
	return out
}

// applyPlan patches one workload's passing containers in a single
// strategic-merge patch (one rollout), then records audit + event.
// Returns whether the patch landed. Failures mark the plans failed.
func (r *RightsizingPolicyReconciler) applyPlan(
	ctx context.Context,
	rp *kubeherov1.RightsizingPolicy,
	wp *workloadPlan,
	now time.Time,
) bool {
	log := logf.FromContext(ctx).WithValues("workload", wp.W.key())
	passing := wp.passing()
	changeID := newChangeID()

	change := RightsizeChange{
		ChangeID:  changeID,
		AppliedAt: now.UTC().Format(time.RFC3339),
		Policy:    rp.Namespace + "/" + rp.Name,
	}
	containers := make([]map[string]any, 0, len(passing))
	previousSpec := make([]map[string]any, 0, len(passing))
	savings := 0.0
	for _, cp := range passing {
		cc := ContainerChange{
			Name:     cp.Change.Container,
			Previous: ResourceValues{Requests: quantities(cp.PrevRequests), Limits: quantities(cp.PrevLimits)},
			Applied:  ResourceValues{Requests: quantities(cp.Requests), Limits: quantities(cp.Limits)},
		}
		change.Containers = append(change.Containers, cc)
		res := map[string]any{"requests": quantities(cp.Requests)}
		if len(cp.Limits) > 0 {
			res["limits"] = quantities(cp.Limits)
		}
		containers = append(containers, map[string]any{"name": cp.Change.Container, "resources": res})
		for _, c := range wp.W.Containers {
			if c.Name == cp.Change.Container {
				previousSpec = append(previousSpec, map[string]any{"name": c.Name, "resources": c.Resources})
			}
		}
		savings += cp.Savings
	}

	history, err := parseHistory(wp.W.Annotations).withChange(change).marshal()
	if err != nil {
		markFailed(passing, err)
		return false
	}
	annotations := counterPatch(wp.W.Annotations, now)
	annotations[AnnotationRightsizePrevious] = &history

	patch := map[string]any{
		"metadata": map[string]any{
			// Optimistic concurrency: if the workload changed since the
			// guards read it, the API server rejects the patch.
			"resourceVersion": wp.W.Obj.GetResourceVersion(),
			"annotations":     annotations,
		},
		"spec": map[string]any{
			"template": map[string]any{
				"spec": map[string]any{"containers": containers},
			},
		},
	}
	body, err := json.Marshal(patch)
	if err != nil {
		markFailed(passing, err)
		return false
	}
	if err := r.Patch(ctx, wp.W.Obj, client.RawPatch(types.StrategicMergePatchType, body),
		client.FieldOwner(FieldManager)); err != nil {
		log.Error(err, "Failed to patch workload resources")
		markFailed(passing, err)
		return false
	}

	for _, cp := range passing {
		cp.Change.Outcome = kubeherov1.ChangeOutcomeApplied
		cp.Change.ChangeID = changeID
	}
	log.Info("Applied rightsizing change", "changeId", changeID, "containers", len(passing), "savingsUsdMonth", money(savings))

	// Audit is best effort: the change already happened and is fully
	// recoverable from the workload annotation even if the cp is down.
	if err := r.audit().Emit(ctx, AuditEvent{
		Action:         "rightsize.apply",
		TargetKind:     wp.W.Kind,
		TargetName:     wp.W.Namespace + "/" + wp.W.Name,
		ActorSub:       "operator",
		ClusterID:      r.ClusterID,
		Outcome:        "applied",
		EffectUsdMonth: math.Max(0, savings),
		PayloadJSON: map[string]any{
			"changeId":        changeID,
			"policy":          change.Policy,
			"namespace":       wp.W.Namespace,
			"workload":        wp.W.Name,
			"kind":            wp.W.Kind,
			"containers":      change.Containers,
			"previousSpec":    map[string]any{"containers": previousSpec},
			"savingsUsdMonth": savings,
			"undo":            "kubehero undo " + changeID,
		},
	}); err != nil {
		log.Error(err, "Failed to emit rightsize.apply audit event", "changeId", changeID)
	}

	if r.Recorder != nil {
		r.Recorder.Eventf(wp.W.Obj, rp, corev1.EventTypeNormal, "RightsizeApplied", "Rightsize",
			"%s (policy %s, change %s; undo: kubehero undo %s)",
			describeChange(passing), change.Policy, changeID, changeID)
	}
	return true
}

func markFailed(cps []*containerPlan, err error) {
	for _, cp := range cps {
		cp.OK = false
		cp.Change.Outcome = kubeherov1.ChangeOutcomeFailed
		cp.Change.Reason = truncate("patch failed: "+err.Error(), 256)
	}
}

func describeChange(cps []*containerPlan) string {
	parts := make([]string, 0, len(cps))
	for _, cp := range cps {
		var bits []string
		if c := cp.Change.CPURequest; c != nil {
			bits = append(bits, "cpu "+c.From.String()+"→"+c.To.String())
		}
		if c := cp.Change.MemoryRequest; c != nil {
			bits = append(bits, "memory "+c.From.String()+"→"+c.To.String())
		}
		parts = append(parts, cp.Change.Container+": "+strings.Join(bits, ", "))
	}
	return truncate(strings.Join(parts, "; "), 700)
}

// writeEvaluation folds an evaluation into the policy status.
func (r *RightsizingPolicyReconciler) writeEvaluation(
	ctx context.Context,
	rp *kubeherov1.RightsizingPolicy,
	spec resolvedSpec,
	ev *evaluation,
	now time.Time,
) {
	t := metav1.NewTime(now)
	rp.Status.LastEvaluated = &t
	rp.Status.WorkloadsInScope = int32(ev.Workloads)

	sort.SliceStable(ev.Recs, func(i, j int) bool {
		si, _ := strconv.ParseFloat(ev.Recs[i].SavingsUSDMonth, 64)
		sj, _ := strconv.ParseFloat(ev.Recs[j].SavingsUSDMonth, 64)
		return si > sj
	})
	rp.Status.Recommendations = capList(ev.Recs, maxStatusEntries)
	rp.Status.TotalSavingsUSDMonth = money(ev.TotalSavings)

	sortPlannedChanges(ev.Changes)
	if spec.Mode == kubeherov1.RightsizingModeRecommend {
		rp.Status.PlannedChanges = nil
		rp.Status.PlannedChangesHash = ""
	} else {
		rp.Status.PlannedChanges = capList(ev.Changes, maxStatusEntries)
	}
	if ev.Applied > 0 {
		rp.Status.LastApplied = &t
	}

	if spec.Mode == kubeherov1.RightsizingModeShadow {
		hash := planHash(ev.Changes)
		if hash != "" && hash != rp.Status.PlannedChangesHash {
			r.emitShadow(ctx, rp, ev.Changes)
		}
		rp.Status.PlannedChangesHash = hash
	} else {
		rp.Status.PlannedChangesHash = ""
	}

	meta.SetStatusCondition(&rp.Status.Conditions,
		NewCondition(rp, ConditionDataAvailable, ev.DataStatus, ev.DataReason, ev.DataMessage))

	msg := fmt.Sprintf("%s mode: %d workloads in %d namespaces, %d recommendations",
		spec.Mode, ev.Workloads, ev.Namespaces, len(ev.Recs))
	switch spec.Mode {
	case kubeherov1.RightsizingModeShadow:
		msg += fmt.Sprintf(", %d changes would apply (nothing is mutated)", countOutcome(ev.Changes, kubeherov1.ChangeOutcomePlanned))
	case kubeherov1.RightsizingModeApply:
		msg += fmt.Sprintf(", %d workloads patched this pass, %d changes blocked by guards",
			ev.Applied, countOutcome(ev.Changes, kubeherov1.ChangeOutcomeBlocked))
	}
	if ev.NoSelector {
		msg += "; scope.namespaceSelector is unset, which selects no namespaces ({} selects every non-system namespace)"
	}
	if ev.Truncated {
		msg += fmt.Sprintf("; only the first %d workloads were evaluated", maxWorkloadsPerPolicy)
	}
	meta.SetStatusCondition(&rp.Status.Conditions,
		NewCondition(rp, ConditionReady, "True", ReasonReconciled, msg))
}

func (r *RightsizingPolicyReconciler) emitShadow(ctx context.Context, rp *kubeherov1.RightsizingPolicy, changes []kubeherov1.PlannedChange) {
	var planned []kubeherov1.PlannedChange
	savings := 0.0
	for _, c := range changes {
		if c.Outcome != kubeherov1.ChangeOutcomePlanned {
			continue
		}
		planned = append(planned, c)
		v, _ := strconv.ParseFloat(c.SavingsUSDMonth, 64)
		savings += v
	}
	if err := r.audit().Emit(ctx, AuditEvent{
		Action:     "rightsize.shadow",
		TargetKind: "RightsizingPolicy",
		TargetName: rp.Namespace + "/" + rp.Name,
		ActorSub:   "operator",
		ClusterID:  r.ClusterID,
		Outcome:    "shadow",
		PayloadJSON: map[string]any{
			"policy":          rp.Namespace + "/" + rp.Name,
			"wouldApply":      capList(planned, maxStatusEntries),
			"changes":         len(planned),
			"savingsUsdMonth": savings,
		},
	}); err != nil {
		logf.FromContext(ctx).Error(err, "Failed to emit rightsize.shadow audit event")
	}
}

func setArmedCondition(rp *kubeherov1.RightsizingPolicy, armed bool) {
	status, reason, msg := "False", ReasonNotArmed,
		"apply mode will not mutate workloads until the policy is armed (`kubehero cap --arm`)"
	switch {
	case rp.Spec.Mode != kubeherov1.RightsizingModeApply:
		status, reason, msg = strconvBool(armed), ReasonArmNotNeeded,
			rp.Spec.Mode+" mode never mutates workloads; arming only matters in apply mode"
	case armed:
		status, reason, msg = "True", ReasonArmedOK,
			"apply mode patches workloads when every safety guard passes"
	}
	meta.SetStatusCondition(&rp.Status.Conditions, NewCondition(rp, ConditionArmed, status, reason, msg))
}

func resetEvaluation(rp *kubeherov1.RightsizingPolicy) {
	rp.Status.Recommendations = nil
	rp.Status.PlannedChanges = nil
	rp.Status.PlannedChangesHash = ""
	rp.Status.TotalSavingsUSDMonth = ""
	rp.Status.WorkloadsInScope = 0
	meta.RemoveStatusCondition(&rp.Status.Conditions, ConditionDataAvailable)
}

func (r *RightsizingPolicyReconciler) updateStatus(ctx context.Context, rp *kubeherov1.RightsizingPolicy) error {
	if err := r.Status().Update(ctx, rp); err != nil {
		if apierrors.IsConflict(err) {
			// The policy changed under us; the watch will requeue it.
			return nil
		}
		return fmt.Errorf("status update: %w", err)
	}
	return nil
}

func (r *RightsizingPolicyReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *RightsizingPolicyReconciler) audit() AuditEmitter {
	if r.Audit != nil {
		return r.Audit
	}
	return NoopAuditEmitter{}
}

func (r *RightsizingPolicyReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *RightsizingPolicyReconciler) requeueAfter() time.Duration {
	if r.RequeueAfter > 0 {
		return r.RequeueAfter
	}
	return defaultRightsizingRequeue
}

// SetupWithManager reconciles on spec changes and on annotation changes
// (arming is an annotation); status writes don't retrigger, and the
// 10-minute requeue picks up new recommendations.
func (r *RightsizingPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kubeherov1.RightsizingPolicy{}, builder.WithPredicates(
			predicate.Or(predicate.GenerationChangedPredicate{}, predicate.AnnotationChangedPredicate{}))).
		Named("rightsizingpolicy").
		Complete(r)
}

// ─── small helpers ─────────────────────────────────────────────────────

func quantities(rl corev1.ResourceList) map[string]string {
	if len(rl) == 0 {
		return nil
	}
	out := make(map[string]string, len(rl))
	for k, v := range rl {
		out[string(k)] = v.String()
	}
	return out
}

func capList[T any](in []T, n int) []T {
	if len(in) > n {
		return in[:n]
	}
	return in
}

func countOutcome(cs []kubeherov1.PlannedChange, outcome string) int {
	n := 0
	for _, c := range cs {
		if c.Outcome == outcome {
			n++
		}
	}
	return n
}

func firstN(s []string, n int) []string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func finite(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return v
}

// truncate caps s at n runes (status fields and event notes have size
// limits, and recommender reasons are free text).
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func strconvBool(b bool) string {
	if b {
		return "True"
	}
	return "False"
}

func labelString(m map[string]string) string {
	if len(m) == 0 {
		return "no cluster labels; set CLUSTER_LABELS"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+m[k])
	}
	return strings.Join(parts, ",")
}
