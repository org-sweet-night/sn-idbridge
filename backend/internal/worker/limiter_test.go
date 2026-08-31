// SPDX-License-Identifier: MIT

package worker

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestBackgroundLimiterTimesOutInsteadOfWaitingIndefinitely(t *testing.T) {
	limiter := newBackgroundLimiter(1, 20*time.Millisecond, time.Second)
	release, err := limiter.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	start := time.Now()
	_, err = limiter.acquire(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("acquire error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("acquire took %s, want fast failure", elapsed)
	}
}

func TestBackgroundLimiterBoundsAdmittedOperation(t *testing.T) {
	limiter := newBackgroundLimiter(1, time.Second, 20*time.Millisecond)
	started := make(chan struct{})
	finished := make(chan error, 1)

	go func() {
		finished <- limiter.do(context.Background(), func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		})
	}()

	select {
	case <-started:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("operation was not admitted")
	}
	if err := <-finished; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("operation error = %v, want deadline exceeded", err)
	}
}
