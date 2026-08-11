// Package engine runs one controller per kind that did not exist when the
// manager started.
//
// controller-runtime builds its controllers before the manager starts, and has
// no way to remove one. Generated kinds appear and disappear with the catalog,
// so their controllers need a lifecycle of their own.
package engine

import (
	"context"
	"fmt"
	"sync"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
)

// Builder starts a controller for one kind. It must return once the controller
// is running, and the controller must stop when ctx is cancelled.
type Builder func(ctx context.Context, gvk schema.GroupVersionKind) error

// Engine owns the running controllers for generated kinds.
type Engine struct {
	Manager ctrl.Manager
	Build   Builder

	mu      sync.Mutex
	running map[schema.GroupVersionKind]context.CancelFunc

	// removeInformer is swappable so the lifecycle can be tested without a
	// live cache.
	removeInformer func(context.Context, schema.GroupVersionKind) error
}

// Start runs a controller for gvk. Calling it for a kind already running is a
// no-op, because a ServiceClass reconciles every time anything about it changes.
func (e *Engine) Start(ctx context.Context, gvk schema.GroupVersionKind) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.running == nil {
		e.running = map[schema.GroupVersionKind]context.CancelFunc{}
	}
	if _, ok := e.running[gvk]; ok {
		return nil
	}

	// Deliberately not derived from the reconcile request's context: that one
	// is cancelled when the reconcile returns, and this controller outlives it.
	cctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	if err := e.Build(cctx, gvk); err != nil {
		cancel()
		return fmt.Errorf("start controller for %s: %w", gvk, err)
	}
	e.running[gvk] = cancel
	return nil
}

// Stop cancels the controller for gvk and drops its informer, in that order.
// Stopping a kind that was never started, or already stopped, is not an
// error: reconcile is level-triggered and will ask more than once.
//
// The mutex stays held for the whole call, through the informer removal. A
// concurrent Start for the same gvk must not build a fresh controller until
// the old one's informer is actually gone — otherwise the new controller can
// start depending on an informer this call is about to remove out from under
// it.
func (e *Engine) Stop(ctx context.Context, gvk schema.GroupVersionKind) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	cancel, ok := e.running[gvk]
	if !ok {
		return nil
	}
	delete(e.running, gvk)
	cancel()

	remove := e.removeInformer
	if remove == nil {
		remove = e.removeFromCache
	}
	if err := remove(ctx, gvk); err != nil {
		return fmt.Errorf("remove informer for %s: %w", gvk, err)
	}
	return nil
}

// Running reports whether a controller for gvk is live.
func (e *Engine) Running(gvk schema.GroupVersionKind) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, ok := e.running[gvk]
	return ok
}

func (e *Engine) removeFromCache(ctx context.Context, gvk schema.GroupVersionKind) error {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	return e.Manager.GetCache().RemoveInformer(ctx, u)
}
