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

// entry serialises Start and Stop for one gvk. Build and informer removal can
// both block — on cache sync, on informer goroutines winding down — and doing
// either while holding a lock shared across all kinds would queue every other
// kind's Start, Stop and Running behind it.
type entry struct {
	mu     sync.Mutex
	cancel context.CancelFunc
}

// Engine owns the running controllers for generated kinds.
type Engine struct {
	Manager ctrl.Manager
	Build   Builder

	mu      sync.Mutex
	entries map[schema.GroupVersionKind]*entry

	// removeInformer is swappable so the lifecycle can be tested without a
	// live cache.
	removeInformer func(context.Context, schema.GroupVersionKind) error
}

// entryFor returns the entry for gvk, creating it if this is the first time
// gvk has been seen. Held only long enough to find or create the pointer —
// never across Build or informer removal.
func (e *Engine) entryFor(gvk schema.GroupVersionKind) *entry {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.entries == nil {
		e.entries = map[schema.GroupVersionKind]*entry{}
	}
	en, ok := e.entries[gvk]
	if !ok {
		en = &entry{}
		e.entries[gvk] = en
	}
	return en
}

// Start runs a controller for gvk. Calling it for a kind already running is a
// no-op, because a ServiceClass reconciles every time anything about it changes.
func (e *Engine) Start(ctx context.Context, gvk schema.GroupVersionKind) error {
	en := e.entryFor(gvk)
	en.mu.Lock()
	defer en.mu.Unlock()

	if en.cancel != nil {
		return nil
	}

	// Deliberately not derived from the reconcile request's context: that one
	// is cancelled when the reconcile returns, and this controller outlives it.
	cctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	if err := e.Build(cctx, gvk); err != nil {
		cancel()
		return fmt.Errorf("start controller for %s: %w", gvk, err)
	}
	en.cancel = cancel
	return nil
}

// Stop cancels the controller for gvk and drops its informer, in that order.
// Stopping a kind that was never started, or already stopped, is not an
// error: reconcile is level-triggered and will ask more than once.
//
// This holds gvk's own entry lock for the whole call, through the informer
// removal, so a concurrent Start for the same gvk cannot build a fresh
// controller until the old one's informer is actually gone — otherwise the
// new controller could start depending on an informer this call is removing
// out from under it. Other kinds are unaffected: each has its own entry.
func (e *Engine) Stop(ctx context.Context, gvk schema.GroupVersionKind) error {
	en := e.entryFor(gvk)
	en.mu.Lock()
	defer en.mu.Unlock()

	if en.cancel == nil {
		return nil
	}
	en.cancel()
	en.cancel = nil

	remove := e.removeInformer
	if remove == nil {
		remove = e.removeFromCache
	}
	if err := remove(ctx, gvk); err != nil {
		return fmt.Errorf("remove informer for %s: %w", gvk, err)
	}
	return nil
}

// Running reports whether a controller for gvk is live. It never blocks
// behind Start or Stop for a different gvk.
func (e *Engine) Running(gvk schema.GroupVersionKind) bool {
	e.mu.Lock()
	en, ok := e.entries[gvk]
	e.mu.Unlock()
	if !ok {
		return false
	}

	en.mu.Lock()
	defer en.mu.Unlock()
	return en.cancel != nil
}

func (e *Engine) removeFromCache(ctx context.Context, gvk schema.GroupVersionKind) error {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	return e.Manager.GetCache().RemoveInformer(ctx, u)
}
