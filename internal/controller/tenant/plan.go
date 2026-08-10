package tenant

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	corev1alpha1 "github.com/rusik69/paas/api/core/v1alpha1"
)

// Limits are the quota and per-container defaults a plan grants.
type Limits struct {
	CPU    string
	Memory string
	Pods   string
	// DefaultCPU and DefaultMemory are what a container gets when it asks for
	// nothing. Required, not optional: a namespace with a ResourceQuota on cpu
	// rejects any pod without a cpu request, so a LimitRange default is what
	// keeps ordinary manifests working inside a quota.
	DefaultCPU    string
	DefaultMemory string
}

// plans is a table rather than a CRD. Three plans exist and inventing a Plan
// kind before anyone has asked for a fourth is the speculative abstraction the
// guidelines forbid; it becomes a CRD when a customer needs a bespoke one.
var plans = map[corev1alpha1.Plan]Limits{
	corev1alpha1.PlanTrial: {
		CPU: "2", Memory: "4Gi", Pods: "20",
		DefaultCPU: "100m", DefaultMemory: "128Mi",
	},
	corev1alpha1.PlanBusiness: {
		CPU: "8", Memory: "16Gi", Pods: "100",
		DefaultCPU: "100m", DefaultMemory: "256Mi",
	},
	corev1alpha1.PlanEnterprise: {
		CPU: "32", Memory: "64Gi", Pods: "500",
		DefaultCPU: "200m", DefaultMemory: "512Mi",
	},
}

// LimitsFor returns the limits a plan grants, and false for an unknown plan.
//
// The CRD's enum makes an unknown plan unreachable through the API, but a
// reconciler that indexed a map blindly would apply a zero quota — which
// forbids every pod — if the enum and this table ever disagreed.
func LimitsFor(plan corev1alpha1.Plan) (Limits, bool) {
	l, ok := plans[plan]
	return l, ok
}

func (l Limits) quota() corev1.ResourceList {
	return corev1.ResourceList{
		corev1.ResourceRequestsCPU:    resource.MustParse(l.CPU),
		corev1.ResourceRequestsMemory: resource.MustParse(l.Memory),
		corev1.ResourceLimitsCPU:      resource.MustParse(l.CPU),
		corev1.ResourceLimitsMemory:   resource.MustParse(l.Memory),
		corev1.ResourcePods:           resource.MustParse(l.Pods),
	}
}

func (l Limits) defaults() corev1.ResourceList {
	return corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse(l.DefaultCPU),
		corev1.ResourceMemory: resource.MustParse(l.DefaultMemory),
	}
}
