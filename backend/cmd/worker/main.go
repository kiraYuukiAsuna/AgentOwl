package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"agentowl/backend/internal/app"
	"agentowl/backend/internal/platform"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	cfg, e := platform.LoadConfig()
	if e != nil {
		slog.Error("configuration invalid", "reason", e.Error())
		os.Exit(1)
	}
	worker, e := app.NewWorker(ctx, cfg)
	if e != nil {
		slog.Error("service configuration invalid")
		os.Exit(1)
	}
	defer worker.Services.Close()
	slog.Info("worker started")
	if e = worker.Run(ctx); e != nil {
		slog.Error("worker stopped")
		os.Exit(1)
	}
}
