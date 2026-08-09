package wait_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rusik69/paas/pkg/wait"
)

func TestFor_ReturnsImmediatelyWhenConditionAlreadyHolds(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		calls := 0

		err := wait.For(t.Context(), time.Second, "nothing", func(context.Context) (bool, error) {
			calls++
			return true, nil
		})
		if err != nil {
			t.Fatalf("For() = %v, want nil", err)
		}
		if calls != 1 {
			t.Errorf("condition calls = %d, want 1", calls)
		}
		if elapsed := time.Since(start); elapsed != 0 {
			t.Errorf("elapsed = %v, want 0 — a satisfied condition must not wait a tick", elapsed)
		}
	})
}

func TestFor_PollsUntilConditionHolds(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		calls := 0

		err := wait.For(t.Context(), 100*time.Millisecond, "third poll", func(context.Context) (bool, error) {
			calls++
			return calls == 3, nil
		})
		if err != nil {
			t.Fatalf("For() = %v, want nil", err)
		}
		// One immediate evaluation plus two ticks.
		if want := 200 * time.Millisecond; time.Since(start) != want {
			t.Errorf("elapsed = %v, want %v", time.Since(start), want)
		}
	})
}

func TestFor_DeadlineExceededNamesWhatWasAwaited(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
		defer cancel()

		err := wait.For(ctx, 100*time.Millisecond, "pvc/data to bind", func(context.Context) (bool, error) {
			return false, nil
		})
		if !errors.Is(err, wait.ErrDeadline) {
			t.Fatalf("For() = %v, want error wrapping ErrDeadline", err)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("For() = %v, want error wrapping context.DeadlineExceeded", err)
		}
		if got, want := err.Error(), "pvc/data to bind"; !contains(got, want) {
			t.Errorf("error %q does not name what was awaited (%q)", got, want)
		}
	})
}

func TestFor_TerminalErrorAbortsWithoutWaitingOutTheDeadline(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		terminal := errors.New("cluster entered Failed phase")

		err := wait.For(t.Context(), time.Minute, "cluster ready", func(context.Context) (bool, error) {
			return false, terminal
		})
		if !errors.Is(err, terminal) {
			t.Fatalf("For() = %v, want error wrapping %v", err, terminal)
		}
		if elapsed := time.Since(start); elapsed != 0 {
			t.Errorf("elapsed = %v, want 0 — a terminal error must not be retried", elapsed)
		}
	})
}

func TestFor_RejectsNonPositiveInterval(t *testing.T) {
	t.Parallel()

	for _, interval := range []time.Duration{0, -time.Second} {
		if err := wait.For(t.Context(), interval, "x", func(context.Context) (bool, error) {
			return true, nil
		}); err == nil {
			t.Errorf("For(interval=%v) = nil, want error", interval)
		}
	}
}

func TestStable_RejectsAConditionThatFlaps(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()

		// True, then false, then true forever: a volume that reports UpToDate
		// before resync completes. Stable must not accept the first reading.
		calls := 0
		err := wait.Stable(ctx, 100*time.Millisecond, time.Second, "volume uptodate",
			func(context.Context) (bool, error) {
				calls++
				return calls != 2, nil
			})
		if err != nil {
			t.Fatalf("Stable() = %v, want nil", err)
		}
		// The settle window can only have started at or after the flap.
		if calls < 12 {
			t.Errorf("condition calls = %d, want >= 12 — settle window restarted too early", calls)
		}
	})
}

func TestStable_TimesOutWhenNeverStable(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()

		calls := 0
		err := wait.Stable(ctx, 100*time.Millisecond, 500*time.Millisecond, "flapping thing",
			func(context.Context) (bool, error) {
				calls++
				return calls%2 == 0, nil
			})
		if !errors.Is(err, wait.ErrDeadline) {
			t.Fatalf("Stable() = %v, want error wrapping ErrDeadline", err)
		}
	})
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
