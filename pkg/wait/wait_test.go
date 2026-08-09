package wait_test

import (
	"context"
	"errors"
	"strings"
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
		if got := err.Error(); !strings.Contains(got, "pvc/data to bind") {
			t.Errorf("error %q does not name what was awaited", got)
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

func TestStable_RejectsNonPositiveSettle(t *testing.T) {
	t.Parallel()

	if err := wait.Stable(t.Context(), time.Second, 0, "x", func(context.Context) (bool, error) {
		return true, nil
	}); err == nil {
		t.Error("Stable(settle=0) = nil, want error")
	}
}

func TestStable_RejectsAConditionThatFlaps(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()

		// True, then false, then true forever: a volume reporting UpToDate
		// before resync completes.
		calls := 0
		err := wait.Stable(ctx, 100*time.Millisecond, time.Second, "volume uptodate",
			func(context.Context) (bool, error) {
				calls++
				return calls != 2, nil
			})
		if err != nil {
			t.Fatalf("Stable() = %v, want nil", err)
		}
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

func TestStable_PropagatesTerminalError(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		terminal := errors.New("resource deleted")
		err := wait.Stable(t.Context(), time.Second, time.Second, "gone",
			func(context.Context) (bool, error) { return false, terminal })
		if !errors.Is(err, terminal) {
			t.Fatalf("Stable() = %v, want error wrapping %v", err, terminal)
		}
	})
}
