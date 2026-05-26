package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kubeherov1 "github.com/kubehero-io/platform/services/operator/api/v1"
)

var _ = Describe("CeilingPolicy Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default", // TODO(user):Modify as needed
		}
		ceilingpolicy := &kubeherov1.CeilingPolicy{}

		BeforeEach(func() {
			By("creating the BudgetPolicy that this CeilingPolicy references")
			budget := &kubeherov1.BudgetPolicy{}
			budgetKey := types.NamespacedName{Name: "test-budget", Namespace: "default"}
			if err := k8sClient.Get(ctx, budgetKey, budget); err != nil && errors.IsNotFound(err) {
				Expect(k8sClient.Create(ctx, &kubeherov1.BudgetPolicy{
					ObjectMeta: metav1.ObjectMeta{Name: "test-budget", Namespace: "default"},
					Spec: kubeherov1.BudgetPolicySpec{
						Scope:   kubeherov1.Scope{},
						Ceiling: "$10000/mo",
					},
				})).To(Succeed())
			}

			By("creating the custom resource for the Kind CeilingPolicy")
			err := k8sClient.Get(ctx, typeNamespacedName, ceilingpolicy)
			if err != nil && errors.IsNotFound(err) {
				resource := &kubeherov1.CeilingPolicy{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: kubeherov1.CeilingPolicySpec{
						BudgetRef: "test-budget",
						Trigger: kubeherov1.Trigger{
							BurnRateMilli: 1500,
							Window:        "5m",
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			resource := &kubeherov1.CeilingPolicy{}
			if err := k8sClient.Get(ctx, typeNamespacedName, resource); err == nil {
				By("Cleanup the specific resource instance CeilingPolicy")
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			}
			budget := &kubeherov1.BudgetPolicy{}
			budgetKey := types.NamespacedName{Name: "test-budget", Namespace: "default"}
			if err := k8sClient.Get(ctx, budgetKey, budget); err == nil {
				Expect(k8sClient.Delete(ctx, budget)).To(Succeed())
			}
		})
		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			controllerReconciler := &CeilingPolicyReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("checking the Tripped condition is False under the stub provider")
			updated := &kubeherov1.CeilingPolicy{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
			cond := findCondition(updated.Status.Conditions, ConditionTripped)
			Expect(cond).NotTo(BeNil())
			Expect(string(cond.Status)).To(Equal("False"))
		})

		It("should set Tripped=True when burn rate exceeds the trigger", func() {
			emitter := &recordingEmitter{}
			controllerReconciler := &CeilingPolicyReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				BurnRate: FixedBurnRate{Value: 2000}, // trigger is 1500
				Audit:    emitter,
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			updated := &kubeherov1.CeilingPolicy{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
			cond := findCondition(updated.Status.Conditions, ConditionTripped)
			Expect(cond).NotTo(BeNil())
			Expect(string(cond.Status)).To(Equal("True"))
			Expect(cond.Reason).To(Equal(ReasonBurnRateExceeded))

			By("emitting one audit event on the False→True transition")
			Expect(emitter.events).To(HaveLen(1))
			Expect(emitter.events[0].Action).To(Equal("ceiling.tripped"))
			Expect(emitter.events[0].TargetName).To(Equal(resourceName))

			By("not re-emitting on the next reconcile when condition stays True")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(emitter.events).To(HaveLen(1))
		})
	})
})

type recordingEmitter struct {
	events []AuditEvent
}

func (r *recordingEmitter) Emit(_ context.Context, e AuditEvent) error {
	r.events = append(r.events, e)
	return nil
}

func findCondition(conds []metav1.Condition, t string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == t {
			return &conds[i]
		}
	}
	return nil
}
