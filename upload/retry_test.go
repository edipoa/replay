package upload

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRetryWithBackoff(t *testing.T) {
	errBoom := errors.New("boom")

	t.Run("succeeds first try, no retry", func(t *testing.T) {
		calls := 0
		err := retryWithBackoff(context.Background(), 4, time.Millisecond, time.Millisecond,
			func() error { calls++; return nil }, nil)
		if err != nil || calls != 1 {
			t.Fatalf("err=%v calls=%d, want nil/1", err, calls)
		}
	})

	t.Run("retries then succeeds", func(t *testing.T) {
		calls := 0
		err := retryWithBackoff(context.Background(), 4, time.Millisecond, time.Millisecond,
			func() error {
				calls++
				if calls < 3 {
					return errBoom
				}
				return nil
			}, nil)
		if err != nil || calls != 3 {
			t.Fatalf("err=%v calls=%d, want nil/3", err, calls)
		}
	})

	t.Run("gives up after maxAttempts and returns last error", func(t *testing.T) {
		calls := 0
		err := retryWithBackoff(context.Background(), 4, time.Millisecond, time.Millisecond,
			func() error { calls++; return errBoom }, nil)
		if !errors.Is(err, errBoom) || calls != 4 {
			t.Fatalf("err=%v calls=%d, want errBoom/4", err, calls)
		}
	})

	t.Run("stops early when ctx is cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		calls := 0
		err := retryWithBackoff(ctx, 4, 10*time.Millisecond, 10*time.Millisecond,
			func() error {
				calls++
				cancel() // cancel during the first failed attempt
				return errBoom
			}, nil)
		if err == nil {
			t.Fatal("want non-nil error after cancellation")
		}
		if calls != 1 {
			t.Fatalf("calls=%d, want 1 (should not retry after cancel)", calls)
		}
	})
}
