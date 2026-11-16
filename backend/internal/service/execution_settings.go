package service

import (
	"agentowl/backend/internal/taskcore"
	"context"
	"encoding/json"
	"errors"
	"math"
	"time"

	"agentowl/backend/internal/runner"
	"github.com/jackc/pgx/v5"
)

func defaultExecutionSettings() Data {
	l := runner.DefaultLimits()
	return Data{"resource_limits": runner.ResourceLimitsRequired, "max_concurrent": 1,
		"network_mode": "none", "read_only_rootfs": true, "test_workspace_writable": false, "approval_mode": "policy",
		"timeout_seconds": l.TimeoutSeconds, "memory_mb": l.MemoryBytes >> 20, "cpu_millis": l.CPUMillis, "pids": l.PIDs,
		"workspace_mb": l.WorkspaceBytes >> 20, "max_files": l.MaxFiles, "max_tool_calls": l.MaxToolCalls,
		"max_tokens": l.MaxTokens, "artifact_mb": l.ArtifactBytes >> 20, "job_retry_attempts": 5, "job_retry_base_seconds": 2,
		"job_retry_max_seconds": 64, "external_retry_attempts": 0, "external_retry_delay_seconds": 60, "version": 0, "updated_at": nil, "updated_by": nil}
}

func readExecutionSettings(ctx context.Context, q DB) (Data, error) {
	var raw []byte
	var version int64
	var updated time.Time
	var actor *string
	err := q.QueryRow(ctx, "SELECT data,version,updated_at,updated_by::text FROM system_settings WHERE name='execution'").Scan(&raw, &version, &updated, &actor)
	if errors.Is(err, pgx.ErrNoRows) {
		return defaultExecutionSettings(), nil
	}
	if err != nil {
		return nil, err
	}
	stored := Data{}
	if err = json.Unmarshal(raw, &stored); err != nil {
		return nil, err
	}
	m := defaultExecutionSettings()
	for k, v := range stored {
		if k == "approval_mode" {
			continue
		}
		m[k] = v
	}
	m["version"], m["updated_at"], m["updated_by"] = version, updated, actor
	return m, nil
}

func (s *Service) executionCapabilities(ctx context.Context) Data {
	return s.runtimeMachineCapabilitySummary(ctx)
}

func (s *Service) GetExecutionSettings(ctx context.Context, a Actor) (Data, error) {
	if a.ID == "" {
		return nil, Forbidden
	}
	m, err := readExecutionSettings(ctx, s.DB)
	if err != nil {
		return nil, err
	}
	delete(m, "approval_mode")
	m["runtime"] = s.executionCapabilities(ctx)
	return m, nil
}

func boundedSettingInteger(value any, min, max int64) (int64, bool) {
	var n int64
	switch v := value.(type) {
	case int:
		n = int64(v)
	case int64:
		n = v
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) || math.Trunc(v) != v || v < float64(min) || v > float64(max) {
			return 0, false
		}
		n = int64(v)
	default:
		return 0, false
	}
	return n, n >= min && n <= max
}

func validateExecutionSettings(b Data) (Data, error) {
	const maxExactInteger int64 = 1<<53 - 1
	const maxMegabytes int64 = math.MaxInt64 >> 20
	bounds := map[string][2]int64{"max_concurrent": {1, 4}, "timeout_seconds": {0, 3600}, "memory_mb": {64, 8192},
		"cpu_millis": {100, 8000}, "pids": {16, 512}, "workspace_mb": {0, maxMegabytes}, "max_files": {0, min(maxExactInteger, int64(math.MaxInt))},
		"max_tool_calls": {0, min(maxExactInteger, int64(math.MaxInt))}, "max_tokens": {0, min(maxExactInteger, int64(math.MaxInt))}, "artifact_mb": {0, maxMegabytes},
		"job_retry_attempts": {1, 10}, "job_retry_base_seconds": {1, 3600}, "job_retry_max_seconds": {1, 86400}, "external_retry_attempts": {0, 5}, "external_retry_delay_seconds": {1, 86400}}
	for key := range b {
		if _, ok := bounds[key]; !ok && !contains([]string{"resource_limits", "expected_version", "network_mode", "read_only_rootfs", "test_workspace_writable", "approval_mode"}, key) {
			return nil, Fail(422, "validation", "包含无法修改的任务执行设置")
		}
	}
	if mode, exists := b["approval_mode"]; exists && mode != "policy" {
		return nil, Fail(422, "approval_policy_required", "请在审批策略中设置各操作的审批方式")
	}
	m := Data{"resource_limits": Str(b, "resource_limits")}
	for key, options := range map[string][]string{"network_mode": {"none", "bridge"}} {
		if !contains(options, Str(b, key)) {
			return nil, Fail(422, "validation", "请选择有效的网络方式")
		}
		m[key] = Str(b, key)
	}
	for _, key := range []string{"read_only_rootfs", "test_workspace_writable"} {
		value, ok := b[key].(bool)
		if !ok {
			return nil, Fail(422, "validation", "请提供有效的文件系统配置")
		}
		m[key] = value
	}
	if !contains([]string{runner.ResourceLimitsRequired, runner.ResourceLimitsDisabled}, Str(m, "resource_limits")) {
		return nil, Fail(422, "validation", "请选择有效的资源限制模式")
	}
	for key, bound := range bounds {
		n, ok := boundedSettingInteger(b[key], bound[0], bound[1])
		if !ok {
			return nil, Fail(422, "validation", "执行额度必须为支持范围内的整数："+key)
		}
		m[key] = n
	}
	if Str(m, "resource_limits") == runner.ResourceLimitsDisabled && Num(m, "max_concurrent") != 1 {
		return nil, Fail(422, "validation", "关闭资源限制时，同时执行的任务数必须为 1")
	}
	return m, nil
}

func (s *Service) SaveExecutionSettings(ctx context.Context, tx pgx.Tx, a Actor, b Data) (Data, error) {
	if !a.Has("admin") {
		return nil, Forbidden
	}
	expected, ok := boundedSettingInteger(b["expected_version"], 0, 9007199254740991)
	if !ok {
		return nil, Fail(422, "validation", "请提供当前配置版本")
	}
	m, err := validateExecutionSettings(b)
	if err != nil {
		return nil, err
	}
	// Read capabilities before taking a write lock; the Runner reads settings back
	// through the internal API when reporting its current admission configuration.
	caps := s.executionCapabilities(ctx)
	if Str(m, "resource_limits") == runner.ResourceLimitsRequired {
		if !Bool(caps, "reachable") {
			return nil, Fail(422, "runner_unavailable", "暂时无法确认执行器能力，请恢复连接后再启用资源限制")
		}
		if !Bool(caps, "supports_resource_limits") {
			return nil, Fail(422, "resource_limits_unsupported", "当前宿主不支持 CPU、内存和进程数硬限制")
		}
	}
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(728690107)"); err != nil {
		return nil, err
	}
	old, err := readExecutionSettings(ctx, tx)
	if err != nil {
		return nil, err
	}
	if Num(old, "version") != expected {
		return nil, Conflict
	}
	_, err = tx.Exec(ctx, `INSERT INTO system_settings(name,data,version,updated_by) VALUES('execution',$1,1,$2)
		ON CONFLICT(name) DO UPDATE SET data=EXCLUDED.data,version=system_settings.version+1,updated_at=now(),updated_by=EXCLUDED.updated_by`, JSON(m), a.ID)
	if err != nil {
		return nil, err
	}
	if err = s.invalidateUnstartedSettingsApprovals(ctx, tx); err != nil {
		return nil, err
	}
	if err = audit(ctx, tx, a, "save", "execution-settings", "execution", ""); err != nil {
		return nil, err
	}
	m, err = readExecutionSettings(ctx, tx)
	if err == nil {
		delete(m, "approval_mode")
		m["runtime"] = caps
	}
	return m, err
}

func executionLimits(m Data) runner.Limits {
	return runner.Limits{TimeoutSeconds: int(Num(m, "timeout_seconds")), MemoryBytes: Num(m, "memory_mb") << 20,
		CPUMillis: Num(m, "cpu_millis"), PIDs: Num(m, "pids"), WorkspaceBytes: Num(m, "workspace_mb") << 20,
		MaxFiles: int(Num(m, "max_files")), MaxToolCalls: int(Num(m, "max_tool_calls")), MaxTokens: int(Num(m, "max_tokens")), ArtifactBytes: Num(m, "artifact_mb") << 20}
}

func executionProfile(m Data) runner.ExecutionProfile {
	return runner.ExecutionProfile{NetworkMode: Str(m, "network_mode"), ReadOnlyRootFS: Bool(m, "read_only_rootfs"), TestWorkspaceWritable: Bool(m, "test_workspace_writable")}
}

func (s *Service) RunnerSettings(ctx context.Context) (Data, error) {
	execution, err := readExecutionSettings(ctx, s.DB)
	if err != nil {
		return nil, err
	}
	model, err := s.RunnerModelSettings(ctx)
	if err != nil {
		return nil, err
	}
	internalModel := Data{}
	for _, key := range []string{"enabled", "base_url", "model_name", "api_key", "allow_http", "json_mode", "max_tokens_field", "max_daily_tokens", "version"} {
		if value, ok := model[key]; ok {
			internalModel[key] = value
		}
	}
	return Data{"execution_version": Num(execution, "version"), "resource_limits": Str(execution, "resource_limits"), "max_concurrent": Num(execution, "max_concurrent"), "execution_profile": executionProfile(execution), "model": internalModel}, nil
}

func (s *Service) invalidateUnstartedSettingsApprovals(ctx context.Context, tx pgx.Tx) error {
	// An already running attempt keeps its approved settings and model. Only
	// unstarted steps need to be prepared again; resume advances their task epoch.
	rows, err := many(ctx, tx, `WITH invalidated AS (
		UPDATE approvals a SET status='stale',version=a.version+1,updated_at=now()
		FROM task_steps st WHERE a.subject_type='step' AND a.subject_id=st.id
		AND st.attempt_id='' AND st.stage IN ('plan','edit','test','review','publish_pr')
		AND NOT EXISTS (SELECT 1 FROM external_operations o WHERE o.approval_id=a.id::text
			AND o.kind='publish_pr' AND o.status NOT IN ('pending','cancelled'))
		AND a.status IN ('pending','approved') RETURNING st.task_id,a.id
	), cancelled AS (
		UPDATE external_operations SET status='cancelled',error='执行设置已更新，请重新审批',version=version+1,updated_at=now()
		WHERE kind='publish_pr' AND status='pending' AND approval_id IN (SELECT id::text FROM invalidated)
	)
	SELECT to_jsonb(t) FROM dev_tasks t WHERE id IN (SELECT task_id FROM invalidated) AND status NOT IN ('paused','cancelled','completed')`)
	if err != nil {
		return err
	}
	for _, t := range rows {
		if err = taskFact(ctx, tx, Str(t, "id"), taskcore.Block, taskcore.Facts{}, Data{"error": "模型或执行设置已更新，请恢复任务并重新审批"}); err != nil {
			return err
		}
	}
	return nil
}
