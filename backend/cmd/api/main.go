package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"agentowl/backend/internal/app"
	"agentowl/backend/internal/platform"
)

func main() {
	if err := run(); err != nil {
		slog.Error("API stopped", "reason", err.Error())
		os.Exit(1)
	}
}
func run() error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	cfg, err := platform.LoadConfig()
	if err != nil {
		return err
	}
	api, err := app.NewAPI(ctx, cfg)
	if err != nil {
		return err
	}
	defer api.Services.Close()
	servers := api.HTTPServers()
	failed := make(chan error, 2)
	for _, server := range servers {
		go func() { failed <- server.ListenAndServe() }()
	}
	slog.Info("API started", "listen", cfg.Listen)
	var serveErr error
	select {
	case <-ctx.Done():
	case e := <-failed:
		if !errors.Is(e, http.ErrServerClosed) {
			serveErr = e
			cancel()
		}
	}
	stop, done := context.WithTimeout(context.Background(), 15*time.Second)
	defer done()
	for _, server := range servers {
		_ = server.Shutdown(stop)
	}
	return serveErr
}
