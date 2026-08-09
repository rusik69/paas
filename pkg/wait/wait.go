// Package wait provides deadline-bounded polling.
//
// It exists so no test in this repository reaches for time.Sleep: a sleep
// encodes a guess about how long a cluster takes to converge, and that guess is
// wrong on a loaded CI runner in one direction and on a fast machine in the
// other.
package wait

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrDeadline is returned when the context expires before the condition holds.
var ErrDeadline = errors.New("wait: deadline exceeded")

// ConditionFunc reports whether the awaited state has been reached.
//
// (false, nil) means "not yet, keep polling". A non-nil error aborts the wait
// immediately, so it is for conditions that can never become true — a resource
// in a terminal failed phase. Report a transient API error as (false, nil).
type ConditionFunc func(ctx context.Context) (bool, error)

// For polls fn every interval until it reports true, returns an error, or ctx
// is done. The condition is evaluated once before the first tick.
//
// On expiry the error wraps ErrDeadline and names what was awaited, so a failing
// e2e run says which object never converged.
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

// Stable polls until fn has reported true continuously for the whole of settle.
//
// Cluster state converges and then unconverges — a DRBD volume reports UpToDate
// mid-resync — so accepting the first true reading turns those races into
// intermittently green builds.
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
