package main

import (
	"agentowl/backend/internal/platform"
	"context"
	"log/slog"
	"os"
	"time"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c, e := platform.LoadConfig()
	if e != nil {
		slog.Error("configuration", "error", e)
		os.Exit(1)
	}
	db, e := platform.Open(ctx, c)
	if e != nil {
		slog.Error("database unavailable")
		os.Exit(1)
	}
	defer db.Close()
	if e = platform.Migrate(ctx, db); e != nil {
		slog.Error("migration failed", "error", e)
		os.Exit(1)
	}
	slog.Info("migrations applied")
}
