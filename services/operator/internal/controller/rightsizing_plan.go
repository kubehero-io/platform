// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	kubeherov1 "github.com/kubehero-io/platform/services/operator/api/v1"
)

// This file is the pure half of the rightsizer: given workloads, the
// recommender's output and the policy, decide what may change. No API
// calls happen here, which is what makes every guard table-testable.

// Step and noise bounds. A single change may never cut a request below
// half or raise it above double; changes smaller than minRelativeDelta
// (or the absolute floors) are not worth rolling every pod for.
const (
	maxShrinkFactor  = 0.5
	maxGrowFactor    = 2.0
	minRelativeDelta = 0.10
	minCPUDeltaMilli = 5
	minMemDeltaBytes = 16 << 20
	memRoundBytes    = 1 << 20 // recommend memory in whole MiB
)

var confidenceRank = map[string]int{"low": 1, "medium": 2, "high": 3}

// workload is the part of a Deployment / StatefulSet the planner needs.
type workload struct {
	Kind        string // Deployment | StatefulSet
	Namespace   string
	Name        string
	Replicas    int32
	Annotations map[string]string
	Containers  []corev1.Container
	// RolloutBlocker is non-empty while the workload is mid-rollout or
	// has unavailable replicas — never stack a change on an unsettled
	// workload (a bad rollout would be indistinguishable from ours).
	RolloutBlocker string
	Obj            client.Object
}

func (w workload) key() string { return w.Kind + "/" + w.Namespace + "/" + w.Name }

func workloadFromDeployment(d *appsv1.Deployment) workload {
	replicas := int32(1)
	if d.Spec.Replicas != nil {
		replicas = *d.Spec.Replicas
	}
	return workload{
		Kind: "Deployment", Namespace: d.Namespace, Name: d.Name,
		Replicas: replicas, Annotations: d.Annotations,
		Containers:     d.Spec.Template.Spec.Containers,
		RolloutBlocker: deploymentRolloutBlocker(d, replicas),
		Obj:            d,
	}
}

func workloadFromStatefulSet(s *appsv1.StatefulSet) workload {
	replicas := int32(1)
	if s.Spec.Replicas != nil {
		replicas = *s.Spec.Replicas
	}
	return workload{
		Kind: "StatefulSet", Namespace: s.Namespace, Name: s.Name,
		Replicas: replicas, Annotations: s.Annotations,
		Containers:     s.Spec.Template.Spec.Containers,
		RolloutBlocker: statefulSetRolloutBlocker(s, replicas),
		Obj:            s,
	}
}

func deploymentRolloutBlocker(d *appsv1.Deployment, replicas int32) string {
	st := d.Status
	switch {
	case d.Spec.Paused:
		return "deployment is paused"
	case st.ObservedGeneration < d.Generation:
		return "rollout pending (spec not yet observed by the deployment controller)"
	case st.UpdatedReplicas < replicas:
		return fmt.Sprintf("rollout in progress (%d/%d replicas updated)", st.UpdatedReplicas, replicas)
	case st.Replicas > st.UpdatedReplicas:
		return fmt.Sprintf("rollout in progress (%d old replicas still running)", st.Replicas-st.UpdatedReplicas)
	case st.UnavailableReplicas > 0:
		return fmt.Sprintf("%d replicas unavailable", st.UnavailableReplicas)
	case st.AvailableReplicas < replicas:
		return fmt.Sprintf("%d/%d replicas available", st.AvailableReplicas, replicas)
	}
	return ""
}

func statefulSetRolloutBlocker(s *appsv1.StatefulSet, replicas int32) string {
	st := s.Status
	switch {
	case st.ObservedGeneration < s.Generation:
		return "rollout pending (spec not yet observed by the statefulset controller)"
	case st.UpdateRevision != "" && st.CurrentRevision != st.UpdateRevision:
		return "rolling update in progress"
	case st.UpdatedReplicas < replicas:
		return fmt.Sprintf("rollout in progress (%d/%d replicas updated)", st.UpdatedReplicas, replicas)
	case st.ReadyReplicas < replicas:
		return fmt.Sprintf("%d/%d replicas ready", st.ReadyReplicas, replicas)
	}
	return ""
}

// autoscalerInfo is what the planner knows about autoscalers pointing at
// one workload.
type autoscalerInfo struct {
	// VPAs lists VerticalPodAutoscalers targeting the workload, and the
	// containers each one explicitly opts out (containerPolicies mode=Off).
	VPAs []vpaTarget
	// HPAResources are resources an HPA scales the workload on by
	// Utilization — changing that request silently moves the HPA's
	// target, so the rightsizer leaves those requests alone. Keys are
	// "cpu" / "memory" (every container) or "<container>/cpu" (a
	// ContainerResource metric); values name the HPA.
	HPAResources map[string]string
}

// hpaFor names the HPA that scales container c on resource res, if any.
func (a autoscalerInfo) hpaFor(c, res string) string {
	if n := a.HPAResources[res]; n != "" {
		return n
	}
	return a.HPAResources[c+"/"+res]
}

type vpaTarget struct {
	Name     string
	OptedOut map[string]bool // container name → VPA explicitly not managing it
}

// managedByVPA reports the VPA that manages container c, if any.
func (a autoscalerInfo) managedByVPA(c string) string {
	for _, v := range a.VPAs {
		if !v.OptedOut[c] && !v.OptedOut["*"] {
			return v.Name
		}
	}
	return ""
}

// planParams carries the resolved policy knobs.
type planParams struct {
	Mode            string
	Armed           bool
	ClusterKnown    bool
	MinConfidence   string
	MinReplicas     int32
	MaxChangePerDay int32
	AdjustLimits    bool
	ChangesToday    func(w workload) int32
}

// containerPlan is one container's evaluated change.
type containerPlan struct {
	Change kubeherov1.PlannedChange
	// Requests / Limits hold only the keys that change (the patch body).
	Requests corev1.ResourceList
	Limits   corev1.ResourceList
	// Prev holds the pre-change value of every key in Requests / Limits.
	PrevRequests corev1.ResourceList
	PrevLimits   corev1.ResourceList
	Savings      float64
	OK           bool // every guard passed
}

// workloadPlan groups the container plans of one workload; apply mode
// patches all passing containers of a workload in one rollout.
type workloadPlan struct {
	W          workload
	Containers []*containerPlan
}

// passing returns the containers cleared to change.
func (p workloadPlan) passing() []*containerPlan {
	var out []*containerPlan
	for _, c := range p.Containers {
		if c.OK {
			out = append(out, c)
		}
	}
	return out
}

// planWorkload evaluates every recommended container of w. recs maps a
// container name to its recommendation. It returns nil when nothing
// about w would change even with every guard waived.
func planWorkload(
	w workload,
	recs map[string]*kuberov1.RightsizingRecommendation,
	auto autoscalerInfo,
	vpaErr error,
	p planParams,
) *workloadPlan {
	wp := &workloadPlan{W: w}
	for i := range w.Containers {
		c := &w.Containers[i]
		rec := recs[c.Name]
		if rec == nil {
			continue
		}
		cp := planContainer(w, c, rec, auto, p.AdjustLimits)
		if cp == nil {
			continue
		}
		applyContainerGuards(cp, rec, auto, p.MinConfidence)
		wp.Containers = append(wp.Containers, cp)
	}
	if len(wp.Containers) == 0 {
		return nil
	}

	// Workload-level guards, in the order a human would ask them. The
	// first failing guard blocks every container of the workload.
	blocker := workloadBlocker(w, p, vpaErr)
	for _, cp := range wp.Containers {
		if cp.Change.Outcome == kubeherov1.ChangeOutcomeBlocked {
			continue // a container-level guard already refused it
		}
		if blocker != "" {
			block(cp, blocker)
			continue
		}
		cp.OK = true
		cp.Change.Outcome = kubeherov1.ChangeOutcomePlanned
	}
	return wp
}

func workloadBlocker(w workload, p planParams, vpaErr error) string {
	if ts, ok := w.Annotations[AnnotationRightsizeReverted]; ok {
		return fmt.Sprintf("reverted by a human at %s (kubehero undo); remove annotation %s to re-enable",
			ts, AnnotationRightsizeReverted)
	}
	if p.Mode == kubeherov1.RightsizingModeApply && !p.Armed {
		return "policy is not armed (humanArm) — arm it with `kubehero cap --arm` or the dashboard"
	}
	if p.Mode == kubeherov1.RightsizingModeApply && !p.ClusterKnown {
		return "CLUSTER_ID is unset, so fleet recommendations cannot be attributed to this cluster"
	}
	if w.Replicas < p.MinReplicas {
		return fmt.Sprintf("%d replicas < safety.minReplicas %d", w.Replicas, p.MinReplicas)
	}
	if w.RolloutBlocker != "" {
		return w.RolloutBlocker
	}
	if vpaErr != nil {
		return "cannot verify VerticalPodAutoscaler ownership: " + vpaErr.Error()
	}
	if p.MaxChangePerDay <= 0 {
		return "safety.maxChangePerDay is 0"
	}
	if p.ChangesToday != nil {
		if n := p.ChangesToday(w); n >= p.MaxChangePerDay {
			return fmt.Sprintf("safety.maxChangePerDay reached (%d/%d changes today, UTC)", n, p.MaxChangePerDay)
		}
	}
	return ""
}

func block(cp *containerPlan, reason string) {
	cp.OK = false
	cp.Change.Outcome = kubeherov1.ChangeOutcomeBlocked
	if cp.Change.Reason != "" {
		reason = reason + "; " + cp.Change.Reason
	}
	cp.Change.Reason = reason
}

// planContainer computes one container's bounded targets. It returns
// nil when the recommendation doesn't move any request by a meaningful
// amount. Container-level guards (confidence, VPA) mark the plan
// blocked but still return it, so status shows what was refused.
func planContainer(
	w workload,
	c *corev1.Container,
	rec *kuberov1.RightsizingRecommendation,
	auto autoscalerInfo,
	adjustLimits bool,
) *containerPlan {
	if strings.EqualFold(rec.GetDirection(), "ok") {
		return nil
	}
	cp := &containerPlan{
		Change: kubeherov1.PlannedChange{
			Namespace: w.Namespace, Workload: w.Name, Kind: w.Kind, Container: c.Name,
		},
		Requests: corev1.ResourceList{}, Limits: corev1.ResourceList{},
		PrevRequests: corev1.ResourceList{}, PrevLimits: corev1.ResourceList{},
	}
	var notes []string
	var fractions []float64 // achieved share of each wanted change

	// ─── CPU ──────────────────────────────────────────────────────────
	if cur, ok := c.Resources.Requests[corev1.ResourceCPU]; ok && rec.GetCpuRecommendedCores() > 0 {
		want := int64(math.Ceil(rec.GetCpuRecommendedCores() * 1000))
		curM := cur.MilliValue()
		if hpa := auto.hpaFor(c.Name, "cpu"); hpa != "" {
			if meaningful(curM, want, minCPUDeltaMilli) {
				notes = append(notes, fmt.Sprintf("cpu left alone: HPA %s scales on cpu utilization", hpa))
				fractions = append(fractions, 0)
			}
		} else {
			target, note := bound(curM, want)
			if lim, ok := c.Resources.Limits[corev1.ResourceCPU]; ok && !adjustLimits && target > lim.MilliValue() {
				target = lim.MilliValue()
				note = "capped at the cpu limit"
			}
			if meaningful(curM, target, minCPUDeltaMilli) {
				to := *resource.NewMilliQuantity(target, resource.DecimalSI)
				cp.Requests[corev1.ResourceCPU] = to
				cp.PrevRequests[corev1.ResourceCPU] = cur.DeepCopy()
				cp.Change.CPURequest = &kubeherov1.QuantityChange{From: cur.DeepCopy(), To: to}
				if adjustLimits {
					scaleLimit(c, cp, corev1.ResourceCPU, curM, target, true)
				}
				fractions = append(fractions, achieved(curM, want, target))
				if note != "" {
					notes = append(notes, "cpu "+note)
				}
			} else if meaningful(curM, want, minCPUDeltaMilli) {
				fractions = append(fractions, 0)
				if note != "" {
					notes = append(notes, "cpu "+note)
				}
			}
		}
	}

	// ─── Memory ───────────────────────────────────────────────────────
	if cur, ok := c.Resources.Requests[corev1.ResourceMemory]; ok && rec.GetMemRecommendedBytes() > 0 {
		want := roundUp(int64(rec.GetMemRecommendedBytes()), memRoundBytes)
		curB := cur.Value()
		observedMax := int64(rec.GetMemMaxBytes())
		switch {
		case auto.hpaFor(c.Name, "memory") != "" && meaningful(curB, want, minMemDeltaBytes):
			notes = append(notes, fmt.Sprintf("memory left alone: HPA %s scales on memory utilization", auto.hpaFor(c.Name, "memory")))
			fractions = append(fractions, 0)
		case auto.hpaFor(c.Name, "memory") != "":
			// HPA-managed and the change is noise anyway: nothing to say.
		case want < curB && rec.GetOomKills() > 0:
			// OOM is not a trade-off: never shrink memory that was killed.
			if meaningful(curB, want, minMemDeltaBytes) {
				notes = append(notes, fmt.Sprintf("memory decrease blocked: %d OOM kills in the window", rec.GetOomKills()))
				fractions = append(fractions, 0)
			}
		case want < curB && observedMax <= 0:
			if meaningful(curB, want, minMemDeltaBytes) {
				notes = append(notes, "memory decrease blocked: observed max unknown")
				fractions = append(fractions, 0)
			}
		default:
			if observedMax > 0 && want < roundUp(observedMax, memRoundBytes) {
				want = roundUp(observedMax, memRoundBytes)
				notes = append(notes, "memory raised to the observed max")
			}
			target, note := bound(curB, want)
			target = roundUp(target, memRoundBytes)
			if lim, ok := c.Resources.Limits[corev1.ResourceMemory]; ok && !adjustLimits && target > lim.Value() {
				target = lim.Value()
				note = "capped at the memory limit"
			}
			if meaningful(curB, target, minMemDeltaBytes) {
				to := *resource.NewQuantity(target, resource.BinarySI)
				cp.Requests[corev1.ResourceMemory] = to
				cp.PrevRequests[corev1.ResourceMemory] = cur.DeepCopy()
				cp.Change.MemoryRequest = &kubeherov1.QuantityChange{From: cur.DeepCopy(), To: to}
				if adjustLimits {
					scaleLimit(c, cp, corev1.ResourceMemory, curB, target, false)
				}
				fractions = append(fractions, achieved(curB, want, target))
				if note != "" {
					notes = append(notes, "memory "+note)
				}
			} else if meaningful(curB, want, minMemDeltaBytes) {
				fractions = append(fractions, 0)
				if note != "" {
					notes = append(notes, "memory "+note)
				}
			}
		}
	}

	if len(cp.Requests) == 0 && len(notes) == 0 {
		return nil // nothing meaningful to change, nothing refused
	}

	cp.Savings = estimateSavings(rec.GetSavingsUsdMonth(), fractions)
	cp.Change.SavingsUSDMonth = money(cp.Savings)
	cp.Change.Reason = strings.Join(notes, "; ")
	if len(cp.Requests) == 0 {
		// Every wanted change was refused by a safety rule (OOM, HPA,
		// unknown max); the notes say which.
		cp.Change.Outcome = kubeherov1.ChangeOutcomeBlocked
	}
	return cp
}

// applyContainerGuards applies the policy-dependent container guards
// (confidence, VPA). Split from planContainer so the target math stays
// independent of policy knobs.
func applyContainerGuards(cp *containerPlan, rec *kuberov1.RightsizingRecommendation, auto autoscalerInfo, minConfidence string) {
	if cp.Change.Outcome == kubeherov1.ChangeOutcomeBlocked {
		return
	}
	if confidenceRank[rec.GetConfidence()] < confidenceRank[minConfidence] {
		conf := rec.GetConfidence()
		if conf == "" {
			conf = "unknown"
		}
		block(cp, fmt.Sprintf("confidence %s < minConfidence %s", conf, minConfidence))
		return
	}
	if v := auto.managedByVPA(cp.Change.Container); v != "" {
		block(cp, fmt.Sprintf("VerticalPodAutoscaler %s manages this container", v))
	}
}

// bound clamps a target into [cur×0.5, cur×2] and says why when it did.
func bound(cur, want int64) (int64, string) {
	lo := int64(math.Ceil(float64(cur) * maxShrinkFactor))
	hi := int64(math.Floor(float64(cur) * maxGrowFactor))
	switch {
	case want < lo:
		return lo, "decrease limited to 50% per step"
	case want > hi:
		return hi, "increase limited to 100% per step"
	}
	return want, ""
}

// meaningful reports whether moving from cur to target is worth a rollout.
func meaningful(cur, target, absFloor int64) bool {
	d := target - cur
	if d < 0 {
		d = -d
	}
	if d < absFloor {
		return false
	}
	if cur <= 0 {
		return true
	}
	return float64(d)/float64(cur) >= minRelativeDelta
}

// achieved is the fraction of the wanted move the bounded target makes.
func achieved(cur, want, target int64) float64 {
	if want == cur {
		return 1
	}
	f := float64(target-cur) / float64(want-cur)
	return math.Max(0, math.Min(1, f))
}

// estimateSavings scales the recommender's full saving by how much of
// the recommended change this step actually makes. The recommender
// doesn't split its saving between cpu and memory, so each wanted
// resource weighs equally — an estimate, labelled as such in the docs.
func estimateSavings(full float64, fractions []float64) float64 {
	if len(fractions) == 0 || math.IsNaN(full) || math.IsInf(full, 0) {
		return 0
	}
	sum := 0.0
	for _, f := range fractions {
		sum += f
	}
	return full * sum / float64(len(fractions))
}

// scaleLimit keeps limit:request constant when adjustLimits is on.
func scaleLimit(c *corev1.Container, cp *containerPlan, name corev1.ResourceName, cur, target int64, milli bool) {
	lim, ok := c.Resources.Limits[name]
	if !ok || cur <= 0 {
		return
	}
	var limVal int64
	if milli {
		limVal = lim.MilliValue()
	} else {
		limVal = lim.Value()
	}
	newLim := int64(math.Ceil(float64(limVal) * float64(target) / float64(cur)))
	if newLim < target {
		newLim = target
	}
	var q resource.Quantity
	if milli {
		q = *resource.NewMilliQuantity(newLim, resource.DecimalSI)
	} else {
		q = *resource.NewQuantity(roundUp(newLim, memRoundBytes), resource.BinarySI)
	}
	if q.Cmp(lim) == 0 {
		return
	}
	cp.Limits[name] = q
	cp.PrevLimits[name] = lim.DeepCopy()
	change := &kubeherov1.QuantityChange{From: lim.DeepCopy(), To: q}
	if name == corev1.ResourceCPU {
		cp.Change.CPULimit = change
	} else {
		cp.Change.MemoryLimit = change
	}
}

func roundUp(v, unit int64) int64 {
	if unit <= 0 || v%unit == 0 {
		return v
	}
	return (v/unit + 1) * unit
}

func minConfidenceOrDefault(s string) string {
	if _, ok := confidenceRank[s]; ok {
		return s
	}
	return "medium"
}

// money renders USD with two decimals; the CRD stores money as a string
// because floats are not portable through CRD schemas.
func money(v float64) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return "0.00"
	}
	return strconv.FormatFloat(v, 'f', 2, 64)
}

// sortPlannedChanges orders status entries: applied / failed first (what
// happened), then planned, then blocked; larger savings first within.
func sortPlannedChanges(pcs []kubeherov1.PlannedChange) {
	rank := map[string]int{
		kubeherov1.ChangeOutcomeApplied: 0,
		kubeherov1.ChangeOutcomeFailed:  1,
		kubeherov1.ChangeOutcomePlanned: 2,
		kubeherov1.ChangeOutcomeBlocked: 3,
	}
	sort.SliceStable(pcs, func(i, j int) bool {
		if rank[pcs[i].Outcome] != rank[pcs[j].Outcome] {
			return rank[pcs[i].Outcome] < rank[pcs[j].Outcome]
		}
		si, _ := strconv.ParseFloat(pcs[i].SavingsUSDMonth, 64)
		sj, _ := strconv.ParseFloat(pcs[j].SavingsUSDMonth, 64)
		if si != sj {
			return si > sj
		}
		return pcs[i].Namespace+"/"+pcs[i].Workload+"/"+pcs[i].Container <
			pcs[j].Namespace+"/"+pcs[j].Workload+"/"+pcs[j].Container
	})
}

// planHash fingerprints the would-apply set for shadow-mode audit dedup.
func planHash(pcs []kubeherov1.PlannedChange) string {
	var lines []string
	for _, pc := range pcs {
		if pc.Outcome != kubeherov1.ChangeOutcomePlanned {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s/%s/%s/%s|%s|%s|%s|%s", pc.Namespace, pc.Kind, pc.Workload, pc.Container,
			qc(pc.CPURequest), qc(pc.MemoryRequest), qc(pc.CPULimit), qc(pc.MemoryLimit)))
	}
	if len(lines) == 0 {
		return ""
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:8])
}

func qc(c *kubeherov1.QuantityChange) string {
	if c == nil {
		return "-"
	}
	return c.From.String() + ">" + c.To.String()
}
