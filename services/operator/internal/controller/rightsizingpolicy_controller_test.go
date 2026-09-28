// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	kubeherov1 "github.com/kubehero-io/platform/services/operator/api/v1"
)

// fakeRightsizing serves canned recommendations per namespace.
type fakeRightsizing struct {
	mu    sync.Mutex
	byNS  map[string]*kuberov1.ListRightsizingResponse
	calls []*kuberov1.ListRightsizingRequest
	err   error
}

func (f *fakeRightsizing) ListRightsizing(
	_ context.Context,
	req *kuberov1.ListRightsizingRequest,
) (*kuberov1.ListRightsizingResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req)
	if f.err != nil {
		return nil, f.err
	}
	if r := f.byNS[req.GetNamespace()]; r != nil {
		return r, nil
	}
	return &kuberov1.ListRightsizingResponse{Source: "live"}, nil
}

func (f *fakeRightsizing) lastCall() *kuberov1.ListRightsizingRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return nil
	}
	return f.calls[len(f.calls)-1]
}

// safeEmitter is a goroutine-safe recording audit emitter.
type safeEmitter struct {
	mu     sync.Mutex
	events []AuditEvent
}

func (e *safeEmitter) Emit(_ context.Context, ev AuditEvent) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, ev)
	return nil
}

func (e *safeEmitter) byAction(action string) []AuditEvent {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []AuditEvent
	for _, ev := range e.events {
		if ev.Action == action {
			out = append(out, ev)
		}
	}
	return out
}

var nsCounter atomic.Int64

// newTestNamespace creates a uniquely named namespace (envtest never
// finishes namespace deletion, so tests never reuse names).
func newTestNamespace(ctx context.Context, prefix string) string {
	name := fmt.Sprintf("%s-%d", prefix, nsCounter.Add(1))
	Expect(k8sClient.Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"rightsize-test": name}},
	})).To(Succeed())
	return name
}

func quantity(s string) resource.Quantity { return resource.MustParse(s) }

// makeDeployment creates a Deployment with one "app" container and
// marks it settled (envtest runs no deployment controller).
func makeDeployment(ctx context.Context, ns, name string, replicas int32, cpu, mem string, annotations map[string]string) *appsv1.Deployment {
	labels := map[string]string{"app": name}
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Annotations: annotations},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(replicas),
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name:  "app",
					Image: "registry.example/app:1",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    quantity(cpu),
							corev1.ResourceMemory: quantity(mem),
						},
					},
				}}},
			},
		},
	}
	Expect(k8sClient.Create(ctx, d)).To(Succeed())
	settle(ctx, d)
	return d
}

func settle(ctx context.Context, d *appsv1.Deployment) {
	replicas := *d.Spec.Replicas
	d.Status = appsv1.DeploymentStatus{
		ObservedGeneration: d.Generation,
		Replicas:           replicas,
		UpdatedReplicas:    replicas,
		ReadyReplicas:      replicas,
		AvailableReplicas:  replicas,
	}
	Expect(k8sClient.Status().Update(ctx, d)).To(Succeed())
}

func rsRec(ns, workload string, cpuCores float64, memBytes uint64) *kuberov1.RightsizingRecommendation {
	return &kuberov1.RightsizingRecommendation{
		Id:                  "test/" + ns + "/" + workload + "/app",
		Cluster:             "test-cluster",
		Namespace:           ns,
		Workload:            workload,
		WorkloadKind:        "Deployment",
		Container:           "app",
		Replicas:            2,
		CpuRecommendedCores: cpuCores,
		MemRecommendedBytes: memBytes,
		MemMaxBytes:         memBytes,
		SavingsUsdMonth:     120.5,
		Confidence:          "high",
		Direction:           "downsize",
		Reason:              "p95 cpu well under request",
		Samples:             10000,
		Window:              "7d",
	}
}

func makePolicy(ctx context.Context, ns, mode string, mutate func(*kubeherov1.RightsizingPolicy)) *kubeherov1.RightsizingPolicy {
	rp := &kubeherov1.RightsizingPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "rs-" + mode},
		Spec: kubeherov1.RightsizingPolicySpec{
			Mode: mode,
			Scope: kubeherov1.Scope{NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"rightsize-test": ns},
			}},
		},
	}
	if mutate != nil {
		mutate(rp)
	}
	Expect(k8sClient.Create(ctx, rp)).To(Succeed())
	return rp
}

func armPolicy(ctx context.Context, rp *kubeherov1.RightsizingPolicy) {
	Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(rp), rp)).To(Succeed())
	if rp.Annotations == nil {
		rp.Annotations = map[string]string{}
	}
	rp.Annotations[AnnotationArmed] = "true"
	Expect(k8sClient.Update(ctx, rp)).To(Succeed())
}

var testNow = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

func newRightsizer(src RightsizingSource, audit AuditEmitter, rec events.EventRecorder) *RightsizingPolicyReconciler {
	return &RightsizingPolicyReconciler{
		Client:        k8sClient,
		Scheme:        k8sClient.Scheme(),
		Source:        src,
		Audit:         audit,
		Recorder:      rec,
		ClusterID:     "test-cluster",
		ClusterLabels: ParseClusterLabels("env=test", "test-cluster"),
		Now:           func() time.Time { return testNow },
	}
}

func reconcilePolicy(ctx context.Context, r *RightsizingPolicyReconciler, rp *kubeherov1.RightsizingPolicy) *kubeherov1.RightsizingPolicy {
	res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rp)})
	Expect(err).NotTo(HaveOccurred())
	Expect(res.RequeueAfter).To(BeNumerically(">", 0))
	out := &kubeherov1.RightsizingPolicy{}
	Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(rp), out)).To(Succeed())
	return out
}

func getDeployment(ctx context.Context, ns, name string) *appsv1.Deployment {
	d := &appsv1.Deployment{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, d)).To(Succeed())
	return d
}

func requestOf(d *appsv1.Deployment, res corev1.ResourceName) string {
	q := d.Spec.Template.Spec.Containers[0].Resources.Requests[res]
	return q.String()
}

func onlyChange(rp *kubeherov1.RightsizingPolicy) kubeherov1.PlannedChange {
	ExpectWithOffset(1, rp.Status.PlannedChanges).To(HaveLen(1), "planned changes: %+v", rp.Status.PlannedChanges)
	return rp.Status.PlannedChanges[0]
}

var _ = Describe("RightsizingPolicy Controller", func() {
	ctx := context.Background()

	It("reports Ready for a minimal recommend policy without a control plane", func() {
		ns := newTestNamespace(ctx, "rs-min")
		rp := makePolicy(ctx, ns, "recommend", nil)
		r := newRightsizer(nil, nil, nil)
		got := reconcilePolicy(ctx, r, rp)

		ready := meta.FindStatusCondition(got.Status.Conditions, ConditionReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionTrue))
		data := meta.FindStatusCondition(got.Status.Conditions, ConditionDataAvailable)
		Expect(data).NotTo(BeNil())
		Expect(data.Reason).To(Equal(ReasonControlPlaneUnset))
	})

	It("rejects an invalid spec without requeueing", func() {
		ns := newTestNamespace(ctx, "rs-invalid")
		rp := makePolicy(ctx, ns, "recommend", func(p *kubeherov1.RightsizingPolicy) {
			p.Spec.Safety.ObservationWindow = "fortnight"
		})
		r := newRightsizer(&fakeRightsizing{}, nil, nil)
		res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rp)})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(BeZero())
		got := &kubeherov1.RightsizingPolicy{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(rp), got)).To(Succeed())
		ready := meta.FindStatusCondition(got.Status.Conditions, ConditionReady)
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal(ReasonInvalidSpec))
	})

	It("recommend mode surfaces recommendations and never mutates", func() {
		ns := newTestNamespace(ctx, "rs-rec")
		makeDeployment(ctx, ns, "api", 2, "2", "4Gi", nil)
		src := &fakeRightsizing{byNS: map[string]*kuberov1.ListRightsizingResponse{
			ns: {Source: "live", Recommendations: []*kuberov1.RightsizingRecommendation{
				rsRec(ns, "api", 0.8, 3<<30),
				rsRec(ns, "ghost", 0.1, 1<<30), // not a live workload: dropped
			}},
		}}
		emitter := &safeEmitter{}
		rp := makePolicy(ctx, ns, "recommend", func(p *kubeherov1.RightsizingPolicy) {
			p.Spec.TargetUtilization = ptr.To[int32](80)
			p.Spec.Safety.ObservationWindow = "14d"
		})
		got := reconcilePolicy(ctx, newRightsizer(src, emitter, nil), rp)

		By("asking the control plane with the translated headroom and window")
		call := src.lastCall()
		Expect(call.GetNamespace()).To(Equal(ns))
		Expect(call.GetClusterId()).To(Equal("test-cluster"))
		Expect(call.GetWindow()).To(Equal("14d"))
		Expect(call.GetHeadroomPct()).To(BeNumerically("~", 25, 0.01)) // 100/80 − 1

		Expect(got.Status.WorkloadsInScope).To(Equal(int32(1)))
		Expect(got.Status.Recommendations).To(HaveLen(1))
		rec := got.Status.Recommendations[0]
		Expect(rec.Workload).To(Equal("api"))
		Expect(rec.Kind).To(Equal("Deployment"))
		Expect(rec.CurrentCPURequest.String()).To(Equal("2"))
		Expect(rec.RecommendedCPURequest.String()).To(Equal("800m"))
		Expect(rec.CurrentMemoryRequest.String()).To(Equal("4Gi"))
		Expect(rec.RecommendedMemoryRequest.String()).To(Equal("3Gi"))
		Expect(rec.SavingsUSDMonth).To(Equal("120.50"))
		Expect(rec.Confidence).To(Equal("high"))
		Expect(got.Status.TotalSavingsUSDMonth).To(Equal("120.50"))
		Expect(got.Status.PlannedChanges).To(BeEmpty())
		Expect(got.Status.LastEvaluated).NotTo(BeNil())

		Expect(requestOf(getDeployment(ctx, ns, "api"), corev1.ResourceCPU)).To(Equal("2"))
		Expect(emitter.events).To(BeEmpty())
	})

	It("ignores demo fixtures from the control plane", func() {
		ns := newTestNamespace(ctx, "rs-demo")
		makeDeployment(ctx, ns, "api", 2, "2", "4Gi", nil)
		src := &fakeRightsizing{byNS: map[string]*kuberov1.ListRightsizingResponse{
			ns: {Source: "demo", Recommendations: []*kuberov1.RightsizingRecommendation{rsRec(ns, "api", 0.8, 3<<30)}},
		}}
		rp := makePolicy(ctx, ns, "apply", func(p *kubeherov1.RightsizingPolicy) { p.Spec.HumanArm = ptr.To(false) })
		got := reconcilePolicy(ctx, newRightsizer(src, nil, nil), rp)
		Expect(got.Status.Recommendations).To(BeEmpty())
		Expect(meta.FindStatusCondition(got.Status.Conditions, ConditionDataAvailable).Reason).To(Equal(ReasonDemoData))
		Expect(requestOf(getDeployment(ctx, ns, "api"), corev1.ResourceCPU)).To(Equal("2"))
	})

	It("shadow mode records what would change and audits it once per plan", func() {
		ns := newTestNamespace(ctx, "rs-shadow")
		makeDeployment(ctx, ns, "api", 2, "2", "4Gi", nil)
		src := &fakeRightsizing{byNS: map[string]*kuberov1.ListRightsizingResponse{
			ns: {Source: "live", Recommendations: []*kuberov1.RightsizingRecommendation{rsRec(ns, "api", 1.2, 3<<30)}},
		}}
		emitter := &safeEmitter{}
		rp := makePolicy(ctx, ns, "shadow", nil)
		r := newRightsizer(src, emitter, nil)
		got := reconcilePolicy(ctx, r, rp)

		pc := onlyChange(got)
		Expect(pc.Outcome).To(Equal(kubeherov1.ChangeOutcomePlanned))
		Expect(pc.CPURequest.From.String()).To(Equal("2"))
		Expect(pc.CPURequest.To.String()).To(Equal("1200m"))
		Expect(pc.MemoryRequest.To.String()).To(Equal("3Gi"))
		Expect(got.Status.PlannedChangesHash).NotTo(BeEmpty())
		Expect(emitter.byAction("rightsize.shadow")).To(HaveLen(1))

		By("not mutating the workload")
		Expect(requestOf(getDeployment(ctx, ns, "api"), corev1.ResourceCPU)).To(Equal("2"))

		By("not re-auditing an unchanged plan")
		reconcilePolicy(ctx, r, rp)
		Expect(emitter.byAction("rightsize.shadow")).To(HaveLen(1))
	})

	It("apply mode patches an armed policy's workloads and records undo data", func() {
		ns := newTestNamespace(ctx, "rs-apply")
		makeDeployment(ctx, ns, "api", 2, "2", "4Gi", nil)
		src := &fakeRightsizing{byNS: map[string]*kuberov1.ListRightsizingResponse{
			ns: {Source: "live", Recommendations: []*kuberov1.RightsizingRecommendation{rsRec(ns, "api", 0.5, 3<<30)}},
		}}
		emitter := &safeEmitter{}
		recorder := events.NewFakeRecorder(10)
		rp := makePolicy(ctx, ns, "apply", nil)
		armPolicy(ctx, rp)
		got := reconcilePolicy(ctx, newRightsizer(src, emitter, recorder), rp)

		pc := onlyChange(got)
		Expect(pc.Outcome).To(Equal(kubeherov1.ChangeOutcomeApplied), pc.Reason)
		Expect(pc.ChangeID).To(HavePrefix("rs-"))
		By("bounding the cpu cut to 50% per step (0.5 cores wanted, 1 core applied)")
		Expect(pc.CPURequest.To.String()).To(Equal("1"))
		Expect(pc.Reason).To(ContainSubstring("decrease limited to 50% per step"))
		Expect(got.Status.LastApplied).NotTo(BeNil())

		d := getDeployment(ctx, ns, "api")
		Expect(requestOf(d, corev1.ResourceCPU)).To(Equal("1"))
		Expect(requestOf(d, corev1.ResourceMemory)).To(Equal("3Gi"))
		Expect(d.Annotations[dayKey(testNow)]).To(Equal("1"))

		By("recording the previous requests for kubehero undo")
		var hist RightsizeHistory
		Expect(json.Unmarshal([]byte(d.Annotations[AnnotationRightsizePrevious]), &hist)).To(Succeed())
		Expect(hist.Version).To(Equal(1))
		Expect(hist.Changes).To(HaveLen(1))
		ch := hist.Changes[0]
		Expect(ch.ChangeID).To(Equal(pc.ChangeID))
		Expect(ch.Policy).To(Equal(ns + "/rs-apply"))
		Expect(ch.Containers).To(HaveLen(1))
		Expect(ch.Containers[0].Previous.Requests).To(Equal(map[string]string{"cpu": "2", "memory": "4Gi"}))
		Expect(ch.Containers[0].Applied.Requests).To(Equal(map[string]string{"cpu": "1", "memory": "3Gi"}))

		By("auditing the change with its previous spec")
		applied := emitter.byAction("rightsize.apply")
		Expect(applied).To(HaveLen(1))
		Expect(applied[0].TargetKind).To(Equal("Deployment"))
		Expect(applied[0].TargetName).To(Equal(ns + "/api"))
		Expect(applied[0].Outcome).To(Equal("applied"))
		payload, err := json.Marshal(applied[0].PayloadJSON)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(payload)).To(ContainSubstring(`"changeId":"` + pc.ChangeID + `"`))
		Expect(string(payload)).To(ContainSubstring(`"previousSpec"`))
		Expect(string(payload)).To(ContainSubstring(`"cpu":"2"`))

		By("emitting a Kubernetes event on the workload")
		Expect(recorder.Events).To(Receive(ContainSubstring("RightsizeApplied")))

		By("recording the field manager on the patched fields")
		managers := []string{}
		for _, mf := range d.ManagedFields {
			managers = append(managers, mf.Manager)
		}
		Expect(managers).To(ContainElement(FieldManager))

		By("honouring maxChangePerDay on the next pass")
		settle(ctx, d)
		again := reconcilePolicy(ctx, newRightsizer(src, emitter, recorder), rp)
		next := onlyChange(again)
		Expect(next.Outcome).To(Equal(kubeherov1.ChangeOutcomeBlocked))
		Expect(next.Reason).To(ContainSubstring("maxChangePerDay"))
		Expect(emitter.byAction("rightsize.apply")).To(HaveLen(1))
	})

	DescribeTable("apply-mode guards block the change",
		func(setup func(ctx context.Context, ns string, src *fakeRightsizing), arm bool, wantReason string) {
			ns := newTestNamespace(ctx, "rs-guard")
			src := &fakeRightsizing{byNS: map[string]*kuberov1.ListRightsizingResponse{}}
			setup(ctx, ns, src)
			rp := makePolicy(ctx, ns, "apply", nil)
			if arm {
				armPolicy(ctx, rp)
			}
			emitter := &safeEmitter{}
			got := reconcilePolicy(ctx, newRightsizer(src, emitter, nil), rp)
			pc := onlyChange(got)
			Expect(pc.Outcome).To(Equal(kubeherov1.ChangeOutcomeBlocked))
			Expect(pc.Reason).To(ContainSubstring(wantReason))
			Expect(emitter.byAction("rightsize.apply")).To(BeEmpty())
			Expect(requestOf(getDeployment(ctx, ns, "api"), corev1.ResourceCPU)).To(Equal("2"))
		},
		Entry("policy not armed", func(ctx context.Context, ns string, src *fakeRightsizing) {
			makeDeployment(ctx, ns, "api", 2, "2", "4Gi", nil)
			src.byNS[ns] = &kuberov1.ListRightsizingResponse{Recommendations: []*kuberov1.RightsizingRecommendation{rsRec(ns, "api", 1.2, 3<<30)}}
		}, false, "not armed"),
		Entry("low confidence", func(ctx context.Context, ns string, src *fakeRightsizing) {
			makeDeployment(ctx, ns, "api", 2, "2", "4Gi", nil)
			r := rsRec(ns, "api", 1.2, 3<<30)
			r.Confidence = "low"
			src.byNS[ns] = &kuberov1.ListRightsizingResponse{Recommendations: []*kuberov1.RightsizingRecommendation{r}}
		}, true, "confidence low < minConfidence medium"),
		Entry("daily change budget spent", func(ctx context.Context, ns string, src *fakeRightsizing) {
			makeDeployment(ctx, ns, "api", 2, "2", "4Gi", map[string]string{dayKey(testNow): "1"})
			src.byNS[ns] = &kuberov1.ListRightsizingResponse{Recommendations: []*kuberov1.RightsizingRecommendation{rsRec(ns, "api", 1.2, 3<<30)}}
		}, true, "maxChangePerDay reached (1/1"),
		Entry("OOM kills block a memory cut", func(ctx context.Context, ns string, src *fakeRightsizing) {
			makeDeployment(ctx, ns, "api", 2, "2", "4Gi", nil)
			r := rsRec(ns, "api", 2, 2<<30) // cpu unchanged, memory halved
			r.OomKills = 3
			src.byNS[ns] = &kuberov1.ListRightsizingResponse{Recommendations: []*kuberov1.RightsizingRecommendation{r}}
		}, true, "memory decrease blocked: 3 OOM kills"),
		Entry("rollout in progress", func(ctx context.Context, ns string, src *fakeRightsizing) {
			d := makeDeployment(ctx, ns, "api", 3, "2", "4Gi", nil)
			d.Status.UpdatedReplicas = 1
			Expect(k8sClient.Status().Update(ctx, d)).To(Succeed())
			src.byNS[ns] = &kuberov1.ListRightsizingResponse{Recommendations: []*kuberov1.RightsizingRecommendation{rsRec(ns, "api", 1.2, 3<<30)}}
		}, true, "rollout in progress (1/3 replicas updated)"),
		Entry("unavailable replicas", func(ctx context.Context, ns string, src *fakeRightsizing) {
			d := makeDeployment(ctx, ns, "api", 3, "2", "4Gi", nil)
			d.Status.UnavailableReplicas = 1
			Expect(k8sClient.Status().Update(ctx, d)).To(Succeed())
			src.byNS[ns] = &kuberov1.ListRightsizingResponse{Recommendations: []*kuberov1.RightsizingRecommendation{rsRec(ns, "api", 1.2, 3<<30)}}
		}, true, "1 replicas unavailable"),
		Entry("VPA manages the container", func(ctx context.Context, ns string, src *fakeRightsizing) {
			makeDeployment(ctx, ns, "api", 2, "2", "4Gi", nil)
			vpa := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "autoscaling.k8s.io/v1",
				"kind":       "VerticalPodAutoscaler",
				"metadata":   map[string]any{"name": "api-vpa", "namespace": ns},
				"spec": map[string]any{
					"targetRef":    map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "name": "api"},
					"updatePolicy": map[string]any{"updateMode": "Auto"},
				},
			}}
			Expect(k8sClient.Create(ctx, vpa)).To(Succeed())
			src.byNS[ns] = &kuberov1.ListRightsizingResponse{Recommendations: []*kuberov1.RightsizingRecommendation{rsRec(ns, "api", 1.2, 3<<30)}}
		}, true, "VerticalPodAutoscaler api-vpa manages this container"),
		Entry("below minReplicas", func(ctx context.Context, ns string, src *fakeRightsizing) {
			makeDeployment(ctx, ns, "api", 0, "2", "4Gi", nil)
			src.byNS[ns] = &kuberov1.ListRightsizingResponse{Recommendations: []*kuberov1.RightsizingRecommendation{rsRec(ns, "api", 1.2, 3<<30)}}
		}, true, "0 replicas < safety.minReplicas 1"),
		Entry("reverted by a human", func(ctx context.Context, ns string, src *fakeRightsizing) {
			makeDeployment(ctx, ns, "api", 2, "2", "4Gi", map[string]string{AnnotationRightsizeReverted: "2026-09-27T10:00:00Z"})
			src.byNS[ns] = &kuberov1.ListRightsizingResponse{Recommendations: []*kuberov1.RightsizingRecommendation{rsRec(ns, "api", 1.2, 3<<30)}}
		}, true, "reverted by a human"),
	)

	It("leaves cpu alone when an HPA scales on cpu utilization", func() {
		ns := newTestNamespace(ctx, "rs-hpa")
		makeDeployment(ctx, ns, "api", 2, "2", "4Gi", nil)
		Expect(k8sClient.Create(ctx, &autoscalingv2.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "api-hpa"},
			Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "api"},
				MinReplicas:    ptr.To[int32](2),
				MaxReplicas:    10,
				Metrics: []autoscalingv2.MetricSpec{{
					Type: autoscalingv2.ResourceMetricSourceType,
					Resource: &autoscalingv2.ResourceMetricSource{
						Name:   corev1.ResourceCPU,
						Target: autoscalingv2.MetricTarget{Type: autoscalingv2.UtilizationMetricType, AverageUtilization: ptr.To[int32](70)},
					},
				}},
			},
		})).To(Succeed())
		src := &fakeRightsizing{byNS: map[string]*kuberov1.ListRightsizingResponse{
			ns: {Recommendations: []*kuberov1.RightsizingRecommendation{rsRec(ns, "api", 1.2, 3<<30)}},
		}}
		rp := makePolicy(ctx, ns, "apply", nil)
		armPolicy(ctx, rp)
		got := reconcilePolicy(ctx, newRightsizer(src, &safeEmitter{}, nil), rp)
		pc := onlyChange(got)
		Expect(pc.Outcome).To(Equal(kubeherov1.ChangeOutcomeApplied))
		Expect(pc.CPURequest).To(BeNil())
		Expect(pc.MemoryRequest.To.String()).To(Equal("3Gi"))
		Expect(pc.Reason).To(ContainSubstring("HPA api-hpa scales on cpu utilization"))
		d := getDeployment(ctx, ns, "api")
		Expect(requestOf(d, corev1.ResourceCPU)).To(Equal("2"))
		Expect(requestOf(d, corev1.ResourceMemory)).To(Equal("3Gi"))
	})

	It("skips excluded and opted-out workloads entirely", func() {
		ns := newTestNamespace(ctx, "rs-excl")
		makeDeployment(ctx, ns, "api", 2, "2", "4Gi", nil)
		makeDeployment(ctx, ns, "batch", 2, "2", "4Gi", map[string]string{AnnotationRightsizing: "disabled"})
		makeDeployment(ctx, ns, "web", 2, "2", "4Gi", nil)
		src := &fakeRightsizing{byNS: map[string]*kuberov1.ListRightsizingResponse{
			ns: {Recommendations: []*kuberov1.RightsizingRecommendation{
				rsRec(ns, "api", 1.2, 3<<30), rsRec(ns, "batch", 1.2, 3<<30), rsRec(ns, "web", 1.2, 3<<30),
			}},
		}}
		rp := makePolicy(ctx, ns, "apply", func(p *kubeherov1.RightsizingPolicy) {
			p.Spec.Exclude = []string{"api"}
			p.Spec.HumanArm = ptr.To(false)
		})
		got := reconcilePolicy(ctx, newRightsizer(src, &safeEmitter{}, nil), rp)
		Expect(got.Status.WorkloadsInScope).To(Equal(int32(1)))
		pc := onlyChange(got)
		Expect(pc.Workload).To(Equal("web"))
		Expect(pc.Outcome).To(Equal(kubeherov1.ChangeOutcomeApplied))
		Expect(requestOf(getDeployment(ctx, ns, "api"), corev1.ResourceCPU)).To(Equal("2"))
		Expect(requestOf(getDeployment(ctx, ns, "batch"), corev1.ResourceCPU)).To(Equal("2"))
	})

	It("reports control-plane failures without touching workloads", func() {
		ns := newTestNamespace(ctx, "rs-cperr")
		makeDeployment(ctx, ns, "api", 2, "2", "4Gi", nil)
		src := &fakeRightsizing{err: errors.New("connection refused")}
		rp := makePolicy(ctx, ns, "apply", func(p *kubeherov1.RightsizingPolicy) { p.Spec.HumanArm = ptr.To(false) })
		got := reconcilePolicy(ctx, newRightsizer(src, nil, nil), rp)
		data := meta.FindStatusCondition(got.Status.Conditions, ConditionDataAvailable)
		Expect(data.Status).To(Equal(metav1.ConditionFalse))
		Expect(data.Reason).To(Equal(ReasonControlPlaneError))
		Expect(got.Status.PlannedChanges).To(BeEmpty())
	})

	It("does nothing for a cluster outside the clusterSelector", func() {
		ns := newTestNamespace(ctx, "rs-cluster")
		makeDeployment(ctx, ns, "api", 2, "2", "4Gi", nil)
		src := &fakeRightsizing{}
		rp := makePolicy(ctx, ns, "recommend", func(p *kubeherov1.RightsizingPolicy) {
			p.Spec.Scope.ClusterSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"env": "prod"}}
		})
		got := reconcilePolicy(ctx, newRightsizer(src, nil, nil), rp)
		Expect(meta.FindStatusCondition(got.Status.Conditions, ConditionReady).Reason).To(Equal(ReasonOutOfScope))
		Expect(src.lastCall()).To(BeNil())
	})
})
