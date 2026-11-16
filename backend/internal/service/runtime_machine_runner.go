package service

import (
	"context"
	"io"

	"agentowl/backend/internal/runner"
)

type runtimeMachineRunner struct {
	machine Data
	client  *runner.Client
}

func (r runtimeMachineRunner) Capabilities(ctx context.Context) (runner.Capabilities, error) {
	return r.client.Capabilities(ctx)
}
func (r runtimeMachineRunner) Start(ctx context.Context, spec runner.RunSpec) (runner.Run, error) {
	return r.client.Start(ctx, spec)
}
func (r runtimeMachineRunner) Inspect(ctx context.Context, id string) (runner.Run, error) {
	return r.client.Inspect(ctx, id)
}
func (r runtimeMachineRunner) Stop(ctx context.Context, id string) (runner.Run, error) {
	return r.client.Stop(ctx, id)
}
func (r runtimeMachineRunner) Collect(ctx context.Context, id string) (io.ReadCloser, error) {
	return r.client.Collect(ctx, id)
}
func (r runtimeMachineRunner) Acknowledge(ctx context.Context, id string) error {
	return r.client.Acknowledge(ctx, id)
}
func (r runtimeMachineRunner) ContextManifest(ctx context.Context, runID string) (runner.ContextManifest, error) {
	return r.client.ContextManifest(ctx, runID)
}
func (r runtimeMachineRunner) ContextRef(ctx context.Context, runID, refID string) (string, error) {
	return r.client.ContextRef(ctx, runID, refID)
}
