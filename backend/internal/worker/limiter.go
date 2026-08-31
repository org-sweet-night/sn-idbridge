// SPDX-License-Identifier: MIT

package worker

import (
	"context"
	"time"
)

type backgroundLimiter struct {
	slots            chan struct{}
	acquireTimeout   time.Duration
	operationTimeout time.Duration
}

func newBackgroundLimiter(maxConcurrent int, acquireTimeout, operationTimeout time.Duration) *backgroundLimiter {
	if maxConcurrent <= 0 {
		maxConcurrent = 2
	}
	if acquireTimeout <= 0 {
		acquireTimeout = 2 * time.Second
	}
	if operationTimeout <= 0 {
		operationTimeout = 5 * time.Minute
	}
	return &backgroundLimiter{
		slots:            make(chan struct{}, maxConcurrent),
		acquireTimeout:   acquireTimeout,
		operationTimeout: operationTimeout,
	}
}

func (l *backgroundLimiter) do(ctx context.Context, operation func(context.Context) error) error {
	release, err := l.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()

	operationCtx, cancel := context.WithTimeout(ctx, l.operationTimeout)
	defer cancel()
	return operation(operationCtx)
}

func (l *backgroundLimiter) acquire(ctx context.Context) (func(), error) {
	acquireCtx, cancel := context.WithTimeout(ctx, l.acquireTimeout)
	defer cancel()
	select {
	case l.slots <- struct{}{}:
		return func() { <-l.slots }, nil
	case <-acquireCtx.Done():
		return nil, acquireCtx.Err()
	}
}
