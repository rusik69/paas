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
//
// Starting a controller launches work — a goroutine driving its run loop —
// that outlives Build's own return, so done must be called exactly once when
// that work stops, for any reason: an engine-initiated Stop, but also one the
// engine did not ask for, such as a cache-sync timeout. Without that signal
// the engine has no way to learn the kind died on its own, and a later Start
// would find it permanently marked running.
//
// done must be called from that other goroutine and never before Build
// returns. Start holds the kind's entry lock across Build, and done takes it,
// so a Builder that reported a stillborn controller synchronously would
// deadlock against its own caller.
type Builder func(ctx context.Context, gvk schema.GroupVersionKind, done func()) error

// entry serialises Start and Stop for one gvk. Build and informer removal can
// both block — on cache sync, on informer goroutines winding down — and doing
// either while holding a lock shared across all kinds would queue every other
// kind's Start, Stop and Running behind it.
type entry struct {
	mu sync.Mutex
	// generation counts the controllers this entry has held, so a done
	// callback from one of them can tell whether it is still that one.
	generation uint64
	cancel     context.CancelFunc
	// informer records that a controller was built for this gvk and its
	// informer is therefore still in the shared cache. It outlives cancel: a
	// controller that stopped on its own leaves the informer behind, and a
	// later Stop is the only thing that removes it.
	informer bool
}

// clear releases the controller for generation if it is still the current
// one for this gvk. done callbacks arrive asynchronously and can land after
// a Stop and a later Start have already replaced this entry's cancel — a
// class update does exactly that, since Stop's RemoveInformer returns far
// sooner than a draining controller's Start. Without the generation check,
// a stale done would call en.cancel() on the new controller and cancel it
// instead of doing nothing, leaving the kind dead until some unrelated later
// event happened to restart it.
//
// It is called from the done callback Start hands to Build, so it must not be
// called by anything already holding en.mu — Stop calls its own inline
// equivalent for exactly that reason. It reports whether it cleared, so the
// caller can notify outside the lock.
func (en *entry) clear(generation uint64) bool {
	en.mu.Lock()
	defer en.mu.Unlock()
	if en.generation != generation {
		return false
	}
	if en.cancel != nil {
		en.cancel()
		en.cancel = nil
	}
	return true
}

// Engine owns the running controllers for generated kinds.
type Engine struct {
	Manager ctrl.Manager
	Build   Builder

	// Stopped, if set, is called after a controller's run loop returns and its
	// slot has been freed — including when nothing asked it to stop, which is
	// the case that matters: freeing the slot only makes a later Start
	// possible, and nothing else would issue one until the next ServiceClass
	// event or the informer's ten-hour resync, leaving the kind unserved in
	// between. The wiring supplies something that puts the owning ServiceClass
	// back through reconcile; the engine cannot, without importing the package
	// that drives it.
	//
	// It runs on the goroutine that reported the stop, holding no engine lock,
	// and must return rather than block indefinitely.
	Stopped func(ctx context.Context, gvk schema.GroupVersionKind)

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

	en.generation++
	generation := en.generation

	// Deliberately not derived from the reconcile request's context: that one
	// is cancelled when the reconcile returns, and this controller outlives it.
	// base rather than cctx for the report: cctx is already cancelled by the
	// time done runs on every path but a self-death, so waking anything with it
	// would be a no-op exactly when the wake is wanted.
	base := context.WithoutCancel(ctx)
	cctx, cancel := context.WithCancel(base)
	done := func() {
		if en.clear(generation) && e.Stopped != nil {
			e.Stopped(base, gvk)
		}
	}
	if err := e.Build(cctx, gvk, done); err != nil {
		cancel()
		return fmt.Errorf("start controller for %s: %w", gvk, err)
	}
	en.cancel = cancel
	en.informer = true
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
//
// The informer is removed even when the controller is already gone. A kind
// that died on its own — a cache-sync timeout, say — has nothing left to
// cancel but still has an informer in the shared cache, watching a resource
// nothing serves.
func (e *Engine) Stop(ctx context.Context, gvk schema.GroupVersionKind) error {
	en := e.entryFor(gvk)
	en.mu.Lock()
	defer en.mu.Unlock()

	if en.cancel != nil {
		en.cancel()
		en.cancel = nil
	}
	if !en.informer {
		return nil
	}

	remove := e.removeInformer
	if remove == nil {
		remove = e.removeFromCache
	}
	if err := remove(ctx, gvk); err != nil {
		return fmt.Errorf("remove informer for %s: %w", gvk, err)
	}
	en.informer = false
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
