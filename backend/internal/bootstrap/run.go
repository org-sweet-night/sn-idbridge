// SPDX-License-Identifier: MIT

package bootstrap

import (
	"context"
	"fmt"
	"time"

	"github.com/smices/open-idb/internal/platform/postgres"
)

func Run(ctx context.Context, cfg Config) (Result, error) {
	if err := cfg.Validate(); err != nil {
		return Result{}, err
	}
	pool, err := postgres.NewPool(ctx, cfg.DatabaseURL, postgres.PoolConfig{
		MaxConns:        2,
		MinConns:        0,
		MinConnsSet:     true,
		AcquireTimeout:  5 * time.Second,
		ApplicationName: "idbridge-sandbox-bootstrap",
	})
	if err != nil {
		return Result{}, fmt.Errorf("connect bootstrap database: %w", err)
	}
	defer pool.Close()
	db := postgres.NewQuerier(pool, 5*time.Second)
	service, err := NewService(db)
	if err != nil {
		return Result{}, err
	}
	return service.Apply(ctx, cfg)
}
