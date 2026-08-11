package engine

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache/informertest"
)

var testGVK = schema.GroupVersionKind{Group: "apps.paas.io", Version: "v1alpha1", Kind: "Postgres"}

// fakeManager satisfies ctrl.Manager by embedding a nil one and overriding
// only the method removeFromCache calls; anything else would panic, which is
// the point — a test that reaches further than GetCache is testing the wrong
// thing.
type fakeManager struct {
	ctrl.Manager
	cache cache.Cache
}

func (f *fakeManager) GetCache() cache.Cache { return f.cache }

func TestEngine_StartIsIdempotent(t *testing.T) {
	var mu sync.Mutex
	var built int
	e := &Engine{Build: func(_ context.Context, _ schema.GroupVersionKind) error {
		mu.Lock()
		defer mu.Unlock()
		built++
		return nil
	}}

	for range 3 {
		if err := e.Start(t.Context(), testGVK); err != nil {
			t.Fatalf("Start: %v", err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if built != 1 {
		t.Errorf("built %d controllers, want 1 — a ServiceClass reconciles repeatedly and must not accumulate them", built)
	}
	if !e.Running(testGVK) {
		t.Error("Running reports false after Start")
	}
}

func TestEngine_StopCancelsBeforeRemoving(t *testing.T) {
	var order []string
	var mu sync.Mutex
	done := make(chan struct{})

	e := &Engine{
		Build: func(ctx context.Context, _ schema.GroupVersionKind) error {
			go func() {
				<-ctx.Done()
				mu.Lock()
				order = append(order, "cancelled")
				mu.Unlock()
				close(done)
			}()
			return nil
		},
		removeInformer: func(_ context.Context, _ schema.GroupVersionKind) error {
			<-done
			mu.Lock()
			order = append(order, "removed")
			mu.Unlock()
			return nil
		},
	}

	if err := e.Start(t.Context(), testGVK); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := e.Stop(t.Context(), testGVK); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[0] != "cancelled" || order[1] != "removed" {
		t.Errorf("order = %v, want [cancelled removed] — removing an informer under a live controller delivers to a controller that has gone", order)
	}
	if e.Running(testGVK) {
		t.Error("Running reports true after Stop")
	}
}

func TestEngine_StopUnknownIsNotAnError(t *testing.T) {
	e := &Engine{Build: func(context.Context, schema.GroupVersionKind) error { return nil }}
	if err := e.Stop(t.Context(), testGVK); err != nil {
		t.Errorf("Stop on an unstarted kind returned %v; reconcile is level-triggered and will ask more than once", err)
	}
}

func TestEngine_StartBuildFailureDoesNotLeaveRunning(t *testing.T) {
	wantErr := errors.New("boom")
	e := &Engine{Build: func(context.Context, schema.GroupVersionKind) error {
		return wantErr
	}}

	err := e.Start(t.Context(), testGVK)
	if err == nil {
		t.Fatal("Start: want error, got nil")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("Start error = %v, want wrapping %v", err, wantErr)
	}
	if e.Running(testGVK) {
		t.Error("Running reports true after a failed Start")
	}
}

func TestEngine_StopRemoveInformerFailurePropagates(t *testing.T) {
	wantErr := errors.New("remove failed")
	e := &Engine{
		Build: func(context.Context, schema.GroupVersionKind) error { return nil },
		removeInformer: func(context.Context, schema.GroupVersionKind) error {
			return wantErr
		},
	}

	if err := e.Start(t.Context(), testGVK); err != nil {
		t.Fatalf("Start: %v", err)
	}

	err := e.Stop(t.Context(), testGVK)
	if err == nil {
		t.Fatal("Stop: want error, got nil")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("Stop error = %v, want wrapping %v", err, wantErr)
	}
	if e.Running(testGVK) {
		t.Error("Running reports true after Stop, even though removeInformer failed — the controller was already cancelled")
	}
}

// TestEngine_StopBlocksConcurrentStartForSameGVK proves the ordering with a
// shared log rather than by timing a "hasn't returned yet" check: Go's
// sync.Mutex gives Start no way to acquire e.mu and call Build again until
// Stop, still holding it, has finished removing the old informer. If Stop
// released the lock before removing the informer (as an earlier version of
// this code did), a concurrent Start could log "built" before "removed".
func TestEngine_StopBlocksConcurrentStartForSameGVK(t *testing.T) {
	removeStarted := make(chan struct{})
	releaseRemove := make(chan struct{})
	var mu sync.Mutex
	var order []string

	e := &Engine{
		Build: func(context.Context, schema.GroupVersionKind) error {
			mu.Lock()
			order = append(order, "built")
			mu.Unlock()
			return nil
		},
		removeInformer: func(context.Context, schema.GroupVersionKind) error {
			close(removeStarted)
			<-releaseRemove
			mu.Lock()
			order = append(order, "removed")
			mu.Unlock()
			return nil
		},
	}

	if err := e.Start(t.Context(), testGVK); err != nil {
		t.Fatalf("Start: %v", err)
	}

	stopDone := make(chan error, 1)
	go func() { stopDone <- e.Stop(t.Context(), testGVK) }()

	// Wait for Stop to have deleted the map entry and entered removeInformer
	// — still holding e.mu — before racing a Start against it. Otherwise
	// which goroutine runs first is undetermined, and a Start that happens
	// to win would just see the still-registered entry and no-op.
	<-removeStarted

	startDone := make(chan error, 1)
	go func() { startDone <- e.Start(t.Context(), testGVK) }()

	close(releaseRemove)

	if err := <-stopDone; err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := <-startDone; err != nil {
		t.Fatalf("Start: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{"built", "removed", "built"}
	if !slices.Equal(order, want) {
		t.Errorf("order = %v, want %v — the second build must not happen until the old informer is removed", order, want)
	}
	if !e.Running(testGVK) {
		t.Error("Running reports false after the blocked Start completed")
	}
}

func TestEngine_StopUsesManagerCacheByDefault(t *testing.T) {
	e := &Engine{
		Manager: &fakeManager{cache: &informertest.FakeInformers{}},
		Build:   func(context.Context, schema.GroupVersionKind) error { return nil },
	}

	if err := e.Start(t.Context(), testGVK); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := e.Stop(t.Context(), testGVK); err != nil {
		t.Errorf("Stop: %v", err)
	}
}
