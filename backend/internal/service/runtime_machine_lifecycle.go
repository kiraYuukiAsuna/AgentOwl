package service

import (
	"context"
	"time"

	"agentowl/backend/internal/runner"
)

func (s *Service) runnerForAttempt(ctx context.Context, attempt Data) (RunnerAPI, Data, error) {
	if machineID := Str(attempt, "runtime_machine_id"); machineID != "" {
		machine, secret, err := s.config(ctx, s.DB, machineID, "runtime-machines")
		if err != nil {
			return nil, nil, err
		}
		token := Str(secret, "token")
		if token == "" {
			return nil, nil, Fail(503, "runtime_machine_unavailable", "Runtime Machine 未配置访问令牌")
		}
		return runtimeMachineRunner{machine: machine, client: runner.NewClient(Str(machine, "base_url"), token, nil)}, machine, nil
	}
	return nil, nil, Fail(503, "runtime_machine_unbound", "运行未绑定 Runtime Machine")
}

func (s *Service) runtimeMachineCapabilities(ctx context.Context, id string) (Data, error) {
	machine, secret, err := s.config(ctx, s.DB, id, "runtime-machines")
	if err != nil {
		return nil, err
	}
	token := Str(secret, "token")
	if token == "" {
		return Data{"id": Str(machine, "id"), "name": Str(machine, "name"), "status": fallback(Str(machine, "status"), "active"), "reachable": false, "ready": false, "error": "缺少 Runtime Machine 令牌"}, nil
	}
	client := runner.NewClient(Str(machine, "base_url"), token, nil)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	caps, err := client.Capabilities(ctx)
	if err != nil {
		return Data{"id": Str(machine, "id"), "name": Str(machine, "name"), "status": fallback(Str(machine, "status"), "active"), "reachable": false, "ready": false, "error": "暂时无法连接执行器，请检查服务状态"}, nil
	}
	result := decodeData(caps)
	if caps.ContextManifestVersion < 2 {
		result["ready"] = false
		result["error"] = "请升级 Runtime Machine 以支持当前任务上下文"
	}
	result["id"] = Str(machine, "id")
	result["name"] = Str(machine, "name")
	result["status"] = fallback(Str(machine, "status"), "active")
	result["labels"] = Object(machine, "labels")
	result["base_url"] = Str(machine, "base_url")
	return result, nil
}
