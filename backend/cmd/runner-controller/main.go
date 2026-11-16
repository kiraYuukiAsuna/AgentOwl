package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"agentowl/backend/internal/runner"
)

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
func run() error {
	addr := env("RUNNER_ADDR", ":8090")
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, port, err := net.SplitHostPort(addr)
		if err != nil {
			return err
		}
		req, _ := http.NewRequestWithContext(ctx, "GET", "http://"+net.JoinHostPort("127.0.0.1", port)+"/healthz", nil)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer res.Body.Close()
		if res.StatusCode != 200 {
			return errors.New("runner not ready")
		}
		return nil
	}
	token, err := secret("RUNNER_SERVICE_TOKEN")
	if err != nil {
		return err
	}
	callbackToken, err := secret("RUNNER_CALLBACK_TOKEN")
	if err != nil {
		return err
	}
	if len(token) < 32 || len(callbackToken) < 32 {
		return errors.New("runner service and callback tokens must contain at least 32 characters")
	}
	machineID := os.Getenv("RUNNER_MACHINE_ID")
	if machineID == "" {
		return errors.New("RUNNER_MACHINE_ID is required")
	}
	apiURL := os.Getenv("RUNNER_API_URL")
	if apiURL == "" {
		return errors.New("RUNNER_API_URL is required")
	}
	callback := runner.NewHTTPAuthorizer(apiURL, callbackToken, machineID, nil)
	storedBytes, err := storageLimitBytes(env("RUNNER_MAX_STORED_MB", "0"))
	if err != nil {
		return err
	}
	controller, err := runner.NewController(runner.Config{DataRoot: env("RUNNER_DATA_ROOT", "/runner"), HostDataRoot: os.Getenv("RUNNER_HOST_DATA_ROOT"), SettingsProvider: callback, ContextProvider: callback, AllowedImages: split(os.Getenv("RUNNER_ALLOWED_IMAGES")), MaxStoredBytes: storedBytes, MinFreeBytes: int64(number("RUNNER_MIN_FREE_MB", 256)) << 20}, runner.NewEngine(env("RUNNER_DOCKER_SOCKET", "/runner/control/docker.sock")), callback, nil)
	if err != nil {
		return err
	}
	startup, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	err = controller.Recover(startup)
	cancel()
	if err != nil {
		return err
	}
	handler, err := runner.NewHandler(controller, token)
	if err != nil {
		return err
	}
	server := &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 60 * time.Second, WriteTimeout: 120 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	result := make(chan error, 1)
	go func() { result <- server.ListenAndServe() }()
	select {
	case err := <-result:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-signalCtx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = controller.Close(shutdown)
	return server.Shutdown(shutdown)
}
func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
func number(key string, fallback int) int {
	if value, err := strconv.Atoi(os.Getenv(key)); err == nil && value > 0 {
		return value
	}
	return fallback
}
func storageLimitBytes(value string) (int64, error) {
	megabytes, err := strconv.ParseUint(value, 10, 64)
	if err != nil || megabytes > (1<<63-1)>>20 {
		return 0, errors.New("RUNNER_MAX_STORED_MB must be a nonnegative integer that fits in bytes; 0 means unlimited")
	}
	return int64(megabytes) << 20, nil
}
func split(value string) []string {
	var out []string
	for _, v := range strings.Split(value, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}
func secret(key string) (string, error) {
	if file := os.Getenv(key + "_FILE"); file != "" {
		data, err := os.ReadFile(file)
		if err != nil {
			return "", errors.New("unable to read " + key + "_FILE")
		}
		return strings.TrimSpace(string(data)), nil
	}
	return os.Getenv(key), nil
}
