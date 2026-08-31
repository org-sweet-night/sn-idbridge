// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/smices/open-idb/internal/app"
	"github.com/smices/open-idb/internal/bootstrap"
	"github.com/smices/open-idb/internal/config"
	"go.uber.org/zap"
)

func main() {
	logger, err := zap.NewProduction()
	if err != nil {
		panic(err)
	}
	defer func() { _ = logger.Sync() }()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if len(os.Args) == 3 && os.Args[1] == "bootstrap" && os.Args[2] == "sandbox" {
		cfg, err := bootstrap.LoadConfig()
		if err != nil {
			logger.Fatal("load sandbox bootstrap config", zap.Error(err))
		}
		result, err := bootstrap.Run(ctx, cfg)
		if err != nil {
			logger.Fatal("run sandbox bootstrap", zap.Error(err))
		}
		logger.Info("sandbox bootstrap contract satisfied", zap.String("entity_id", result.EntityID))
		return
	}
	if len(os.Args) > 1 {
		logger.Fatal("unsupported command")
	}

	cfg, err := config.Load()
	if err != nil {
		logger.Fatal("load config", zap.Error(err))
	}

	application, err := app.New(ctx, cfg, logger)
	if err != nil {
		logger.Fatal("initialize app", zap.Error(err))
	}

	if err := application.Run(ctx); err != nil {
		logger.Fatal("run app", zap.Error(err))
	}
}
