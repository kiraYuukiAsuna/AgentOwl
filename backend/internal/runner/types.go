package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

type File struct {
	Path string `json:"path"`
	Mode uint32 `json:"mode"`
	Data []byte `json:"data"`
}

type Limits struct {
	TimeoutSeconds int   `json:"timeout_seconds"`
	MemoryBytes    int64 `json:"memory_bytes"`
	CPUMillis      int64 `json:"cpu_millis"`
	PIDs           int64 `json:"pids"`
	WorkspaceBytes int64 `json:"workspace_bytes"`
	MaxFiles       int   `json:"max_files"`
	MaxToolCalls   int   `json:"max_tool_calls"`
	MaxTokens      int   `json:"max_tokens"`
	ArtifactBytes  int64 `json:"artifact_bytes"`
}

type ExecutionProfile struct {
	NetworkMode           string `json:"network_mode"`
	ReadOnlyRootFS        bool   `json:"read_only_rootfs"`
	TestWorkspaceWritable bool   `json:"test_workspace_writable"`
}

func DefaultExecutionProfile() ExecutionProfile {
	return ExecutionProfile{NetworkMode: "none", ReadOnlyRootFS: true}
}

func executionProfile(spec RunSpec) ExecutionProfile {
	if spec.ExecutionProfile == nil {
		return DefaultExecutionProfile()
	}
	return *spec.ExecutionProfile
}

type ContextManifest struct {
	Version     int             `json:"version"`
	Facts       json.RawMessage `json:"facts,omitempty"`
	Coverage    string          `json:"coverage,omitempty"`
	TokenBudget int             `json:"token_budget,omitempty"`
	Refs        []ContextRef    `json:"refs,omitempty"`
}

type ContextRef struct {
	ID              string `json:"id"`
	Kind            string `json:"kind"`
	Title           string `json:"title"`
	Digest          string `json:"content_digest,omitempty"`
	Inline          string `json:"inline,omitempty"`
	ArtifactID      string `json:"artifact_id,omitempty"`
	EstimatedTokens int    `json:"estimated_tokens,omitempty"`
}

func validateExecutionProfile(profile ExecutionProfile) error {
	if profile.NetworkMode != "none" && profile.NetworkMode != "bridge" {
		return errors.New("network mode must be none or bridge")
	}
	return nil
}

type RunSpec struct {
	TaskInputProtocol  int               `json:"task_input_protocol,omitempty"`
	TaskInputDigest    string            `json:"task_input_digest,omitempty"`
	TaskInputs         []TaskInput       `json:"task_inputs,omitempty"`
	ExecutionProfile   *ExecutionProfile `json:"execution_profile,omitempty"`
	ExecutionVersion   int64             `json:"execution_version,omitempty"`
	ModelVersion       int64             `json:"model_version,omitempty"`
	ResourceLimits     string            `json:"resource_limits,omitempty"`
	DailyTokenBudget   int64             `json:"daily_token_budget,omitempty"`
	RuntimeMachineID   string            `json:"runtime_machine_id,omitempty"`
	RuntimeMachineName string            `json:"runtime_machine_name,omitempty"`
	RunID              string            `json:"run_id"`
	TaskID             string            `json:"task_id"`
	StepID             string            `json:"step_id"`
	AttemptID          string            `json:"attempt_id"`
	Epoch              int64             `json:"epoch"`
	Stage              string            `json:"stage"`
	ActionDigest       string            `json:"action_digest"`
	InputDigest        string            `json:"input_digest"`
	TreeSHA            string            `json:"tree_sha"`
	Image              string            `json:"image"`
	Goal               string            `json:"goal"`
	Context            string            `json:"context"`
	ContextManifest    ContextManifest   `json:"context_manifest,omitempty"`
	AllowedPaths       []string          `json:"allowed_paths"`
	Argv               []string          `json:"argv,omitempty"`
	Env                map[string]string `json:"env,omitempty"`
	Limits             Limits            `json:"limits"`
	Files              []File            `json:"files"`
}
type Result struct {
	InputReceipts []InputReceipt `json:"input_receipts,omitempty"`
	Summary       string         `json:"summary"`
	ExitCode      *int           `json:"exit_code,omitempty"`
	InputDigest   string         `json:"input_digest"`
	OutputDigest  string         `json:"output_digest"`
	TreeSHA       string         `json:"tree_sha"`
	EvidenceValid bool           `json:"evidence_valid"`
	Error         string         `json:"error,omitempty"`
	ToolCalls     int            `json:"tool_calls"`
	Tokens        int64          `json:"tokens"`
}

type Run struct {
	ExecutionVersion      int64      `json:"execution_version,omitempty"`
	ModelVersion          int64      `json:"model_version,omitempty"`
	RunID                 string     `json:"run_id"`
	TaskID                string     `json:"task_id"`
	AttemptID             string     `json:"attempt_id"`
	Epoch                 int64      `json:"epoch"`
	Stage                 string     `json:"stage"`
	State                 string     `json:"state"`
	SpecDigest            string     `json:"spec_digest"`
	ContainerID           string     `json:"container_id,omitempty"`
	CreatedAt             time.Time  `json:"created_at"`
	StartedAt             *time.Time `json:"started_at,omitempty"`
	FinishedAt            *time.Time `json:"finished_at,omitempty"`
	ArtifactsAcknowledged bool       `json:"artifacts_acknowledged"`
	ResourceLimits        string     `json:"resource_limits,omitempty"`
	ResourceWarning       string     `json:"resource_warning,omitempty"`
	Result                Result     `json:"result"`
}

type PermitRequest struct {
	Phase        string `json:"phase,omitempty"`
	RunID        string `json:"run_id"`
	TaskID       string `json:"task_id"`
	StepID       string `json:"step_id"`
	AttemptID    string `json:"attempt_id"`
	Epoch        int64  `json:"epoch"`
	ActionDigest string `json:"action_digest"`
	SpecDigest   string `json:"spec_digest"`
}

type Permit struct {
	Allowed   bool      `json:"allowed"`
	ExpiresAt time.Time `json:"expires_at"`
	Reason    string    `json:"reason,omitempty"`
}

type Authorizer interface {
	Authorize(context.Context, PermitRequest) (Permit, error)
}

var ErrNotFound = errors.New("run not found")
var ErrConflict = errors.New("run identity conflict")
var ErrNotReady = errors.New("run artifacts not ready")

// SnapshotDigest binds bytes and executable bits, independently of Git serialization.
func SnapshotDigest(files []File) (string, error) {
	ordered := append([]File(nil), files...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Path < ordered[j].Path })
	h := sha256.New()
	previous := ""
	seen := make(map[string]bool, len(ordered))
	for _, f := range ordered {
		if err := validPath(f.Path); err != nil {
			return "", err
		}
		if f.Path == previous {
			return "", fmt.Errorf("duplicate snapshot path: %s", f.Path)
		}
		parent := f.Path
		for strings.Contains(parent, "/") {
			parent = parent[:strings.LastIndexByte(parent, '/')]
			if seen[parent] {
				return "", fmt.Errorf("snapshot file is also a directory: %s", parent)
			}
		}
		if f.Mode != 0644 && f.Mode != 0755 {
			return "", fmt.Errorf("unsupported file mode: %s", f.Path)
		}
		previous = f.Path
		seen[f.Path] = true
		fmt.Fprintf(h, "%d:%s:%o:%d:", len(f.Path), f.Path, f.Mode, len(f.Data))
		h.Write(f.Data)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func SpecDigest(spec RunSpec) (string, error) {
	// Placement is chosen by the control plane after approval and must not change
	// the frozen run identity.
	spec.RuntimeMachineID = ""
	spec.RuntimeMachineName = ""
	// Slices have explicit order; maps use encoding/json's deterministic key order.
	b, err := json.Marshal(spec)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}
