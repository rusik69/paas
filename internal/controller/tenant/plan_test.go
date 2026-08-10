package tenant

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	corev1alpha1 "github.com/rusik69/paas/api/core/v1alpha1"
)

// Every plan the CRD accepts must have limits, or a tenant on that plan gets a
// zero quota, which forbids every pod.
func TestLimitsFor_CoversEveryPlanTheAPIAccepts(t *testing.T) {
	t.Parallel()

	for _, plan := range []corev1alpha1.Plan{
		corev1alpha1.PlanTrial, corev1alpha1.PlanBusiness, corev1alpha1.PlanEnterprise,
	} {
		limits, ok := LimitsFor(plan)
		if !ok {
			t.Errorf("plan %q has no limits", plan)
			continue
		}
		if limits.CPU == "" || limits.Memory == "" || limits.Pods == "" {
			t.Errorf("plan %q has an incomplete quota: %+v", plan, limits)
		}
		// A ResourceQuota on cpu rejects pods that request none, so a plan
		// without container defaults would break ordinary manifests.
		if limits.DefaultCPU == "" || limits.DefaultMemory == "" {
			t.Errorf("plan %q has no container defaults: %+v", plan, limits)
		}
	}
}

func TestLimitsFor_UnknownPlan(t *testing.T) {
	t.Parallel()

	if _, ok := LimitsFor(corev1alpha1.Plan("gold")); ok {
		t.Error("an unknown plan returned limits")
	}
}

// The quota bounds requests and limits alike. Bounding only requests lets a
// tenant burst past its plan on limits, which is what actually consumes a node.
func TestLimits_QuotaBoundsRequestsAndLimits(t *testing.T) {
	t.Parallel()

	limits, ok := LimitsFor(corev1alpha1.PlanBusiness)
	if !ok {
		t.Fatal("business plan has no limits")
	}
	quota := limits.quota()

	for _, key := range []corev1.ResourceName{
		corev1.ResourceRequestsCPU, corev1.ResourceRequestsMemory,
		corev1.ResourceLimitsCPU, corev1.ResourceLimitsMemory,
		corev1.ResourcePods,
	} {
		if _, ok := quota[key]; !ok {
			t.Errorf("quota does not bound %s", key)
		}
	}
}

func TestLimits_DefaultsAreParseable(t *testing.T) {
	t.Parallel()

	for _, plan := range []corev1alpha1.Plan{
		corev1alpha1.PlanTrial, corev1alpha1.PlanBusiness, corev1alpha1.PlanEnterprise,
	} {
		limits, _ := LimitsFor(plan)
		defaults := limits.defaults()
		if len(defaults) != 2 {
			t.Errorf("plan %q defaults = %v, want cpu and memory", plan, defaults)
		}
	}
}
