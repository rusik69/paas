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
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var testGVK = schema.GroupVersionKind{Group: "apps.paas.io", Version: "v1alpha1", Kind: "Postgres"}

var otherGVK = schema.GroupVersionKind{Group: "apps.paas.io", Version: "v1alpha1", Kind: "MySQL"}

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
	e := &Engine{Build: func(_ context.Context, _ schema.GroupVersionKind, _ func()) error {
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
		Build: func(ctx context.Context, _ schema.GroupVersionKind, _ func()) error {
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

func TestEngine_RunningFalseForAnUnseenGVK(t *testing.T) {
	e := &Engine{Build: func(context.Context, schema.GroupVersionKind, func()) error { return nil }}
	if e.Running(testGVK) {
		t.Error("Running reports true for a gvk that was never started or stopped")
	}
}

func TestEngine_StopUnknownIsNotAnError(t *testing.T) {
	e := &Engine{Build: func(context.Context, schema.GroupVersionKind, func()) error { return nil }}
	if err := e.Stop(t.Context(), testGVK); err != nil {
		t.Errorf("Stop on an unstarted kind returned %v; reconcile is level-triggered and will ask more than once", err)
	}
}

// TestEngine_DoneReleasesAKindThatStoppedOnItsOwn is the regression guard for
// the window a Builder that starts its controller in a goroutine opens: a
// cache-sync timeout, say, stops the controller without anyone calling Stop.
// Without done clearing the entry, Start would find en.cancel still set and
// no-op forever, leaving the kind dead with nothing to recover it.
func TestEngine_DoneReleasesAKindThatStoppedOnItsOwn(t *testing.T) {
	var mu sync.Mutex
	var built int
	var done func()

	e := &Engine{Build: func(_ context.Context, _ schema.GroupVersionKind, d func()) error {
		mu.Lock()
		built++
		done = d
		mu.Unlock()
		return nil
	}}

	if err := e.Start(t.Context(), testGVK); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !e.Running(testGVK) {
		t.Fatal("Running reports false right after Start")
	}

	// The controller stops on its own — nobody called Stop.
	mu.Lock()
	done()
	mu.Unlock()

	if e.Running(testGVK) {
		t.Error("Running reports true after the controller's own done fired")
	}

	if err := e.Start(t.Context(), testGVK); err != nil {
		t.Fatalf("Start after done: %v", err)
	}
	mu.Lock()
	got := built
	mu.Unlock()
	if got != 2 {
		t.Errorf("built %d controllers, want 2 — a kind that died on its own must be startable again", got)
	}
	if !e.Running(testGVK) {
		t.Error("Running reports false after the second Start")
	}
}

// TestEngine_StaleDoneDoesNotCancelASuccessor is the regression guard for a
// class update: Stop returns as soon as RemoveInformer does, well before the
// old controller's draining c.Start actually calls done. If a Start for the
// same gvk lands in that window, the old done must not touch the new
// controller when it finally arrives — the generation check is what makes
// that true rather than merely usually true.
func TestEngine_StaleDoneDoesNotCancelASuccessor(t *testing.T) {
	var mu sync.Mutex
	var dones []func()
	var ctxs []context.Context

	e := &Engine{
		Build: func(ctx context.Context, _ schema.GroupVersionKind, done func()) error {
			mu.Lock()
			dones = append(dones, done)
			ctxs = append(ctxs, ctx)
			mu.Unlock()
			return nil
		},
		removeInformer: func(context.Context, schema.GroupVersionKind) error { return nil },
	}

	if err := e.Start(t.Context(), testGVK); err != nil {
		t.Fatalf("Start (1st): %v", err)
	}
	if err := e.Stop(t.Context(), testGVK); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := e.Start(t.Context(), testGVK); err != nil {
		t.Fatalf("Start (2nd): %v", err)
	}

	mu.Lock()
	staleDone := dones[0]
	secondCtx := ctxs[1]
	mu.Unlock()

	// The first controller's done arrives late — after Stop already nil'd its
	// cancel and a second Start replaced it with a new one.
	staleDone()

	if !e.Running(testGVK) {
		t.Error("Running reports false — a stale done cleared the current controller instead of doing nothing")
	}
	select {
	case <-secondCtx.Done():
		t.Error("the second controller's context was cancelled by the first controller's stale done")
	default:
	}
}

// TestEngine_DoneRacingStopIsHarmless covers the ordering the generation
// check does not need to disambiguate: done and Stop for the very same
// controller, arriving in either order. Both converge on "not running",
// never on a panic or a double-cancel.
func TestEngine_DoneRacingStopIsHarmless(t *testing.T) {
	var mu sync.Mutex
	var done func()

	e := &Engine{
		Build: func(_ context.Context, _ schema.GroupVersionKind, d func()) error {
			mu.Lock()
			done = d
			mu.Unlock()
			return nil
		},
		removeInformer: func(context.Context, schema.GroupVersionKind) error { return nil },
	}

	if err := e.Start(t.Context(), testGVK); err != nil {
		t.Fatalf("Start: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		mu.Lock()
		d := done
		mu.Unlock()
		d()
	}()
	go func() {
		defer wg.Done()
		_ = e.Stop(t.Context(), testGVK)
	}()
	wg.Wait()

	if e.Running(testGVK) {
		t.Error("Running reports true after both done and Stop ran")
	}
}

func TestEngine_StartBuildFailureDoesNotLeaveRunning(t *testing.T) {
	wantErr := errors.New("boom")
	e := &Engine{Build: func(context.Context, schema.GroupVersionKind, func()) error {
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
		Build: func(context.Context, schema.GroupVersionKind, func()) error { return nil },
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
		Build: func(context.Context, schema.GroupVersionKind, func()) error {
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

// TestEngine_StopForOneGVKDoesNotBlockStartForAnother proves cross-GVK
// concurrency with a hang, not a timing window: if Start(otherGVK) queued
// behind Stop(testGVK)'s in-flight removeInformer, this call would block
// forever, since releaseRemove is not closed until after it returns.
func TestEngine_StopForOneGVKDoesNotBlockStartForAnother(t *testing.T) {
	removeStarted := make(chan struct{})
	releaseRemove := make(chan struct{})

	e := &Engine{
		Build: func(context.Context, schema.GroupVersionKind, func()) error { return nil },
		removeInformer: func(_ context.Context, gvk schema.GroupVersionKind) error {
			if gvk == testGVK {
				close(removeStarted)
				<-releaseRemove
			}
			return nil
		},
	}

	if err := e.Start(t.Context(), testGVK); err != nil {
		t.Fatalf("Start(testGVK): %v", err)
	}

	stopDone := make(chan error, 1)
	go func() { stopDone <- e.Stop(t.Context(), testGVK) }()
	<-removeStarted // Stop(testGVK) is now blocked inside removeInformer.

	if err := e.Start(t.Context(), otherGVK); err != nil {
		t.Fatalf("Start(otherGVK): %v", err)
	}
	if !e.Running(otherGVK) {
		t.Error("Running(otherGVK) reports false right after Start(otherGVK) completed")
	}

	close(releaseRemove)
	if err := <-stopDone; err != nil {
		t.Fatalf("Stop(testGVK): %v", err)
	}
}

// TestEngine_RunningDoesNotBlockBehindAnotherGVKsStop proves the same
// property for Running: it must not queue behind an unrelated kind's
// teardown. Like the test above, a regression here hangs rather than races.
func TestEngine_RunningDoesNotBlockBehindAnotherGVKsStop(t *testing.T) {
	removeStarted := make(chan struct{})
	releaseRemove := make(chan struct{})

	e := &Engine{
		Build: func(context.Context, schema.GroupVersionKind, func()) error { return nil },
		removeInformer: func(_ context.Context, gvk schema.GroupVersionKind) error {
			if gvk == testGVK {
				close(removeStarted)
				<-releaseRemove
			}
			return nil
		},
	}

	if err := e.Start(t.Context(), testGVK); err != nil {
		t.Fatalf("Start(testGVK): %v", err)
	}
	if err := e.Start(t.Context(), otherGVK); err != nil {
		t.Fatalf("Start(otherGVK): %v", err)
	}

	stopDone := make(chan error, 1)
	go func() { stopDone <- e.Stop(t.Context(), testGVK) }()
	<-removeStarted // Stop(testGVK) is now blocked inside removeInformer.

	if !e.Running(otherGVK) {
		t.Error("Running(otherGVK) reports false while Stop(testGVK) is blocked")
	}

	close(releaseRemove)
	if err := <-stopDone; err != nil {
		t.Fatalf("Stop(testGVK): %v", err)
	}
}

// recordingCache is the manager's cache with the one method removeFromCache
// calls recorded. informertest.FakeInformers alone proves nothing here: its
// RemoveInformer returns nil whatever it is handed, so a Stop that removed the
// wrong kind — or nothing at all — would pass.
type recordingCache struct {
	*informertest.FakeInformers
	removed []schema.GroupVersionKind
}

func (c *recordingCache) RemoveInformer(_ context.Context, obj client.Object) error {
	c.removed = append(c.removed, obj.GetObjectKind().GroupVersionKind())
	return nil
}

func TestEngine_StopUsesManagerCacheByDefault(t *testing.T) {
	cache := &recordingCache{FakeInformers: &informertest.FakeInformers{}}
	e := &Engine{
		Manager: &fakeManager{cache: cache},
		Build:   func(context.Context, schema.GroupVersionKind, func()) error { return nil },
	}

	if err := e.Start(t.Context(), testGVK); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := e.Stop(t.Context(), testGVK); err != nil {
		t.Errorf("Stop: %v", err)
	}

	if !slices.Equal(cache.removed, []schema.GroupVersionKind{testGVK}) {
		t.Errorf("removed %v from the manager's cache, want exactly [%v]", cache.removed, testGVK)
	}
}

// A controller that stopped on its own leaves its informer in the shared
// cache: nothing cancelled it, so nothing removed it. Stop returning early on
// the missing cancel — as it once did — left that informer watching a resource
// the class had already dropped.
func TestEngine_StopRemovesTheInformerOfASelfStoppedKind(t *testing.T) {
	cache := &recordingCache{FakeInformers: &informertest.FakeInformers{}}
	var done func()
	e := &Engine{
		Manager: &fakeManager{cache: cache},
		Build: func(_ context.Context, _ schema.GroupVersionKind, d func()) error {
			done = d
			return nil
		},
	}

	if err := e.Start(t.Context(), testGVK); err != nil {
		t.Fatalf("Start: %v", err)
	}
	done() // a cache-sync timeout, say: nobody called Stop
	if err := e.Stop(t.Context(), testGVK); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if !slices.Equal(cache.removed, []schema.GroupVersionKind{testGVK}) {
		t.Errorf("removed %v, want the self-stopped kind's informer gone too", cache.removed)
	}
}

// A kind nothing ever started has no informer to remove, and reaching the
// manager's cache for one would be a nil dereference in exactly the case Stop
// documents as harmless.
func TestEngine_StopNeverStartedRemovesNoInformer(t *testing.T) {
	cache := &recordingCache{FakeInformers: &informertest.FakeInformers{}}
	e := &Engine{
		Manager: &fakeManager{cache: cache},
		Build:   func(context.Context, schema.GroupVersionKind, func()) error { return nil },
	}

	if err := e.Stop(t.Context(), testGVK); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if len(cache.removed) != 0 {
		t.Errorf("removed %v, want nothing for a kind that was never started", cache.removed)
	}
}

// The freed slot has to be acted on by something. The engine cannot start the
// kind again itself — the class it would have to read lives in a package that
// imports this one — so it reports the death, and a kind that stopped without
// anyone asking stays dead if this does not fire.
func TestEngine_StoppedFiresWhenAControllerDiesOnItsOwn(t *testing.T) {
	var done func()
	var got []schema.GroupVersionKind
	var live bool

	e := &Engine{
		Build: func(_ context.Context, _ schema.GroupVersionKind, d func()) error {
			done = d
			return nil
		},
		Stopped: func(ctx context.Context, gvk schema.GroupVersionKind) {
			got = append(got, gvk)
			live = ctx.Err() == nil
		},
	}

	if err := e.Start(t.Context(), testGVK); err != nil {
		t.Fatalf("Start: %v", err)
	}
	done()

	if !slices.Equal(got, []schema.GroupVersionKind{testGVK}) {
		t.Errorf("Stopped saw %v, want [%v]", got, testGVK)
	}
	if !live {
		t.Error("Stopped was handed an already-cancelled context; anything it tried to wake would give up immediately")
	}
}

// A done that lost its generation belongs to a controller two Starts ago. It
// clears nothing, so it must report nothing either — waking the class over a
// controller that is running would be noise on every class update.
func TestEngine_StoppedDoesNotFireForAStaleDone(t *testing.T) {
	var dones []func()
	var got []schema.GroupVersionKind

	e := &Engine{
		Build: func(_ context.Context, _ schema.GroupVersionKind, d func()) error {
			dones = append(dones, d)
			return nil
		},
		removeInformer: func(context.Context, schema.GroupVersionKind) error { return nil },
		Stopped:        func(_ context.Context, gvk schema.GroupVersionKind) { got = append(got, gvk) },
	}

	if err := e.Start(t.Context(), testGVK); err != nil {
		t.Fatalf("Start (1st): %v", err)
	}
	if err := e.Stop(t.Context(), testGVK); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := e.Start(t.Context(), testGVK); err != nil {
		t.Fatalf("Start (2nd): %v", err)
	}

	dones[0]()

	if len(got) != 0 {
		t.Errorf("Stopped fired %v for a controller that had already been replaced", got)
	}
}
