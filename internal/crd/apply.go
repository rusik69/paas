package crd

import (
	"context"
	"fmt"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/rusik69/paas/pkg/wait"
)

// One group registered once into a fresh scheme, which cannot fail. Building it
// here makes that structural rather than an error branch no test can reach.
var installScheme = func() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(apiextensionsv1.AddToScheme(s))
	return s
}()

// Apply installs every embedded CRD and waits for each to become Established,
// returning how many were installed.
//
// It is level-triggered: run against an up-to-date cluster it changes nothing.
// Ownership is forced, because the operator owns these objects completely and a
// conflict it cannot resolve would wedge it for good.
func Apply(ctx context.Context, c client.Client) (int, error) {
	crds, err := Load()
	if err != nil {
		return 0, err
	}
	if err := applyAll(ctx, c, crds); err != nil {
		return 0, err
	}
	return len(crds), nil
}

func applyAll(ctx context.Context, c client.Client, crds []*apiextensionsv1.CustomResourceDefinition) error {
	for _, crd := range crds {
		obj := crd.DeepCopy()
		obj.SetGroupVersionKind(apiextensionsv1.SchemeGroupVersion.WithKind("CustomResourceDefinition"))
		if err := c.Patch(ctx, obj, client.Apply,
			client.FieldOwner(FieldManager), client.ForceOwnership); err != nil {
			return fmt.Errorf("apply crd %s: %w", crd.Name, err)
		}
	}

	// Separately, and after every apply: waiting inline would serialise a slow
	// establishment behind each other CRD's apply for no reason.
	for _, crd := range crds {
		if err := waitEstablished(ctx, c, crd.Name); err != nil {
			return err
		}
	}
	return nil
}

// Established reports whether the API server has accepted a CRD.
//
// The error is terminal rather than "not yet": names already taken by another
// CRD are never granted, so a caller that kept waiting would report a timeout
// instead of the conflict that caused it. It is separate from the wait below
// because a reconciler woken by a watch has the object already and must not
// poll for it — see internal/controller/serviceclass.
func Established(crd *apiextensionsv1.CustomResourceDefinition) (bool, error) {
	for _, cond := range crd.Status.Conditions {
		if cond.Type == apiextensionsv1.NamesAccepted && cond.Status == apiextensionsv1.ConditionFalse {
			return false, fmt.Errorf("crd %s names rejected: %s", crd.Name, cond.Message)
		}
		if cond.Type == apiextensionsv1.Established && cond.Status == apiextensionsv1.ConditionTrue {
			return true, nil
		}
	}
	return false, nil
}

// waitEstablished polls rather than watches: this runs once at startup, before
// any manager or cache exists, and a watch would cost both for a wait measured
// in seconds.
func waitEstablished(ctx context.Context, c client.Client, name string) error {
	return wait.For(ctx, time.Second, "crd "+name+" Established", func(ctx context.Context) (bool, error) {
		got := &apiextensionsv1.CustomResourceDefinition{}
		if err := c.Get(ctx, types.NamespacedName{Name: name}, got); err != nil {
			return false, nil
		}
		return Established(got)
	})
}

// Install builds a client from cfg and applies every embedded CRD, returning
// how many were installed.
//
// It exists so cmd/paas-operator stays flag wiring and nothing else, which is
// what keeps the logic here reachable from a test.
func Install(ctx context.Context, cfg *rest.Config, timeout time.Duration) (int, error) {
	c, err := client.New(cfg, client.Options{Scheme: installScheme})
	if err != nil {
		return 0, fmt.Errorf("build client: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	n, err := Apply(ctx, c)
	if err != nil {
		return 0, fmt.Errorf("install CRDs: %w", err)
	}
	return n, nil
}
