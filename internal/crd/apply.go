package crd

import (
	"context"
	"fmt"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/rusik69/paas/pkg/wait"
)

// Apply installs every embedded CRD and waits for each to become Established.
//
// It is level-triggered: run against an up-to-date cluster it changes nothing.
// Ownership is forced, because the operator owns these objects completely and a
// conflict it cannot resolve would wedge it for good.
func Apply(ctx context.Context, c client.Client) error {
	crds, err := Load()
	if err != nil {
		return err
	}

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

// waitEstablished polls rather than watches: this runs once at startup, and a
// watch would cost a cache and an informer for a wait measured in seconds.
func waitEstablished(ctx context.Context, c client.Client, name string) error {
	return wait.For(ctx, time.Second, "crd "+name+" Established", func(ctx context.Context) (bool, error) {
		got := &apiextensionsv1.CustomResourceDefinition{}
		if err := c.Get(ctx, types.NamespacedName{Name: name}, got); err != nil {
			return false, nil
		}
		for _, cond := range got.Status.Conditions {
			// Terminal: names already taken by another CRD are never granted,
			// so waiting out the deadline would report a timeout instead of the
			// conflict that caused it.
			if cond.Type == apiextensionsv1.NamesAccepted && cond.Status == apiextensionsv1.ConditionFalse {
				return false, fmt.Errorf("crd %s names rejected: %s", name, cond.Message)
			}
			if cond.Type == apiextensionsv1.Established && cond.Status == apiextensionsv1.ConditionTrue {
				return true, nil
			}
		}
		return false, nil
	})
}
