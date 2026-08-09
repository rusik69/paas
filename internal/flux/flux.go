// Package flux installs the Flux controllers this platform delivers releases
// with.
//
// Only source-controller and helm-controller. Not kustomize-controller, not
// notification-controller, not image automation: each is more attack surface
// and more CRDs for a capability nothing in the roadmap uses yet.
package flux

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"slices"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/rusik69/paas/internal/kube"
)

// FieldManager owns every field this package applies.
//
// API, like every field-manager string: changing it orphans ownership of the
// Flux install on every cluster already running.
const FieldManager = "paas-operator/flux"

// Namespace is where the controllers and their objects live.
const Namespace = "flux-system"

// ErrNoManifests means the vendored manifests are missing or empty.
var ErrNoManifests = errors.New("vendored flux manifests are empty: run 'make vendor-flux'")

// Regenerate with `make vendor-flux`, which runs the pinned flux CLI.
//
//go:embed manifests/flux.yaml
var manifests []byte

// ShardSelector confines these controllers to the default shard: they watch
// only objects that carry no shard label.
//
// Set now, with one shard, so adding a second later is a value change on a new
// deployment rather than a redeploy of everything — retrofitting it means every
// existing object is briefly watched by two controllers or by none.
const ShardSelector = "!sharding.fluxcd.io/key"

const shardFlag = "--watch-label-selector=" + ShardSelector

// Parsed once. The bytes are fixed at compile time, so a failure here means the
// binary itself is malformed — a programmer error, not a runtime one, and the
// one case go-guidelines allows a panic outside main.
var objects = func() []*unstructured.Unstructured {
	objs, err := load(manifests)
	utilruntime.Must(err)
	return objs
}()

// Split out so the failure paths are reachable from a test: an error branch no
// test can reach is one that stays untested and then wrong.
func load(b []byte) ([]*unstructured.Unstructured, error) {
	objs, err := kube.Decode(b)
	if err != nil {
		return nil, fmt.Errorf("parse vendored flux manifests: %w", err)
	}
	if len(objs) == 0 {
		return nil, ErrNoManifests
	}
	if err := applyShardSelector(objs); err != nil {
		return nil, err
	}
	return objs, nil
}

// applyShardSelector adds the shard flag to every controller Deployment.
//
// Applied here rather than patched into the vendored file, so `make vendor-flux`
// stays a plain re-export of the pinned CLI and cannot silently drop it.
func applyShardSelector(objs []*unstructured.Unstructured) error {
	patched := 0
	for _, o := range objs {
		if o.GetKind() != "Deployment" {
			continue
		}
		containers, err := containersOf(o)
		if err != nil {
			return err
		}
		for i, c := range containers {
			m, ok := c.(map[string]any)
			if !ok {
				return fmt.Errorf("deployment %s container %d is malformed", o.GetName(), i)
			}
			args, ok := m["args"].([]any)
			if m["args"] != nil && !ok {
				return fmt.Errorf("deployment %s container %d args are not a list", o.GetName(), i)
			}
			for _, a := range args {
				if _, ok := a.(string); !ok {
					return fmt.Errorf("deployment %s container %d has a non-string arg %v",
						o.GetName(), i, a)
				}
			}
			// Mutated in place rather than through SetNested*, which cannot fail
			// on a path already walked and would leave two branches no test can
			// reach.
			if !slices.Contains(args, any(shardFlag)) {
				m["args"] = append(args, shardFlag)
			}
		}
		patched++
	}
	if patched == 0 {
		return errors.New("vendored flux manifests contain no Deployment to shard")
	}
	return nil
}

// containersOf walks to the pod spec's containers without copying, so edits
// land on the object itself.
func containersOf(o *unstructured.Unstructured) ([]any, error) {
	spec, ok := o.Object["spec"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("deployment %s has no containers: no spec", o.GetName())
	}
	template, ok := spec["template"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("deployment %s has no containers: no template", o.GetName())
	}
	podSpec, ok := template["spec"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("deployment %s has no containers: no template spec", o.GetName())
	}
	containers, ok := podSpec["containers"].([]any)
	if !ok {
		return nil, fmt.Errorf("deployment %s has no containers", o.GetName())
	}
	return containers, nil
}

// Bootstrap installs the Flux controllers.
//
// Level-triggered: run against an up-to-date cluster it changes nothing.
func Bootstrap(ctx context.Context, c client.Client) error {
	return kube.ApplyAll(ctx, c, objects, FieldManager)
}
