package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"agentowl/backend/internal/integration"
)

// Settings is an internal callback response. It contains credentials and must
// never be persisted as a run specification, artifact, or log entry.
type Settings struct {
	ExecutionProfile ExecutionProfile `json:"execution_profile"`
	ExecutionVersion int64            `json:"execution_version"`
	ResourceLimits   string           `json:"resource_limits"`
	MaxConcurrent    int              `json:"max_concurrent"`
	Model            ModelSettings    `json:"model"`
}

type ModelSettings struct {
	Enabled        bool   `json:"enabled"`
	BaseURL        string `json:"base_url"`
	ModelName      string `json:"model_name"`
	APIKey         string `json:"api_key"`
	AllowHTTP      bool   `json:"allow_http"`
	JSONMode       string `json:"json_mode"`
	MaxTokensField string `json:"max_tokens_field"`
	MaxDailyTokens int64  `json:"max_daily_tokens"`
	Version        int64  `json:"version"`
}

type SettingsProvider interface {
	Settings(context.Context) (Settings, error)
}

// Capabilities exposes only host capabilities and non-secret effective settings.
// Ready describes configuration and host prerequisites, not free capacity.
// Existing runs retain their settings; admission also checks ActiveRuns.
type Capabilities struct {
	ContextManifestVersion int    `json:"context_manifest_version"`
	TaskInputReceipts      bool   `json:"task_input_receipts_v1,omitempty"`
	ContextRefPages        bool   `json:"context_ref_pages_v2,omitempty"`
	NetworkMode            string `json:"network_mode"`
	ReadOnlyRootFS         bool   `json:"read_only_rootfs"`
	TestWorkspaceWritable  bool   `json:"test_workspace_writable"`
	Reachable              bool   `json:"reachable"`
	Rootless               bool   `json:"rootless"`
	FilesystemReady        bool   `json:"filesystem_ready"`
	CgroupVersion          string `json:"cgroup_version"`
	CgroupDriver           string `json:"cgroup_driver"`
	SupportsResourceLimits bool   `json:"supports_resource_limits"`
	MemoryLimit            bool   `json:"memory_limit"`
	CPULimit               bool   `json:"cpu_limit"`
	PIDsLimit              bool   `json:"pids_limit"`
	ResourceLimits         string `json:"resource_limits"`
	MaxConcurrent          int    `json:"max_concurrent"`
	ExecutionVersion       int64  `json:"execution_version"`
	ModelVersion           int64  `json:"model_version"`
	SettingsConfigured     bool   `json:"settings_configured"`
	ModelConfigured        bool   `json:"model_configured"`
	ActiveRuns             int    `json:"active_runs"`
	Ready                  bool   `json:"ready"`
	Warning                string `json:"warning,omitempty"`
	Error                  string `json:"error,omitempty"`
}

func (a *HTTPAuthorizer) Settings(ctx context.Context) (Settings, error) {
	res, err := a.client.request(ctx, "GET", a.callbackPath("/internal/runner/settings"), nil)
	if err != nil {
		return Settings{}, errors.New("runner settings unavailable")
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, (64<<10)+1))
	if err != nil || len(data) > 64<<10 {
		return Settings{}, errors.New("invalid runner settings response")
	}
	var settings Settings
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&settings); err != nil {
		return Settings{}, errors.New("invalid runner settings response")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return Settings{}, errors.New("invalid runner settings response")
	}
	return settings, nil
}

func (c *Controller) currentSettings(ctx context.Context) (Settings, error) {
	if c.cfg.SettingsProvider == nil {
		return Settings{ExecutionProfile: DefaultExecutionProfile(), ResourceLimits: c.cfg.ResourceLimits, MaxConcurrent: c.cfg.MaxConcurrent}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	s, err := c.cfg.SettingsProvider.Settings(ctx)
	if err != nil {
		return Settings{}, errors.New("runner settings unavailable")
	}
	if s.ExecutionVersion < 1 || (s.ResourceLimits != ResourceLimitsRequired && s.ResourceLimits != ResourceLimitsDisabled) || s.MaxConcurrent < 1 || s.MaxConcurrent > 4 || s.ResourceLimits == ResourceLimitsDisabled && s.MaxConcurrent != 1 || validateExecutionProfile(s.ExecutionProfile) != nil {
		return Settings{}, errors.New("runner settings are missing or invalid")
	}
	c.mu.Lock()
	if s.ExecutionVersion < c.observedExecutionVersion || s.Model.Version < c.observedModelVersion {
		c.mu.Unlock()
		return Settings{}, errors.New("runner settings response is stale")
	}
	changed := c.observedExecutionVersion != s.ExecutionVersion || c.observedResourceLimits != s.ResourceLimits
	if s.ExecutionVersion >= c.observedExecutionVersion {
		c.observedExecutionVersion, c.observedResourceLimits = s.ExecutionVersion, s.ResourceLimits
	}
	c.observedModelVersion = s.Model.Version
	c.mu.Unlock()
	if changed && resourceWarning(s.ResourceLimits) != "" {
		log.Printf("runner execution_version=%d resource_limits=%s warning=%q", s.ExecutionVersion, s.ResourceLimits, resourceWarning(s.ResourceLimits))
	}
	return s, nil
}

func configuredModel(s ModelSettings) (Model, error) {
	if !s.Enabled || s.Version < 1 || strings.TrimSpace(s.BaseURL) == "" || strings.TrimSpace(s.ModelName) == "" {
		return nil, errors.New("model settings are missing or disabled")
	}
	model, err := integration.NewModelClient(integration.ModelConfig{BaseURL: s.BaseURL, Model: s.ModelName, APIKey: s.APIKey, AllowHTTP: s.AllowHTTP, JSONMode: s.JSONMode, MaxTokensField: s.MaxTokensField, Timeout: 5 * time.Minute})
	if err != nil {
		return nil, errors.New("model settings are invalid")
	}
	return model, nil
}

func (c *Controller) settingsForRun(ctx context.Context, spec RunSpec) (Settings, Model, error) {
	s, err := c.currentSettings(ctx)
	if err != nil {
		return Settings{}, nil, err
	}
	if c.cfg.SettingsProvider == nil {
		// Explicitly injected configuration is retained for isolated tests and
		// standalone protocol fixtures; the production entry point never uses it.
		if spec.ResourceLimits != "" && spec.ResourceLimits != s.ResourceLimits {
			return Settings{}, nil, fmt.Errorf("%w: requested resource mode differs from controller configuration", ErrConflict)
		}
		s.ExecutionProfile = executionProfile(spec)
		return s, c.model, nil
	}
	if spec.ExecutionVersion < 1 || spec.ModelVersion < 1 || spec.ExecutionVersion != s.ExecutionVersion || spec.ModelVersion != s.Model.Version || spec.ResourceLimits != s.ResourceLimits || executionProfile(spec) != s.ExecutionProfile {
		return Settings{}, nil, fmt.Errorf("%w: execution or model settings changed; approval must be refreshed", ErrConflict)
	}
	model, err := configuredModel(s.Model)
	if err != nil {
		return Settings{}, nil, err
	}
	return s, model, nil
}

func (c *Controller) Capabilities(ctx context.Context) Capabilities {
	result := Capabilities{Reachable: true, TaskInputReceipts: true, ContextRefPages: true, ContextManifestVersion: 2}
	c.mu.Lock()
	for _, a := range c.runs {
		if !terminal(a.run.State) {
			result.ActiveRuns++
		}
	}
	c.mu.Unlock()
	info, infoErr := c.engine.Info(ctx)
	if infoErr == nil {
		for _, option := range info.SecurityOptions {
			if strings.Contains(option, "rootless") {
				result.Rootless = true
			}
		}
		result.CgroupVersion, result.CgroupDriver = info.CgroupVersion, info.CgroupDriver
		cgroups := result.Rootless && info.CgroupVersion == "2" && info.CgroupDriver == "systemd"
		result.MemoryLimit, result.CPULimit, result.PIDsLimit = cgroups && info.MemoryLimit, cgroups && info.CPUQuota, cgroups && info.PidsLimit
		result.SupportsResourceLimits = result.MemoryLimit && result.CPULimit && result.PIDsLimit
		// The disabled admission profile retains every non-cgroup prerequisite.
		result.FilesystemReady = validateRuntimeIdentity(info, c.cfg.HostDataRoot) == nil && c.preflight(ctx, ResourceLimitsDisabled) == nil
	}
	s, err := c.currentSettings(ctx)
	if err == nil {
		result.SettingsConfigured = true
		result.ExecutionVersion, result.ModelVersion = s.ExecutionVersion, s.Model.Version
		result.ResourceLimits, result.MaxConcurrent = s.ResourceLimits, s.MaxConcurrent
		result.NetworkMode, result.ReadOnlyRootFS, result.TestWorkspaceWritable = s.ExecutionProfile.NetworkMode, s.ExecutionProfile.ReadOnlyRootFS, s.ExecutionProfile.TestWorkspaceWritable
		result.Warning = resourceWarning(s.ResourceLimits)
		if c.cfg.SettingsProvider == nil {
			result.ModelConfigured = c.model != nil
		} else {
			_, modelErr := configuredModel(s.Model)
			result.ModelConfigured = modelErr == nil
		}
	}
	switch {
	case infoErr != nil:
		result.Error = "runner runtime unavailable"
	case !result.Rootless || !result.FilesystemReady:
		result.Error = "runner isolation preflight unavailable"
	case err != nil:
		result.Error = "runner settings unavailable"
	case s.ResourceLimits == ResourceLimitsRequired && !result.SupportsResourceLimits:
		result.Error = "host cannot enforce required CPU, memory and PID limits"
	case c.cfg.SettingsProvider != nil && !result.ModelConfigured:
		result.Error = "model settings are missing, disabled or invalid"
	default:
		result.Ready = true
	}
	return result
}
