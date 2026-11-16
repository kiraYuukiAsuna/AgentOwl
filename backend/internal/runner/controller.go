package runner

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"agentowl/backend/internal/integration"
)

type Model interface {
	Complete(context.Context, integration.ChatRequest) (integration.ChatResponse, error)
}

type Config struct {
	DataRoot         string
	HostDataRoot     string
	ResourceLimits   string
	SettingsProvider SettingsProvider
	ContextProvider  ContextProvider
	AllowedImages    []string
	MaxConcurrent    int
	MaxStoredBytes   int64
	MinFreeBytes     int64
	LeasePoll        time.Duration
	MaxRequestBytes  int64
}

type activeRun struct {
	run            Run
	spec           RunSpec
	cancel         context.CancelFunc
	permitUntil    time.Time
	permissionLost bool
	model          Model
}
type Controller struct {
	cfg                      Config
	engine                   *Engine
	authorizer               Authorizer
	model                    Model
	mu                       sync.Mutex
	runs                     map[string]*activeRun
	preflight                func(context.Context, string) error
	observedExecutionVersion int64
	observedResourceLimits   string
	observedModelVersion     int64
}

func DefaultLimits() Limits {
	return Limits{TimeoutSeconds: 900, MemoryBytes: 1 << 30, CPUMillis: 1000, PIDs: 128}
}

func NewController(cfg Config, engine *Engine, authorizer Authorizer, model Model) (*Controller, error) {
	if engine == nil || authorizer == nil {
		return nil, errors.New("Docker engine and permission authorizer are required")
	}
	if !filepath.IsAbs(cfg.DataRoot) || !filepath.IsAbs(cfg.HostDataRoot) {
		return nil, errors.New("runner data paths must be absolute")
	}
	var err error
	cfg.ResourceLimits, err = normalizeResourceLimits(cfg.ResourceLimits)
	if err != nil {
		return nil, err
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 1
	}
	if cfg.MaxConcurrent > 4 {
		return nil, errors.New("runner concurrency exceeds supported limit")
	}
	if cfg.ResourceLimits == ResourceLimitsDisabled && cfg.MaxConcurrent != 1 {
		return nil, errors.New("disabled resource limits require RUNNER_MAX_CONCURRENT=1")
	}
	if cfg.MaxStoredBytes < 0 || cfg.MaxRequestBytes < 0 {
		return nil, errors.New("runner storage and request limits cannot be negative")
	}
	if cfg.MinFreeBytes <= 0 {
		cfg.MinFreeBytes = 256 << 20
	}
	if cfg.LeasePoll <= 0 {
		cfg.LeasePoll = 10 * time.Second
	}
	if len(cfg.AllowedImages) == 0 {
		return nil, errors.New("at least one pinned toolchain image is required")
	}
	for _, image := range cfg.AllowedImages {
		if !imagePattern.MatchString(image) {
			return nil, errors.New("toolchain images must be pinned by SHA-256 digest")
		}
	}
	for _, sub := range []string{"tasks", "runtime", "manifests"} {
		if err := os.MkdirAll(filepath.Join(cfg.DataRoot, sub), 0700); err != nil {
			return nil, err
		}
	}
	actual, err := filepath.EvalSymlinks(cfg.DataRoot)
	if err != nil {
		return nil, err
	}
	if actual != filepath.Clean(cfg.DataRoot) {
		return nil, errors.New("runner data root must not contain symlinks")
	}
	c := &Controller{cfg: cfg, engine: engine, authorizer: authorizer, model: model, runs: map[string]*activeRun{}}
	c.preflight = func(ctx context.Context, resourceLimits string) error {
		info, err := engine.Info(ctx)
		if err != nil {
			return err
		}
		if err := validateInfo(info, cfg.HostDataRoot, resourceLimits); err != nil {
			return err
		}
		return diskPreflight(cfg.DataRoot, cfg.MinFreeBytes)
	}
	if warning := resourceWarning(cfg.ResourceLimits); cfg.SettingsProvider == nil && warning != "" {
		log.Printf("runner resource_limits=%s warning=%q", cfg.ResourceLimits, warning)
	}
	return c, nil
}

var idPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,95}$`)
var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var treePattern = regexp.MustCompile(`^[a-f0-9]{40}([a-f0-9]{24})?$`)
var imagePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/-]+@sha256:[a-f0-9]{64}$`)

func (c *Controller) validate(spec RunSpec) error {
	for _, id := range []string{spec.RunID, spec.TaskID, spec.StepID, spec.AttemptID} {
		if !idPattern.MatchString(id) {
			return errors.New("invalid run/task/step/attempt identifier")
		}
	}
	if spec.Epoch < 1 || !digestPattern.MatchString(spec.ActionDigest) || !digestPattern.MatchString(spec.InputDigest) || !treePattern.MatchString(spec.TreeSHA) {
		return errors.New("invalid epoch or evidence digest")
	}
	if spec.Stage != "plan" && spec.Stage != "edit" && spec.Stage != "test" && spec.Stage != "review" {
		return errors.New("unsupported execution stage")
	}
	allowedImage := false
	for _, im := range c.cfg.AllowedImages {
		if spec.Image == im {
			allowedImage = true
		}
	}
	if !allowedImage {
		return errors.New("image is not in controller allowlist")
	}
	l := spec.Limits
	if l.TimeoutSeconds < 0 || l.TimeoutSeconds > 3600 || l.MemoryBytes < 64<<20 || l.MemoryBytes > 8<<30 || l.CPUMillis < 100 || l.CPUMillis > 8000 || l.PIDs < 16 || l.PIDs > 512 || l.WorkspaceBytes < 0 || l.MaxFiles < 0 || l.MaxToolCalls < 0 || l.MaxTokens < 0 || l.ArtifactBytes < 0 || spec.DailyTokenBudget < 0 {
		return errors.New("execution limits missing or outside controller bounds")
	}
	if err := validateExecutionProfile(executionProfile(spec)); err != nil {
		return err
	}
	if l.MaxFiles > 0 && len(spec.Files) > l.MaxFiles {
		return errors.New("snapshot has too many files")
	}
	var total int64
	for _, f := range spec.Files {
		total = addSize(total, int64(len(f.Data)))
	}
	if l.WorkspaceBytes > 0 && total > l.WorkspaceBytes {
		return errors.New("snapshot exceeds workspace byte limit")
	}
	digest, err := SnapshotDigest(spec.Files)
	if err != nil {
		return err
	}
	if digest != spec.InputDigest {
		return errors.New("input snapshot digest mismatch")
	}
	for _, p := range spec.AllowedPaths {
		if p != "." {
			if err := validPath(strings.TrimSuffix(p, "/")); err != nil {
				return err
			}
		}
	}
	if spec.Stage == "edit" && len(spec.AllowedPaths) == 0 {
		return errors.New("edit stage requires approved paths")
	}
	if spec.Stage == "test" {
		if len(spec.Argv) == 0 || len(spec.Argv) > 64 {
			return errors.New("test requires approved argv")
		}
	} else if len(spec.Argv) > 0 {
		return errors.New("non-test stages cannot execute commands")
	}
	for _, arg := range spec.Argv {
		if len(arg) > 8192 || strings.ContainsRune(arg, 0) {
			return errors.New("invalid command argument")
		}
	}
	// Network access does not grant credentials or arbitrary environment keys.
	envAllowed := map[string]bool{"GOCACHE": true, "GOMODCACHE": true, "GOPATH": true, "GOPROXY": true, "GOSUMDB": true, "CGO_ENABLED": true, "GOOS": true, "GOARCH": true, "HOME": true, "TMPDIR": true, "NPM_CONFIG_CACHE": true, "CI": true}
	for k, v := range spec.Env {
		if !envAllowed[k] || strings.ContainsRune(v, 0) || len(v) > 4096 {
			return errors.New("environment variable is not allowed in test profile")
		}
	}
	return nil
}

func permitRequest(s RunSpec, digest, phase string) PermitRequest {
	return PermitRequest{Phase: phase, RunID: s.RunID, TaskID: s.TaskID, StepID: s.StepID, AttemptID: s.AttemptID, Epoch: s.Epoch, ActionDigest: s.ActionDigest, SpecDigest: digest}
}
func terminal(state string) bool {
	return state == "succeeded" || state == "failed" || state == "stopped" || state == "blocked"
}
func containerName(runID string) string {
	sum := sha256.Sum256([]byte(runID))
	return "agentowl-" + hex.EncodeToString(sum[:16])
}

func (c *Controller) Start(ctx context.Context, spec RunSpec) (Run, error) {
	if spec.ExecutionProfile != nil {
		profile := *spec.ExecutionProfile
		spec.ExecutionProfile = &profile
	}
	if err := c.validate(spec); err != nil {
		return Run{}, err
	}
	if err := validateTaskInputs(spec); err != nil {
		return Run{}, err
	}
	digest, err := SpecDigest(spec)
	if err != nil {
		return Run{}, err
	}
	c.mu.Lock()
	if existing, ok := c.runs[spec.RunID]; ok {
		r := existing.run
		c.mu.Unlock()
		if r.SpecDigest != digest {
			return Run{}, ErrConflict
		}
		return r, nil
	}
	c.mu.Unlock()
	settings, model, err := c.settingsForRun(ctx, spec)
	if err != nil {
		return Run{}, err
	}
	if err := c.preflight(ctx, settings.ResourceLimits); err != nil {
		return Run{}, fmt.Errorf("runner preflight: %w", err)
	}
	if err := c.engine.ImageExists(ctx, spec.Image); err != nil {
		return Run{}, fmt.Errorf("approved toolchain image unavailable: %w", err)
	}
	permit, err := c.authorizer.Authorize(ctx, permitRequest(spec, digest, "admit"))
	if err != nil {
		return Run{}, errors.New("run authorization unavailable")
	}
	if !permit.Allowed || !permit.ExpiresAt.After(time.Now()) {
		if !permit.Allowed && permit.Reason == "settings_changed" {
			return Run{}, fmt.Errorf("%w: approved execution settings changed before admission", ErrConflict)
		}
		return Run{}, errors.New("run authorization denied or expired")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.runs[spec.RunID]; ok {
		if existing.run.SpecDigest != digest {
			return Run{}, ErrConflict
		}
		return existing.run, nil
	}
	if c.cfg.SettingsProvider != nil && settings.ExecutionVersion < c.observedExecutionVersion {
		return Run{}, fmt.Errorf("%w: runner settings changed during admission", ErrConflict)
	}
	if c.cfg.SettingsProvider != nil && settings.Model.Version < c.observedModelVersion {
		return Run{}, fmt.Errorf("%w: model settings changed during admission", ErrConflict)
	}
	active := 0
	for _, run := range c.runs {
		if run.spec.TaskID == spec.TaskID && run.spec.AttemptID == spec.AttemptID {
			return Run{}, ErrConflict
		}
		if !terminal(run.run.State) {
			active++
		}
	}
	if active >= settings.MaxConcurrent {
		return Run{}, errors.New("runner capacity exhausted")
	}
	reserved := addSize(int64(lenMustJSON(spec)), addSize(spec.Limits.ArtifactBytes, multiplySize(int64(spec.Limits.MaxFiles), 1024)))
	if err := c.checkStoredCapacity(reserved); err != nil {
		return Run{}, err
	}
	r := Run{RunID: spec.RunID, TaskID: spec.TaskID, AttemptID: spec.AttemptID, Epoch: spec.Epoch, Stage: spec.Stage, State: "queued", SpecDigest: digest, CreatedAt: time.Now().UTC(), ExecutionVersion: settings.ExecutionVersion, ModelVersion: settings.Model.Version, ResourceLimits: settings.ResourceLimits, ResourceWarning: resourceWarning(settings.ResourceLimits), Result: Result{InputDigest: spec.InputDigest, TreeSHA: spec.TreeSHA}}
	for _, in := range spec.TaskInputs {
		r.Result.InputReceipts = append(r.Result.InputReceipts, InputReceipt{InputID: in.ID, Digest: in.Digest, Phase: "delivered", PartID: "all"})
	}
	a := &activeRun{run: r, spec: spec, permitUntil: permit.ExpiresAt, model: model}
	if err := os.MkdirAll(c.runDir(spec.RunID), 0700); err != nil {
		return Run{}, err
	}
	if err := atomicJSON(filepath.Join(c.runDir(spec.RunID), "spec.json"), spec); err != nil {
		return Run{}, err
	}
	if err := c.persist(a); err != nil {
		return Run{}, err
	}
	var runCtx context.Context
	var cancel context.CancelFunc
	if spec.Limits.TimeoutSeconds == 0 {
		runCtx, cancel = context.WithCancel(context.Background())
	} else {
		runCtx, cancel = context.WithTimeout(context.Background(), time.Duration(spec.Limits.TimeoutSeconds)*time.Second)
	}
	a.cancel = cancel
	c.runs[spec.RunID] = a
	if r.ResourceWarning != "" {
		log.Printf("runner run_id=%s resource_limits=%s warning=%q", spec.RunID, r.ResourceLimits, r.ResourceWarning)
	}
	go c.execute(runCtx, a)
	return r, nil
}

func lenMustJSON(v any) int { b, _ := json.Marshal(v); return len(b) }
func (c *Controller) Inspect(id string) (Run, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a, ok := c.runs[id]
	if !ok {
		return Run{}, ErrNotFound
	}
	return a.run, nil
}
func (c *Controller) Stop(ctx context.Context, id string) (Run, error) {
	c.mu.Lock()
	a, ok := c.runs[id]
	if !ok {
		c.mu.Unlock()
		return Run{}, ErrNotFound
	}
	if terminal(a.run.State) {
		r := a.run
		c.mu.Unlock()
		return r, nil
	}
	wasUnknown := a.run.State == "outcome_unknown"
	a.run.State = "stopping"
	if a.cancel != nil {
		a.cancel()
	}
	idContainer := a.run.ContainerID
	_ = c.persist(a)
	c.mu.Unlock()
	if wasUnknown {
		restoreUnknown := func(err error) (Run, error) {
			c.mu.Lock()
			a.run.State = "outcome_unknown"
			a.run.Result.EvidenceValid = false
			_ = c.persist(a)
			c.mu.Unlock()
			return Run{}, err
		}
		container, err := c.engine.Inspect(ctx, containerName(a.spec.RunID))
		if err != nil && !dockerNotFound(err) {
			return restoreUnknown(err)
		}
		if err == nil {
			if !containerMatches(container, a.spec, a.run.SpecDigest) {
				return restoreUnknown(ErrConflict)
			}
			idContainer = container.ID
			if err := c.engine.Kill(ctx, idContainer); err != nil {
				return restoreUnknown(err)
			}
		}
		c.mu.Lock()
		a.run.ContainerID = idContainer
		a.run.State = "collecting"
		a.run.Result.EvidenceValid = false
		now := time.Now().UTC()
		a.run.FinishedAt = &now
		c.mu.Unlock()
		_ = c.collect(a, "stopped")
		c.mu.Lock()
		a.run.State = "stopped"
		_ = c.persist(a)
		c.mu.Unlock()
		if idContainer != "" {
			_ = c.engine.Remove(ctx, idContainer)
		}
		_ = os.RemoveAll(c.workspace(a.spec))
		return c.Inspect(id)
	}
	if idContainer != "" {
		if err := c.engine.Kill(ctx, idContainer); err != nil {
			return Run{}, err
		}
	}
	return c.Inspect(id)
}

func (c *Controller) execute(ctx context.Context, a *activeRun) {
	defer a.cancel()
	defer func() {
		c.mu.Lock()
		a.model = nil
		c.mu.Unlock()
	}()
	done := make(chan struct{})
	defer close(done)
	go c.renew(ctx, a, done)
	err := c.prepare(ctx, a)
	if err == nil {
		c.mu.Lock()
		a.run.State = "running"
		now := time.Now().UTC()
		a.run.StartedAt = &now
		err = c.persist(a)
		c.mu.Unlock()
	}
	if err == nil {
		if a.spec.Stage == "test" {
			err = c.test(ctx, a)
		} else {
			err = c.agent(ctx, a)
		}
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c.mu.Lock()
	containerID := a.run.ContainerID
	unknown := a.run.State == "outcome_unknown"
	c.mu.Unlock()
	if unknown {
		c.mu.Lock()
		a.run.Result.Error = "container creation outcome unknown; explicit stop or restart reconciliation required"
		a.run.Result.EvidenceValid = false
		_ = c.persist(a)
		c.mu.Unlock()
		return
	}
	if containerID != "" {
		if killErr := c.engine.Kill(stopCtx, containerID); killErr != nil {
			c.mu.Lock()
			a.run.State = "outcome_unknown"
			a.run.Result.Error = "container stop could not be confirmed; reconciliation required"
			a.run.Result.EvidenceValid = false
			_ = c.persist(a)
			c.mu.Unlock()
			return
		}
	}
	if ctx.Err() != nil && err == nil {
		err = ctx.Err()
	}
	c.mu.Lock()
	finalState := "succeeded"
	if err != nil {
		a.run.Result.Error = redact(err.Error())
		a.run.Result.EvidenceValid = false
		if a.run.State == "stopping" || errors.Is(err, context.Canceled) {
			finalState = "stopped"
		} else {
			finalState = "failed"
		}
	}
	a.run.State = "collecting"
	now := time.Now().UTC()
	a.run.FinishedAt = &now
	c.mu.Unlock()
	if archiveErr := c.collect(a, finalState); archiveErr != nil {
		c.mu.Lock()
		finalState = "failed"
		a.run.Result.EvidenceValid = false
		a.run.Result.Error = "artifact collection: " + redact(archiveErr.Error())
		c.mu.Unlock()
	}
	c.mu.Lock()
	if a.run.State == "stopping" {
		finalState = "stopped"
		a.run.Result.EvidenceValid = false
	}
	a.run.State = finalState
	_ = c.persist(a)
	c.mu.Unlock()
	if containerID != "" {
		_ = c.engine.Remove(stopCtx, containerID)
	}
	_ = os.RemoveAll(c.workspace(a.spec))
}

func (c *Controller) prepare(ctx context.Context, a *activeRun) error {
	name := containerName(a.spec.RunID)
	container, err := c.engine.Inspect(ctx, name)
	if err == nil {
		if !containerMatches(container, a.spec, a.run.SpecDigest) {
			return ErrConflict
		}
		c.mu.Lock()
		a.run.ContainerID = container.ID
		_ = c.persist(a)
		c.mu.Unlock()
		// A reused runtime may already have executed code; it must be reconciled, not restarted.
		return errors.New("existing attempt container requires reconciliation; use a new approved attempt")
	}
	if !dockerNotFound(err) {
		return fmt.Errorf("runtime identity cannot be established: %w", err)
	}
	if err := storeSnapshot(c.workspace(a.spec), a.spec.Files); err != nil {
		return err
	}
	hostPath := filepath.Join(c.cfg.HostDataRoot, "tasks", a.spec.TaskID, a.spec.AttemptID)
	id, err := c.engine.Create(ctx, a.spec, name, hostPath, a.run.SpecDigest, a.run.ResourceLimits)
	if err != nil {
		lookupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		found, lookupErr := c.engine.Inspect(lookupCtx, name)
		if lookupErr != nil {
			c.mu.Lock()
			a.run.State = "outcome_unknown"
			c.mu.Unlock()
			return errors.New("container creation outcome unknown; reconciliation required")
		}
		if !containerMatches(found, a.spec, a.run.SpecDigest) {
			return ErrConflict
		}
		id = found.ID
		if found.State.Running {
			c.mu.Lock()
			a.run.ContainerID = id
			_ = c.persist(a)
			c.mu.Unlock()
			return errors.New("unexpected running container after create")
		}
	}
	c.mu.Lock()
	a.run.ContainerID = id
	err = c.persist(a)
	c.mu.Unlock()
	if err != nil {
		return err
	}
	if a.spec.Stage == "test" && executionProfile(a.spec).TestWorkspaceWritable {
		if err := c.engine.SeedTestWorkspace(ctx, id, a.spec.Files); err != nil {
			return fmt.Errorf("initialize independent test workspace: %w", err)
		}
	}
	permit, err := c.authorizer.Authorize(ctx, permitRequest(a.spec, a.run.SpecDigest, "prepare"))
	if err != nil || !permit.Allowed || !permit.ExpiresAt.After(time.Now()) {
		return errors.New("authorization expired before container start")
	}
	return c.engine.Start(ctx, id)
}

func (c *Controller) renew(ctx context.Context, a *activeRun, done <-chan struct{}) {
	ticker := time.NewTicker(c.cfg.LeasePoll)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			requestCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			permit, err := c.authorizer.Authorize(requestCtx, permitRequest(a.spec, a.run.SpecDigest, "renew"))
			cancel()
			c.mu.Lock()
			a.permissionLost = err != nil
			if err == nil && permit.Allowed && permit.ExpiresAt.After(time.Now()) {
				a.permitUntil = permit.ExpiresAt
			}
			expired := !a.permitUntil.After(time.Now())
			denied := err == nil && !permit.Allowed
			if denied || expired {
				a.cancel()
			}
			c.mu.Unlock()
		}
	}
}

func (c *Controller) test(ctx context.Context, a *activeRun) error {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		container, err := c.engine.Inspect(ctx, a.run.ContainerID)
		if err != nil {
			return err
		}
		if !container.State.Running {
			code := container.State.ExitCode
			c.mu.Lock()
			a.run.Result.ExitCode = &code
			c.mu.Unlock()
			if executionProfile(a.spec).TestWorkspaceWritable {
				volume, err := c.openTestVolume(a, container)
				if err != nil {
					return fmt.Errorf("verify test workspace: %w", err)
				}
				defer volume.Close()
				// Only the approved source files are compared. Generated dependency
				// trees stay in the disposable volume, never the archive or PR.
				for _, approved := range a.spec.Files {
					if err := ctx.Err(); err != nil {
						return err
					}
					actual, err := readTestInputFile(volume, approved.Path, int64(len(approved.Data)))
					if err != nil {
						return fmt.Errorf("verify approved test input %q: %w", approved.Path, err)
					}
					if actual.Mode != approved.Mode || string(actual.Data) != string(approved.Data) {
						return errors.New("test modified approved input in its writable workspace; evidence invalid")
					}
				}
			}
			files, err := readSnapshot(c.workspace(a.spec), a.spec.Limits.WorkspaceBytes, a.spec.Limits.MaxFiles)
			if err != nil {
				return err
			}
			digest, err := SnapshotDigest(files)
			if err != nil {
				return err
			}
			c.mu.Lock()
			a.run.Result.OutputDigest = digest
			a.run.Result.EvidenceValid = digest == a.spec.InputDigest && !container.State.OOMKilled
			a.run.Result.Summary = fmt.Sprintf("Approved test exited with code %d", code)
			c.mu.Unlock()
			if digest != a.spec.InputDigest {
				return errors.New("test modified frozen input; evidence invalid")
			}
			if container.State.OOMKilled {
				return errors.New("test exceeded memory limit")
			}
			if code != 0 {
				return fmt.Errorf("test exited with code %d", code)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (c *Controller) runDir(id string) string { return filepath.Join(c.cfg.DataRoot, "manifests", id) }
func (c *Controller) workspace(s RunSpec) string {
	return filepath.Join(c.cfg.DataRoot, "tasks", s.TaskID, s.AttemptID)
}
func (c *Controller) persist(a *activeRun) error {
	return atomicJSON(filepath.Join(c.runDir(a.spec.RunID), "run.json"), a.run)
}
func atomicJSON(name string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	tmp := name + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(b)
	syncErr := f.Sync()
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	if syncErr != nil {
		return syncErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(tmp, name); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(name))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (c *Controller) checkStoredCapacity(additional int64) error {
	if additional < 0 {
		return errors.New("invalid storage reservation")
	}
	var total int64
	err := filepath.WalkDir(filepath.Join(c.cfg.DataRoot, "manifests"), func(_ string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("symlink in manifest storage")
		}
		st, err := entry.Info()
		if err != nil {
			return err
		}
		total = addSize(total, st.Size())
		return nil
	})
	if err != nil {
		return err
	}
	if c.cfg.MaxStoredBytes > 0 && (total > c.cfg.MaxStoredBytes || additional > c.cfg.MaxStoredBytes-total) {
		return errors.New("runner artifact store full; acknowledge collected runs before retrying")
	}
	available, err := diskAvailable(filepath.Join(c.cfg.DataRoot, "manifests"))
	if err != nil {
		return err
	}
	if available < c.cfg.MinFreeBytes || available-c.cfg.MinFreeBytes < additional {
		return errors.New("runner storage reserve would be exceeded")
	}
	return nil
}

func (c *Controller) collect(a *activeRun, finalState string) error {
	files, err := readSnapshot(c.workspace(a.spec), a.spec.Limits.WorkspaceBytes, a.spec.Limits.MaxFiles)
	if os.IsNotExist(err) {
		files = nil
		err = nil
	}
	if err != nil {
		return err
	}
	digest, err := SnapshotDigest(files)
	if err != nil {
		return err
	}
	c.mu.Lock()
	a.run.Result.OutputDigest = digest
	run := a.run
	run.State = finalState
	c.mu.Unlock()
	manifest := make([]map[string]any, 0, len(files))
	for _, f := range files {
		sum := sha256.Sum256(f.Data)
		manifest = append(manifest, map[string]any{"path": f.Path, "mode": f.Mode, "size": len(f.Data), "sha256": hex.EncodeToString(sum[:])})
	}
	resultData, _ := json.MarshalIndent(run, "", "  ")
	manifestData, _ := json.MarshalIndent(manifest, "", "  ")
	entries := []File{{Path: "result.json", Mode: 0644, Data: resultData}, {Path: "manifest.json", Mode: 0644, Data: manifestData}, {Path: "diff.patch", Mode: 0644, Data: []byte(diffText(a.spec.Files, files))}}
	if events, err := os.ReadFile(filepath.Join(c.runDir(a.spec.RunID), "tools.ndjson")); err == nil {
		entries = append(entries, File{Path: "tools.ndjson", Mode: 0644, Data: events})
	}
	if a.run.ContainerID != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		logs, logsErr := c.engine.Logs(ctx, a.run.ContainerID, 4<<20)
		cancel()
		if logsErr == nil {
			entries = append(entries, File{Path: "test.log", Mode: 0644, Data: logs})
		}
	}
	for _, f := range files {
		f.Path = "workspace/" + f.Path
		entries = append(entries, f)
	}
	var total int64
	for _, f := range entries {
		total = addSize(total, int64(len(f.Data)))
	}
	if a.spec.Limits.ArtifactBytes > 0 && total > a.spec.Limits.ArtifactBytes {
		return errors.New("artifact total exceeds approved limit")
	}
	c.mu.Lock()
	err = c.checkStoredCapacity(total)
	c.mu.Unlock()
	if err != nil {
		return err
	}
	name := filepath.Join(c.runDir(a.spec.RunID), "artifacts.tar.gz")
	fd, err := os.OpenFile(name+".tmp", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(fd)
	tw := tar.NewWriter(gz)
	for _, f := range entries {
		if err = tw.WriteHeader(&tar.Header{Name: f.Path, Mode: int64(f.Mode), Size: int64(len(f.Data)), Typeflag: tar.TypeReg}); err != nil {
			break
		}
		if _, err = tw.Write(f.Data); err != nil {
			break
		}
	}
	for _, closer := range []io.Closer{tw, gz, fd} {
		if closeErr := closer.Close(); err == nil {
			err = closeErr
		}
	}
	if err != nil {
		_ = os.Remove(name + ".tmp")
		return err
	}
	return os.Rename(name+".tmp", name)
}

func (c *Controller) Collect(id string) (*os.File, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a, ok := c.runs[id]
	if !ok {
		return nil, ErrNotFound
	}
	if !terminal(a.run.State) {
		return nil, ErrNotReady
	}
	fd, err := os.Open(filepath.Join(c.runDir(id), "artifacts.tar.gz"))
	if os.IsNotExist(err) {
		return nil, ErrNotReady
	}
	return fd, err
}

func (c *Controller) Acknowledge(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	a, ok := c.runs[id]
	if !ok {
		return ErrNotFound
	}
	if !terminal(a.run.State) {
		return ErrNotReady
	}
	if err := os.Remove(filepath.Join(c.runDir(id), "artifacts.tar.gz")); err != nil && !os.IsNotExist(err) {
		return err
	}
	a.run.ArtifactsAcknowledged = true
	if err := c.persist(a); err != nil {
		return err
	}
	a.spec.Files = nil
	return atomicJSON(filepath.Join(c.runDir(id), "spec.json"), a.spec)
}

// Recover never resumes an interrupted model session or executes an old attempt again.
func (c *Controller) Recover(ctx context.Context) error {
	// Recovery may need to stop a task started with resource limits disabled.
	// Establish ownership before cleanup, but never let stricter admission rules
	// (or low storage) prevent stopping containers left by the previous process.
	info, err := c.engine.Info(ctx)
	if err != nil {
		return err
	}
	if err := validateRuntimeIdentity(info, c.cfg.HostDataRoot); err != nil {
		return err
	}
	entries, err := os.ReadDir(filepath.Join(c.cfg.DataRoot, "manifests"))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !idPattern.MatchString(entry.Name()) {
			continue
		}
		var spec RunSpec
		var run Run
		if err := readJSONLimit(filepath.Join(c.runDir(entry.Name()), "spec.json"), c.cfg.MaxRequestBytes, &spec); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		if err := readJSONLimit(filepath.Join(c.runDir(entry.Name()), "run.json"), 1<<20, &run); err != nil {
			if !os.IsNotExist(err) {
				return err
			}
			digest, digestErr := SpecDigest(spec)
			if digestErr != nil {
				return digestErr
			}
			run = Run{RunID: spec.RunID, TaskID: spec.TaskID, AttemptID: spec.AttemptID, Epoch: spec.Epoch, Stage: spec.Stage, State: "outcome_unknown", SpecDigest: digest, CreatedAt: time.Now().UTC(), Result: Result{InputDigest: spec.InputDigest, TreeSHA: spec.TreeSHA}}
		}
		if spec.RunID != entry.Name() || run.RunID != entry.Name() {
			return errors.New("manifest identity mismatch")
		}
		for _, id := range []string{spec.RunID, spec.TaskID, spec.StepID, spec.AttemptID} {
			if !idPattern.MatchString(id) {
				return errors.New("unsafe identifier in stored manifest")
			}
		}
		if !run.ArtifactsAcknowledged {
			digest, err := SpecDigest(spec)
			if err != nil || digest != run.SpecDigest {
				return errors.New("stored specification digest mismatch")
			}
		} else if !terminal(run.State) {
			return errors.New("active run cannot have acknowledged artifacts")
		}
		a := &activeRun{run: run, spec: spec}
		c.runs[run.RunID] = a
		if terminal(run.State) {
			continue
		}
		container, lookupErr := c.engine.Inspect(ctx, containerName(spec.RunID))
		if lookupErr != nil && !dockerNotFound(lookupErr) {
			return lookupErr
		}
		if lookupErr == nil {
			if !containerMatches(container, spec, run.SpecDigest) {
				return ErrConflict
			}
			a.run.ContainerID = container.ID
			if err := c.engine.Kill(ctx, container.ID); err != nil {
				return err
			}
		}
		a.run.State = "failed"
		a.run.Result.Error = "controller restarted; interrupted attempt stopped, new approval check required"
		a.run.Result.EvidenceValid = false
		now := time.Now().UTC()
		a.run.FinishedAt = &now
		_ = c.collect(a, "failed")
		if err := c.persist(a); err != nil {
			return err
		}
		if a.run.ContainerID != "" {
			if err := c.engine.Remove(ctx, a.run.ContainerID); err != nil {
				return err
			}
		}
		_ = os.RemoveAll(c.workspace(spec))
	}
	managed, err := c.engine.Managed(ctx)
	if err != nil {
		return err
	}
	for _, container := range managed {
		a, ok := c.runs[container.Config.Labels["agentowl.run_id"]]
		if !ok || terminal(a.run.State) {
			if err := c.engine.Kill(ctx, container.ID); err != nil {
				return err
			}
			if err := c.engine.Remove(ctx, container.ID); err != nil {
				return err
			}
		}
	}
	if c.cfg.SettingsProvider == nil {
		return c.preflight(ctx, c.cfg.ResourceLimits)
	}
	// With API settings, keep serving capability queries even when no current
	// configuration can admit runs. Start and readiness still enforce isolation.
	return nil
}

func (c *Controller) contextManifest(ctx context.Context, runID string) (ContextManifest, error) {
	if c.cfg.ContextProvider == nil {
		return ContextManifest{}, nil
	}
	return c.cfg.ContextProvider.ContextManifest(ctx, runID)
}

func (c *Controller) contextRef(ctx context.Context, runID, refID string) (string, error) {
	if c.cfg.ContextProvider == nil {
		return "", ErrNotFound
	}
	return c.cfg.ContextProvider.ContextRef(ctx, runID, refID)
}

func (c *Controller) contextRefPage(ctx context.Context, runID, refID, cursor string) (ContextPage, error) {
	if provider, ok := c.cfg.ContextProvider.(PagedContextProvider); ok {
		return provider.ContextRefPage(ctx, runID, refID, cursor)
	}
	body, err := c.contextRef(ctx, runID, refID)
	if err != nil {
		return ContextPage{}, err
	}
	return PageContext(refID, body, cursor)
}

func readJSONLimit(name string, max int64, target any) error {
	fd, err := os.Open(name)
	if err != nil {
		return err
	}
	defer fd.Close()
	st, err := fd.Stat()
	if err != nil {
		return err
	}
	if max > 0 && st.Size() > max {
		return errors.New("manifest too large")
	}
	return json.NewDecoder(quotaReader(fd, max)).Decode(target)
}

func (c *Controller) Close(ctx context.Context) error {
	c.mu.Lock()
	ids := make([]string, 0)
	for id, a := range c.runs {
		if !terminal(a.run.State) {
			ids = append(ids, id)
		}
	}
	c.mu.Unlock()
	var first error
	for _, id := range ids {
		if _, err := c.Stop(ctx, id); err != nil && first == nil {
			first = err
		}
	}
	return first
}
