// Package wait provides deadline-bounded polling for tests and reconcilers.
//
// It exists so that no test in this repository reaches for time.Sleep. A sleep
// encodes a guess about how long a cluster takes to converge; on a loaded CI
// runner that guess is wrong in one direction and the test flakes, and on a
// fast machine it is wrong in the other and the test is slow. Polling against a
// deadline is correct in both cases, and the last observed error is reported
// when the deadline expires, which is the difference between "timed out" and a
// diagnosis.
package wait

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrDeadline is returned when the context expires before the condition holds.
// Callers that need to distinguish a timeout from a hard failure branch on it
// with errors.Is.
var ErrDeadline = errors.New("wait: deadline exceeded")

// ConditionFunc reports whether the awaited state has been reached.
//
// Returning (false, nil) means "not yet, keep polling". Returning a non-nil
// error aborts the wait immediately: use it for conditions that can never
// become true, such as a resource that has entered a terminal failed phase.
// A transient error from the API server should be reported as (false, nil)
// with the error recorded via Describe, not returned.
type ConditionFunc func(ctx context.Context) (bool, error)

// For polls fn every interval until it reports true, it returns an error, or
// ctx is done. The condition is evaluated once before the first tick, so a
// condition that already holds costs nothing.
//
// On expiry the returned error wraps ErrDeadline and names what was being
// awaited, so a failing e2e run says which object never converged.
func For(ctx context.Context, interval time.Duration, what string, fn ConditionFunc) error {
	if interval <= 0 {
		return fmt.Errorf("wait for %s: interval must be positive, got %v", what, interval)
	}

	ok, err := fn(ctx)
	if err != nil {
		return fmt.Errorf("wait for %s: %w", what, err)
	}
	if ok {
		return nil
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for %s: %w after %w", what, ErrDeadline, ctx.Err())
		case <-ticker.C:
			ok, err := fn(ctx)
			if err != nil {
				return fmt.Errorf("wait for %s: %w", what, err)
			}
			if ok {
				return nil
			}
		}
	}
}

// Stable polls until fn has reported true continuously for the whole of
// settle, and fails if the condition ever flaps back to false.
//
// Cluster state converges and then unconverges: a Deployment reports Available
// before its second replica is scheduled, and a DRBD volume reports UpToDate
// mid-resync. Asserting on the first true reading turns those races into
// intermittent green builds, which is worse than a red one.
func Stable(ctx context.Context, interval, settle time.Duration, what string, fn ConditionFunc) error {
	if settle <= 0 {
		return fmt.Errorf("wait for stable %s: settle must be positive, got %v", what, settle)
	}

	var since time.Time
	return For(ctx, interval, what, func(ctx context.Context) (bool, error) {
		ok, err := fn(ctx)
		if err != nil {
			return false, err
		}
		if !ok {
			since = time.Time{}
			return false, nil
		}
		if since.IsZero() {
			since = time.Now()
		}
		return time.Since(since) >= settle, nil
	})
}
