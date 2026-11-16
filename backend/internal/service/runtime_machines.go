package service

import (
	"context"
	"net/http"
	"sort"
	"time"

	"agentowl/backend/internal/runner"
)

type runtimeMachineClient struct {
	machine Data
	client  *runner.Client
}

func (s *Service) runtimeMachines(ctx context.Context) ([]Data, error) {
	rows, err := many(ctx, s.DB, "SELECT to_jsonb(c) FROM configs c WHERE kind='runtime-machines' ORDER BY c.name")
	if err != nil {
		return nil, err
	}
	out := make([]Data, 0, len(rows))
	for _, row := range rows {
		out = append(out, configPublic(row))
	}
	return out, nil
}

func (s *Service) runtimeMachineClients(ctx context.Context) ([]runtimeMachineClient, error) {
	machines, err := s.runtimeMachines(ctx)
	if err != nil {
		return nil, err
	}
	clients := make([]runtimeMachineClient, 0, len(machines))
	for _, machine := range machines {
		if fallback(Str(machine, "status"), "active") == "disabled" {
			continue
		}
		_, secret, err := s.config(ctx, s.DB, Str(machine, "id"), "runtime-machines")
		if err != nil {
			return nil, err
		}
		token := Str(secret, "token")
		if token == "" {
			continue
		}
		clients = append(clients, runtimeMachineClient{machine: machine, client: runner.NewClient(Str(machine, "base_url"), token, &http.Client{Timeout: 30 * time.Second})})
	}
	sort.SliceStable(clients, func(i, j int) bool {
		left, right := Num(clients[i].machine, "scheduling_weight"), Num(clients[j].machine, "scheduling_weight")
		if left == right {
			return Str(clients[i].machine, "name") < Str(clients[j].machine, "name")
		}
		return left > right
	})
	return clients, nil
}

func imageAllowedOnMachine(machine Data, image string) bool {
	allowed := Strings(machine, "allowed_images")
	return len(allowed) == 0 || contains(allowed, image)
}

func (s *Service) pickRuntimeMachine(ctx context.Context, spec runner.RunSpec) (runtimeMachineClient, error) {
	clients, err := s.runtimeMachineClients(ctx)
	if err != nil {
		return runtimeMachineClient{}, err
	}
	outdated, compatible := false, false
	for _, candidate := range clients {
		if status := fallback(Str(candidate.machine, "status"), "active"); status == "draining" {
			continue
		}
		if !imageAllowedOnMachine(candidate.machine, spec.Image) {
			continue
		}
		caps, err := candidate.client.Capabilities(ctx)
		if err != nil || !caps.Ready {
			continue
		}
		if spec.TaskInputProtocol > 0 && (!caps.TaskInputReceipts || !caps.ContextRefPages) {
			continue
		}
		if spec.ContextManifest.Version >= 2 && caps.ContextManifestVersion < 2 {
			outdated = true
			continue
		}
		compatible = true
		if caps.ResourceLimits != "" && spec.ResourceLimits != "" && caps.ResourceLimits != spec.ResourceLimits {
			continue
		}
		if caps.MaxConcurrent > 0 && caps.ActiveRuns >= caps.MaxConcurrent {
			continue
		}
		return candidate, nil
	}
	if outdated && !compatible {
		return runtimeMachineClient{}, Fail(503, "runtime_context_unsupported", "请升级 Runtime Machine 以支持当前任务上下文")
	}
	return runtimeMachineClient{}, Fail(503, "runtime_machine_unavailable", "当前没有可用的 Runtime Machine")
}

func (s *Service) runtimeMachineCapabilitySummary(ctx context.Context) Data {
	clients, err := s.runtimeMachineClients(ctx)
	if err != nil || len(clients) == 0 {
		return Data{"reachable": false, "ready": false, "error": "执行器尚未连接"}
	}
	items := []Data{}
	ready := false
	supportsResourceLimits := false
	for _, machine := range clients {
		entry, err := s.runtimeMachineCapabilities(ctx, Str(machine.machine, "id"))
		if err != nil {
			return nil
		}
		ready = ready || Bool(entry, "ready")
		supportsResourceLimits = supportsResourceLimits || Bool(entry, "supports_resource_limits")
		items = append(items, entry)
	}
	return Data{"reachable": true, "ready": ready, "supports_resource_limits": supportsResourceLimits, "machines": items, "source": "pool"}
}
