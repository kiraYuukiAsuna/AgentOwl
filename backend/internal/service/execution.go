package service

import (
	"agentowl/backend/internal/taskcore"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"agentowl/backend/internal/integration"
	"agentowl/backend/internal/runner"
	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/errgroup"
)

func (s *Service) repoGitHub(ctx context.Context, q DB, id string) (*integration.GitHubClient, Data, error) {
	repo, secret, e := s.config(ctx, q, id, "repositories")
	if e != nil {
		return nil, nil, e
	}
	if !Bool(repo, "enabled") {
		return nil, nil, Fail(409, "repository_disabled", "仓库未启用")
	}
	client, e := integration.NewGitHubClient(integration.GitHubConfig{Owner: Str(repo, "owner"), Repo: Str(repo, "repo"), Token: Str(secret, "token"), BaseURL: Str(Object(repo, "config"), "base_url")})
	return client, repo, e
}
func (s *Service) fetchSnapshot(ctx context.Context, repoID, reference string) ([]runner.File, string, string, error) {
	settings, e := readExecutionSettings(ctx, s.DB)
	if e != nil {
		return nil, "", "", e
	}
	limits := executionLimits(settings)
	gh, repo, e := s.repoGitHub(ctx, s.DB, repoID)
	if e != nil {
		return nil, "", "", e
	}
	ref, e := gh.GetRef(ctx, "heads/"+fallback(reference, Str(repo, "base_branch")))
	if e != nil {
		return nil, "", "", e
	}
	commit, e := gh.GetCommit(ctx, ref.Object.SHA)
	if e != nil {
		return nil, "", "", e
	}
	tree, e := gh.GetFullTree(ctx, commit.Tree.SHA)
	if e != nil {
		return nil, "", "", e
	}
	entries := []integration.GitTreeEntry{}
	var declaredSize int64
	for _, entry := range tree.Tree {
		if entry.Type == "tree" {
			continue
		}
		if entry.Type != "blob" || entry.Mode != "100644" && entry.Mode != "100755" {
			return nil, "", "", Fail(422, "unsupported_tree", "仓库含子模块或符号链接，须先改为普通文件")
		}
		if entry.Size < 0 || entry.Size > math.MaxInt64-declaredSize || limits.WorkspaceBytes > 0 && entry.Size > limits.WorkspaceBytes-declaredSize || limits.MaxFiles > 0 && len(entries) >= limits.MaxFiles {
			return nil, "", "", Fail(422, "snapshot_too_large", "仓库快照超过任务配置的文件额度")
		}
		declaredSize += entry.Size
		entries = append(entries, entry)
	}
	files := make([]runner.File, len(entries))
	group, fetchCtx := errgroup.WithContext(ctx)
	group.SetLimit(8)
	var mu sync.Mutex
	var size int64
	for i, entry := range entries {
		if fetchCtx.Err() != nil {
			break
		}
		group.Go(func() error {
			b, err := gh.GetBlob(fetchCtx, entry.SHA)
			if err != nil {
				return err
			}
			mu.Lock()
			defer mu.Unlock()
			if int64(len(b)) > math.MaxInt64-size || limits.WorkspaceBytes > 0 && int64(len(b)) > limits.WorkspaceBytes-size {
				return Fail(422, "snapshot_too_large", "仓库快照超过任务配置的文件额度")
			}
			size += int64(len(b))
			mode := uint32(0644)
			if entry.Mode == "100755" {
				mode = 0755
			}
			files[i] = runner.File{Path: entry.Path, Mode: mode, Data: b}
			return nil
		})
	}
	if e = group.Wait(); e != nil {
		return nil, "", "", e
	}
	if e = ctx.Err(); e != nil {
		return nil, "", "", e
	}
	if _, e = runner.SnapshotDigest(files); e != nil {
		return nil, "", "", e
	}
	return files, ref.Object.SHA, commit.Tree.SHA, nil
}
func (s *Service) blockTask(ctx context.Context, id, reason string) error {
	return s.transaction(ctx, func(tx pgx.Tx) error {
		e := taskFact(ctx, tx, id, taskcore.Block, taskcore.Facts{}, Data{"error": reason})
		return e
	})
}

func (s *Service) prepareStep(ctx context.Context, p Data) error {
	task, e := one(ctx, s.DB, "SELECT to_jsonb(t) FROM dev_tasks t WHERE id=$1", Str(p, "task_id"))
	if e != nil {
		return e
	}
	if Num(task, "epoch") != Num(p, "epoch") || Num(task, "revision") != Num(p, "revision") || contains([]string{"paused", "cancelled", "completed"}, Str(task, "status")) {
		return nil
	}
	stage := Str(p, "stage")
	if !contains(stages, stage) || stage == "track" {
		return Fail(422, "invalid_stage", "未知开发阶段")
	}
	_, repo, e := s.repoGitHub(ctx, s.DB, Str(task, "repository_id"))
	if e != nil {
		return e
	}
	if len(Strings(repo, "allowed_paths")) == 0 {
		return s.blockTask(ctx, Str(task, "id"), "请配置仓库允许修改的路径")
	}
	if !strings.Contains(Str(repo, "runner_image"), "@sha256:") {
		return s.blockTask(ctx, Str(task, "id"), "请配置固定 digest 的 Runner 镜像")
	}
	if len(Strings(repo, "test_argv")) == 0 {
		return s.blockTask(ctx, Str(task, "id"), "请配置可信的自测命令参数")
	}
	model, e := s.CurrentModel(ctx)
	if e != nil {
		return e
	}
	clients, err := s.runtimeMachineClients(ctx)
	if err != nil {
		return err
	}
	if len(clients) == 0 || model == nil {
		return s.blockTask(ctx, Str(task, "id"), "请配置 Runtime Machine 与模型接口")
	}
	input := Data{}
	var files []runner.File
	if stage == "plan" {
		reference := ""
		expectedHead := ""
		prNumber := int64(0)
		targetBase := ""
		prior, e := one(ctx, s.DB, "SELECT to_jsonb(o) FROM external_operations o WHERE kind='publish_pr' AND subject_id=$1 AND status IN ('succeeded','failed') AND response->>'branch_created'='true' ORDER BY created_at DESC LIMIT 1", Str(task, "id"))
		if e != nil && e != NotFound {
			return e
		}
		if e == nil {
			gh, _, err := s.repoGitHub(ctx, s.DB, Str(task, "repository_id"))
			if err != nil {
				return err
			}
			reference = "agentowl/" + Str(task, "id")
			prNumber = Num(Object(prior, "response"), "pr_number")
			branch, err := gh.GetRef(ctx, "heads/"+reference)
			if err != nil {
				return err
			}
			expectedHead = branch.Object.SHA
			if prNumber > 0 {
				pr, err := gh.GetPullRequest(ctx, int(prNumber))
				if err != nil {
					return err
				}
				if pr.State != "open" || pr.Merged || pr.Head.Ref != reference || pr.Head.SHA != expectedHead {
					return s.blockTask(ctx, Str(task, "id"), "关联 PR 已关闭、合并或目标分支不匹配，请重新确认问题")
				}
			}
			target, err := gh.GetRef(ctx, "heads/"+Str(repo, "base_branch"))
			if err != nil {
				return err
			}
			targetBase = target.Object.SHA
		}
		files, inputBase, inputTree, err := s.fetchSnapshot(ctx, Str(task, "repository_id"), reference)
		if err != nil {
			return err
		}
		if expectedHead != "" && expectedHead != inputBase {
			return Conflict
		}
		input["base_commit"] = inputBase
		input["base_tree"] = inputTree
		input["target_base_commit"] = fallback(targetBase, inputBase)
		input["expected_branch_head"] = expectedHead
		input["pr_number"] = prNumber
		return s.persistPreparedStep(ctx, task, repo, stage, input, files)
	}

	previous := stages[indexStage(stage)-1]
	st, e := one(ctx, s.DB, "SELECT to_jsonb(st) FROM task_steps st WHERE task_id=$1 AND revision=$2 AND stage=$3 AND status='succeeded'", Str(task, "id"), Num(task, "revision"), previous)
	if e != nil {
		return e
	}
	prior := Object(st, "input")
	out := Object(st, "output")
	for _, key := range []string{"base_commit", "base_tree", "target_base_commit", "expected_branch_head", "pr_number", "baseline_snapshot_id", "baseline_digest", "diff_artifact_id", "test_log_artifact_id", "tools_artifact_id"} {
		input[key] = prior[key]
	}
	input["snapshot_id"] = out["snapshot_id"]
	input["previous_summary"] = out["summary"]
	input["context"] = Str(prior, "context") + "\n" + previous + ": " + Str(out, "summary")
	input["stage_hint"] = previous + ": " + Str(out, "summary")
	if previous == "edit" {
		input["diff_artifact_id"] = out["diff_artifact_id"]
	}
	if previous == "test" {
		input["test_log_artifact_id"] = out["test_log_artifact_id"]
	}
	if previous == "review" {
		input["tools_artifact_id"] = out["tools_artifact_id"]
	}
	input["tests_passed"] = Bool(out, "tests_passed") || Bool(prior, "tests_passed")
	if previous == "test" {
		input["tests_passed"] = Bool(out, "tests_passed")
	}
	input["test_digest"] = fallback(Str(out, "test_digest"), Str(prior, "test_digest"))
	files, e = s.readSnapshot(ctx, Str(input, "snapshot_id"))
	if e != nil {
		return e
	}
	if (stage == "review" || stage == "publish_pr") && (!Bool(input, "tests_passed") || Str(input, "test_digest") != mustSnapshotDigest(files)) {
		return s.blockTask(ctx, Str(task, "id"), "测试证据未绑定当前代码快照")
	}
	if stage == "review" {
		logID := Str(input, "test_log_artifact_id")
		if logID == "" {
			return s.blockTask(ctx, Str(task, "id"), "缺少自测日志，无法提供审查证据")
		}
		if _, err := s.readArtifact(ctx, logID); err != nil {
			return err
		}
	}
	input["context_manifest"] = decodeData(s.buildContextManifest(ctx, input))
	return s.persistPreparedStep(ctx, task, repo, stage, input, files)
}

func indexStage(s string) int {
	for i, x := range stages {
		if s == x {
			return i
		}
	}
	return -1
}
func mustSnapshotDigest(files []runner.File) string { d, _ := runner.SnapshotDigest(files); return d }
func (s *Service) persistPreparedStep(ctx context.Context, task, repo Data, stage string, input Data, files []runner.File) error {
	digest, e := runner.SnapshotDigest(files)
	if e != nil {
		return e
	}
	input["input_digest"] = digest
	input["image"] = Str(repo, "runner_image")
	input["allowed_paths"] = Strings(repo, "allowed_paths")
	input["argv"] = Strings(repo, "test_argv")
	input["env"] = Object(Object(repo, "config"), "test_env")
	input["goal"] = Str(task, "goal") + "\n验收条件：" + Str(task, "acceptance_criteria")
	input["context"] = fallback(Str(input, "context"), Str(input, "previous_summary"))
	var baseline []runner.File
	if stage == "plan" {
		baseline = files
	} else {
		baseline, e = s.readSnapshot(ctx, Str(input, "baseline_snapshot_id"))
		if e != nil {
			return e
		}
	}
	changed := changedPaths(baseline, files)
	for _, path := range changed {
		if !pathAllowed(path, Strings(repo, "allowed_paths")) {
			return s.blockTask(ctx, Str(task, "id"), "修改超出配置允许的路径")
		}
	}
	return s.transaction(ctx, func(tx pgx.Tx) error {
		// Serialize preparation with both settings saves before locking task or
		// approval rows. Save holds the same single-setting lock until its old
		// approvals are invalidated; execution then model is the shared order.
		if _, e := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(728690107)"); e != nil {
			return e
		}
		if _, e := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended('system_settings:model',0))"); e != nil {
			return e
		}
		settings, e := readExecutionSettings(ctx, tx)
		if e != nil {
			return e
		}
		var modelVersion int64
		if e = tx.QueryRow(ctx, "SELECT COALESCE((SELECT version FROM system_settings WHERE name='model'),0)").Scan(&modelVersion); e != nil {
			return e
		}
		limits := executionLimits(settings)
		if e = validateSnapshotQuota(files, limits); e != nil {
			return e
		}
		input["execution_settings_version"] = Num(settings, "version")
		input["model_settings_version"] = modelVersion
		input["resource_limits"] = Str(settings, "resource_limits")
		input["execution_profile"] = decodeData(executionProfile(settings))
		input["approval_mode"] = Str(settings, "approval_mode")
		input["limits"] = decodeData(limits)
		t, e := locked(ctx, tx, "dev_tasks", Str(task, "id"))
		if e != nil {
			return e
		}
		if Num(t, "epoch") != Num(task, "epoch") || Num(t, "revision") != Num(task, "revision") || contains([]string{"paused", "cancelled", "completed"}, Str(t, "status")) {
			return nil
		}
		if e = s.freezeTaskInputs(ctx, tx, t, input); e != nil {
			return e
		}
		if e = s.freezeContext(ctx, tx, t, input); e != nil {
			return e
		}
		for _, path := range changed {
			if !pathAllowed(path, Strings(input, "allowed_paths")) {
				return Fail(403, "input_scope_conflict", "修改超出任务授权的路径范围")
			}
		}
		var existingStatus string
		_ = tx.QueryRow(ctx, "SELECT status FROM task_steps WHERE task_id=$1 AND revision=$2 AND stage=$3", Str(t, "id"), Num(t, "revision"), stage).Scan(&existingStatus)
		if contains([]string{"running", "succeeded", "pending_approval"}, existingStatus) && Str(t, "status") != "queued" {
			return nil
		}
		if stage == "plan" {
			artifact, e := s.storeSnapshot(ctx, tx, Str(t, "id"), Str(t, "case_id"), "", files)
			if e != nil {
				return e
			}
			input["snapshot_id"] = Str(artifact, "id")
			input["baseline_snapshot_id"] = Str(artifact, "id")
			input["baseline_digest"] = digest
		}
		id := ID()
		st, e := one(ctx, tx, `INSERT INTO task_steps(id,task_id,revision,stage,input) VALUES($1,$2,$3,$4,$5) ON CONFLICT(task_id,revision,stage) DO UPDATE SET input=EXCLUDED.input,output='{}',status='pending_approval',error='',attempt_id='',version=task_steps.version+1,updated_at=now() RETURNING to_jsonb(task_steps)`, id, Str(t, "id"), Num(t, "revision"), stage, JSON(input))
		if e != nil {
			return e
		}
		id = Str(st, "id")
		action := Data{"case_id": Str(t, "case_id"), "task_id": Str(t, "id"), "repository_id": Str(t, "repository_id"), "repository_version": Num(repo, "version"), "revision": Num(t, "revision"), "epoch": Num(t, "epoch"), "input_digest": digest, "input_hash": Hash(JSON(input)), "file_count": len(files), "changed_paths": changed, "max_tokens": limits.MaxTokens, "timeout_seconds": limits.TimeoutSeconds, "tests_passed": Bool(input, "tests_passed"), "stage": stage, "snapshot_id": Str(input, "snapshot_id")}
		action["input"] = input
		for _, key := range []string{"goal", "context", "allowed_paths", "base_commit", "test_digest", "diff_artifact_id", "test_log_artifact_id", "context_manifest"} {
			action[key] = input[key]
		}
		action["execution_settings_version"] = input["execution_settings_version"]
		action["model_settings_version"] = input["model_settings_version"]
		action["resource_limits"] = input["resource_limits"]
		action["execution_profile"] = input["execution_profile"]
		action["approval_mode"] = input["approval_mode"]
		action["limits"] = input["limits"]
		action["test_argv"] = input["argv"]
		action["runner_image"] = input["image"]
		ap, e := s.createApproval(ctx, tx, Actor{ID: "system:worker"}, stage, "step", id, "批准开发阶段："+stage, action)
		if e != nil {
			return e
		}
		status := "pending_approval"
		if Str(ap, "status") == "rejected" {
			status = "blocked"
		}
		if _, e = tx.Exec(ctx, "UPDATE task_steps SET approval_id=$2 WHERE id=$1", id, Str(ap, "id")); e != nil {
			return e
		}
		e = taskFact(ctx, tx, Str(t, "id"), taskcore.Prepared, taskcore.Facts{NextStage: stage, Status: status}, Data{"base_commit": Str(input, "base_commit"), "error": ""})
		return e
	})
}
func changedPaths(before, after []runner.File) []string {
	old := map[string]runner.File{}
	for _, f := range before {
		old[f.Path] = f
	}
	changed := []string{}
	for _, f := range after {
		p, ok := old[f.Path]
		if !ok || p.Mode != f.Mode || string(p.Data) != string(f.Data) {
			changed = append(changed, f.Path)
		}
		delete(old, f.Path)
	}
	for p := range old {
		changed = append(changed, p)
	}
	sort.Strings(changed)
	return changed
}

func contextRefTokens(value string) int {
	n, err := runner.EstimateTextTokens(value)
	if err != nil {
		return len(value)
	}
	return n
}

func (s *Service) buildContextManifest(ctx context.Context, input Data) runner.ContextManifest {
	manifest := runner.ContextManifest{Version: 1}
	if facts := Object(input, "context_facts"); len(facts) > 0 {
		manifest.Version = 2
		manifest.Facts = JSON(facts)
		manifest.Coverage = "Frozen task inputs and locally received messages; no full chat history or personal user token."
		for _, item := range []struct{ ID, Title string }{{"case", "问题详情"}, {"task_inputs", "已采用的信息"}, {"reference_inputs", "参考信息"}, {"source_contexts", "来源消息上下文"}, {"stage_results", "已完成阶段"}, {"previous_summary", "上一阶段结果"}} {
			if body := Str(Object(input, "context_documents"), item.ID); body != "" {
				manifest.Refs = append(manifest.Refs, runner.ContextRef{ID: item.ID, Kind: "snapshot", Title: item.Title, Digest: Hash([]byte(body)), EstimatedTokens: contextRefTokens(body)})
			}
		}
	}
	addInline := func(id, kind, title, body string) {
		manifest.Refs = append(manifest.Refs, runner.ContextRef{ID: id, Kind: kind, Title: title, Inline: body, EstimatedTokens: contextRefTokens(body)})
	}
	addArtifact := func(id, kind, title, artifactID string) {
		if artifactID == "" {
			return
		}
		manifest.Refs = append(manifest.Refs, runner.ContextRef{ID: id, Kind: kind, Title: title, ArtifactID: artifactID})
	}
	if summary := strings.TrimSpace(Str(input, "previous_summary")); summary != "" && manifest.Version == 1 {
		addInline("previous_summary", "summary", "Previous stage summary", summary)
	}
	if stage := strings.TrimSpace(Str(input, "stage_hint")); stage != "" && manifest.Version == 1 {
		addInline("stage_hint", "status", "Current preparation state", stage)
	}
	addArtifact("diff", "artifact", "Code diff", Str(input, "diff_artifact_id"))
	addArtifact("test_log", "artifact", "Test log", Str(input, "test_log_artifact_id"))
	addArtifact("tools_log", "artifact", "Tool log", Str(input, "tools_artifact_id"))
	if len(manifest.Refs) > 0 {
		if tokens, err := runner.EstimateJSONTokens(manifest); err == nil {
			manifest.TokenBudget = tokens
		}
	}
	return manifest
}

func (s *Service) executeApproval(ctx context.Context, id string) error {
	var runID string
	e := s.transaction(ctx, func(tx pgx.Tx) error {
		// Keep approval execution and settings invalidation on the same lock
		// order, so saving cannot race creation of an attempt or pending PR.
		if _, e := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(728690107)"); e != nil {
			return e
		}
		if _, e := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended('system_settings:model',0))"); e != nil {
			return e
		}
		ap, e := locked(ctx, tx, "approvals", id)
		if e != nil {
			return e
		}
		if Str(ap, "status") != "approved" {
			return nil
		}
		if e = s.validateApproval(ctx, tx, ap, true); e != nil {
			var failure *Error
			if Str(ap, "kind") == "create_task" && errors.As(e, &failure) && contains([]string{"approval_expired", "policy_changed", "policy_denied", "policy_expired", "proposal_stale", "source_changed", "forbidden", "not_found", "version_conflict"}, failure.Code) {
				_, err := tx.Exec(ctx, `UPDATE approvals SET status='stale',reason=$2,version=version+1,updated_at=now() WHERE id=$1`, id, safeError(e))
				return err
			}
			return e
		}
		action := Object(ap, "action")
		switch Str(ap, "subject_type") {
		case "admission":
			return s.executeTaskCreationApproval(ctx, tx, ap)
		case "knowledge":
			if _, e = tx.Exec(ctx, "UPDATE knowledge SET status='published',version=version+1,updated_at=now() WHERE id=$1", Str(ap, "subject_id")); e != nil {
				return e
			}
			k, e := locked(ctx, tx, "knowledge", Str(ap, "subject_id"))
			if e != nil {
				return e
			}
			if _, e = tx.Exec(ctx, "INSERT INTO knowledge_versions(knowledge_id,version,snapshot) VALUES($1,$2,$3)", Str(k, "id"), Num(k, "version"), JSON(k)); e != nil {
				return e
			}
			if _, e = tx.Exec(ctx, "UPDATE approvals SET status='executed',version=version+1,updated_at=now() WHERE id=$1", id); e != nil {
				return e
			}
			return audit(ctx, tx, Actor{ID: "system:worker"}, "publish", "knowledge", Str(k, "id"), "")
		case "reply":
			r, e := locked(ctx, tx, "replies", Str(ap, "subject_id"))
			if e != nil {
				return e
			}
			op := ID()
			req := Data{"body": Str(action, "body"), "channel_version": Num(action, "channel_version"), "connector_version": Num(action, "connector_version"), "case_id": Str(r, "case_id"), "reply_id": Str(r, "id"), "evidence": action["evidence"]}
			if Bool(action, "private_channel") {
				req["private_channel"], req["external_id"], req["private_actor_id"] = true, Str(action, "external_id"), Str(action, "private_actor_id")
			}
			if mention := Str(action, "mention_user_id"); mention != "" {
				req["mention_user_id"] = mention
			}
			if e = s.bindReplyFeedback(ctx, tx, req, r, action); e != nil {
				return e
			}
			target := Data{"channel_id": Str(action, "channel_id")}
			if Bool(action, "private_channel") {
				target = Data{"channel_id": Str(action, "channel_id"), "private_channel": true, "connector_id": Str(action, "connector_id"), "external_id": Str(action, "external_id"), "private_actor_id": Str(action, "private_actor_id")}
			}
			inserted, e := one(ctx, tx, `INSERT INTO external_operations(id,kind,subject_id,dedupe_key,target,request,approval_id) VALUES($1,'reply_send',$2,$3,$4,$5,$6) ON CONFLICT(dedupe_key) DO UPDATE SET dedupe_key=EXCLUDED.dedupe_key RETURNING to_jsonb(external_operations)`, op, Str(r, "id"), "reply:"+id, JSON(target), JSON(req), id)
			if e != nil {
				return e
			}
			if _, e = tx.Exec(ctx, "UPDATE replies SET external_operation_id=$2,status='queued' WHERE id=$1", Str(r, "id"), Str(inserted, "id")); e != nil {
				return e
			}
			return enqueue(ctx, tx, "dispatch_external", "external:"+Str(inserted, "id"), Data{"operation_id": Str(inserted, "id")}, 0)
		case "step":
			st, e := locked(ctx, tx, "task_steps", Str(ap, "subject_id"))
			if e != nil {
				return e
			}
			t, e := locked(ctx, tx, "dev_tasks", Str(st, "task_id"))
			if e != nil {
				return e
			}
			input := Object(st, "input")
			if Hash(JSON(input)) != Str(action, "input_hash") {
				return Conflict
			}
			if Str(st, "status") == "succeeded" {
				return nil
			}
			if Str(st, "stage") == "publish_pr" {
				req := Data{"task_id": Str(t, "id"), "step_id": Str(st, "id"), "snapshot_id": Str(input, "snapshot_id"), "baseline_snapshot_id": Str(input, "baseline_snapshot_id"), "input_digest": Str(input, "input_digest"), "base_commit": Str(input, "base_commit"), "base_tree": Str(input, "base_tree"), "target_base_commit": fallback(Str(input, "target_base_commit"), Str(input, "base_commit")), "expected_branch_head": Str(input, "expected_branch_head"), "pr_number": Num(input, "pr_number"), "branch": "agentowl/" + Str(t, "id"), "title": "fix(auth): " + Str(t, "title"), "body": Str(t, "goal") + "\n\n验收条件\n" + Str(t, "acceptance_criteria") + "\n\n自测通过，代码快照：" + Str(input, "input_digest") + "\n\n审查摘要\n" + Str(input, "previous_summary"), "tests_passed": Bool(input, "tests_passed"), "epoch": Num(t, "epoch"), "revision": Num(t, "revision")}
				op, e := one(ctx, tx, `INSERT INTO external_operations(id,kind,subject_id,dedupe_key,target,request,approval_id) VALUES($1,'publish_pr',$2,$3,$4,$5,$6) ON CONFLICT(dedupe_key) DO UPDATE SET dedupe_key=EXCLUDED.dedupe_key RETURNING to_jsonb(external_operations)`, ID(), Str(t, "id"), "publish:"+id, JSON(Data{"repository_id": Str(t, "repository_id")}), JSON(req), id)
				if e != nil {
					return e
				}
				if e = taskFact(ctx, tx, Str(t, "id"), taskcore.Publishing, taskcore.Facts{}, nil); e != nil {
					return e
				}
				if _, e = tx.Exec(ctx, "UPDATE task_steps SET status='running',version=version+1,updated_at=now() WHERE id=$1", Str(st, "id")); e != nil {
					return e
				}
				return enqueue(ctx, tx, "dispatch_external", "external:"+Str(op, "id"), Data{"operation_id": Str(op, "id")}, 0)
			}
			if Str(st, "attempt_id") != "" {
				runID = Str(st, "attempt_id")
				return nil
			}
			files, e := s.readSnapshot(ctx, Str(input, "snapshot_id"))
			if e != nil {
				return e
			}
			runID = ID()
			spec := runner.RunSpec{RunID: runID, AttemptID: runID, TaskID: Str(t, "id"), StepID: Str(st, "id"), Epoch: Num(t, "epoch"), Stage: Str(st, "stage"), ActionDigest: Str(ap, "action_hash"), InputDigest: Str(input, "input_digest"), TreeSHA: Str(input, "base_tree"), Image: Str(input, "image"), Goal: Str(input, "goal"), Context: Str(input, "context"), ContextManifest: runner.ContextManifest{}, AllowedPaths: Strings(input, "allowed_paths"), Files: files, Limits: runner.DefaultLimits()}
			if items, ok := input["task_inputs"]; ok {
				if e = json.Unmarshal(JSON(items), &spec.TaskInputs); e != nil {
					return e
				}
				if len(spec.TaskInputs) > 0 {
					spec.TaskInputProtocol = 1
					spec.TaskInputDigest = Str(input, "input_set_digest")
				}
			}
			spec.ExecutionVersion = Num(input, "execution_settings_version")
			spec.ModelVersion = Num(input, "model_settings_version")
			spec.ResourceLimits = Str(input, "resource_limits")
			if err := json.Unmarshal(JSON(input["context_manifest"]), &spec.ContextManifest); err != nil {
				return err
			}
			if e = json.Unmarshal(JSON(input["execution_profile"]), &spec.ExecutionProfile); e != nil {
				return e
			}
			if spec.Stage == "test" {
				if e = json.Unmarshal(JSON(input["env"]), &spec.Env); e != nil {
					return e
				}
				spec.Argv = Strings(input, "argv")
			}
			if e = json.Unmarshal(JSON(input["limits"]), &spec.Limits); e != nil {
				return e
			}
			if e = s.reserveRunTokenBudget(ctx, tx, &spec); e != nil {
				return e
			}
			placement, err := s.pickRuntimeMachine(ctx, spec)
			if err != nil {
				return err
			}
			spec.RuntimeMachineID = Str(placement.machine, "id")
			spec.RuntimeMachineName = Str(placement.machine, "name")
			digest, e := runner.SpecDigest(spec)
			if e != nil {
				return e
			}
			spec.Files = nil
			if _, e = tx.Exec(ctx, "INSERT INTO run_attempts(id,task_id,step_id,epoch,stage,action_digest,spec_digest,spec,runtime_machine_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)", runID, spec.TaskID, spec.StepID, spec.Epoch, spec.Stage, spec.ActionDigest, digest, JSON(spec), nullIfBlank(spec.RuntimeMachineID)); e != nil {
				return e
			}
			if _, e = tx.Exec(ctx, "UPDATE task_steps SET status='running',attempt_id=$2,version=version+1,updated_at=now() WHERE id=$1", Str(st, "id"), runID); e != nil {
				return e
			}
			if e = taskFact(ctx, tx, Str(t, "id"), taskcore.Started, taskcore.Facts{}, nil); e != nil {
				return e
			}
			return enqueue(ctx, tx, "poll_run", "run:start:"+runID, Data{"run_id": runID}, 0)
		}
		return Forbidden
	})
	return e
}

func (s *Service) AuthorizeRun(ctx context.Context, p Data) (Data, error) {
	allowed := false
	reason := ""
	phase := Str(p, "phase")
	if !contains([]string{"admit", "prepare", "renew"}, phase) {
		return Data{"allowed": false, "reason": "invalid_phase"}, nil
	}
	e := s.transaction(ctx, func(tx pgx.Tx) error {
		// Match configuration saves and publication/preparation before any row
		// locks. A save either precedes this permit or observes an admitted run.
		if _, e := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(728690107)"); e != nil {
			return e
		}
		if _, e := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended('system_settings:model',0))"); e != nil {
			return e
		}
		r, e := locked(ctx, tx, "run_attempts", Str(p, "run_id"))
		if e != nil {
			return nil
		}
		if !contains([]string{"queued", "running", "collecting"}, Str(r, "status")) || Str(r, "id") != Str(p, "attempt_id") || Str(r, "task_id") != Str(p, "task_id") || Str(r, "step_id") != Str(p, "step_id") || Num(r, "epoch") != Num(p, "epoch") || Str(r, "action_digest") != Str(p, "action_digest") || Str(r, "spec_digest") != Str(p, "spec_digest") {
			return nil
		}
		var fresh bool
		if e = tx.QueryRow(ctx, "SELECT updated_at>now()-interval '40 seconds' FROM run_attempts WHERE id=$1", Str(r, "id")).Scan(&fresh); e != nil || !fresh {
			return e
		}
		st, e := locked(ctx, tx, "task_steps", Str(r, "step_id"))
		if e != nil {
			return e
		}
		if Str(st, "attempt_id") != Str(r, "id") {
			return nil
		}
		ap, e := locked(ctx, tx, "approvals", Str(st, "approval_id"))
		if e != nil {
			return e
		}
		if e = s.validateApproval(ctx, tx, ap, true); e != nil {
			return nil
		}
		if Str(r, "status") == "queued" {
			task, err := locked(ctx, tx, "dev_tasks", Str(r, "task_id"))
			if err != nil {
				return err
			}
			if err = s.taskInputGate(ctx, tx, task); err != nil {
				return nil
			}
		}
		if Str(r, "status") == "queued" {
			settings, err := readExecutionSettings(ctx, tx)
			if err != nil {
				return err
			}
			var modelVersion int64
			if err = tx.QueryRow(ctx, "SELECT COALESCE((SELECT version FROM system_settings WHERE name='model'),0)").Scan(&modelVersion); err != nil {
				return err
			}
			var spec runner.RunSpec
			if err = json.Unmarshal(JSON(r["spec"]), &spec); err != nil {
				return err
			}
			profile := runner.DefaultExecutionProfile()
			if spec.ExecutionProfile != nil {
				profile = *spec.ExecutionProfile
			}
			if spec.ExecutionVersion != Num(settings, "version") || spec.ModelVersion != modelVersion || spec.ResourceLimits != Str(settings, "resource_limits") || profile != executionProfile(settings) {
				reason = "settings_changed"
				return nil
			}
			if phase == "prepare" {
				// This committed permission is the admission boundary, before the
				// container starts. Polling must never downgrade it back to queued.
				if _, err = tx.Exec(ctx, "UPDATE run_attempts SET status='running',started_at=COALESCE(started_at,now()),version=version+1,updated_at=now() WHERE id=$1", Str(r, "id")); err != nil {
					return err
				}
			}
		}
		allowed = true
		return nil
	})
	return Data{"allowed": allowed, "reason": reason, "expires_at": time.Now().Add(25 * time.Second).UTC().Format(time.RFC3339)}, e
}
func (s *Service) runSpec(ctx context.Context, r Data) (runner.RunSpec, error) {
	var spec runner.RunSpec
	e := json.Unmarshal(JSON(r["spec"]), &spec)
	if e != nil {
		return spec, e
	}
	st, e := one(ctx, s.DB, "SELECT to_jsonb(st) FROM task_steps st WHERE id=$1", Str(r, "step_id"))
	if e != nil {
		return spec, e
	}
	spec.Files, e = s.readSnapshot(ctx, Str(Object(st, "input"), "snapshot_id"))
	if e != nil {
		return spec, e
	}
	digest, e := runner.SpecDigest(spec)
	if e != nil || digest != Str(r, "spec_digest") {
		return spec, Fail(409, "snapshot_changed", "运行快照校验失败")
	}
	return spec, nil
}
func (s *Service) pollRun(ctx context.Context, id string) error {
	r, e := one(ctx, s.DB, "SELECT to_jsonb(r) FROM run_attempts r WHERE id=$1", id)
	if e != nil {
		return e
	}
	runnerClient, _, err := s.runnerForAttempt(ctx, r)
	if err != nil {
		return err
	}
	if contains([]string{"succeeded", "failed", "stopped", "blocked"}, Str(r, "status")) {
		e = runnerClient.Acknowledge(ctx, id)
		if errors.Is(e, runner.ErrNotFound) {
			return nil
		}
		return e
	}
	var valid bool
	settled := false
	e = s.transaction(ctx, func(tx pgx.Tx) error {
		if e := lockExecutionSettings(ctx, tx); e != nil {
			return e
		}
		current, e := locked(ctx, tx, "run_attempts", id)
		if e != nil {
			return e
		}
		r = current
		if contains([]string{"succeeded", "failed", "stopped", "blocked"}, Str(r, "status")) {
			settled = true
			return nil
		}
		st, e := locked(ctx, tx, "task_steps", Str(r, "step_id"))
		if e != nil {
			return e
		}
		ap, e := locked(ctx, tx, "approvals", Str(st, "approval_id"))
		if e != nil {
			return e
		}
		if Str(st, "attempt_id") != id || Str(ap, "action_hash") != Str(r, "action_digest") {
			return nil
		}
		if e = s.validateApproval(ctx, tx, ap, true); e != nil {
			return nil
		}
		valid = true
		_, e = tx.Exec(ctx, "UPDATE run_attempts SET updated_at=now() WHERE id=$1 AND status IN ('queued','running','collecting','outcome_unknown')", id)
		return e
	})
	if e != nil {
		return e
	}
	if settled {
		e = runnerClient.Acknowledge(ctx, id)
		if errors.Is(e, runner.ErrNotFound) {
			return nil
		}
		return e
	}
	if !valid {
		return s.stopTask(ctx, Str(r, "task_id"))
	}
	spec, e := s.runSpec(ctx, r)
	if e != nil {
		return e
	}
	remote, e := runnerClient.Inspect(ctx, id)
	if errors.Is(e, runner.ErrNotFound) && Str(r, "status") == "queued" {
		remote, e = runnerClient.Start(ctx, spec)
		if errors.Is(e, runner.ErrConflict) {
			startErr := e
			return s.transaction(ctx, func(tx pgx.Tx) error {
				attempt, err := locked(ctx, tx, "run_attempts", id)
				if err != nil {
					return err
				}
				if Str(attempt, "status") != "queued" || Str(attempt, "spec_digest") != Str(r, "spec_digest") || Num(attempt, "epoch") != spec.Epoch {
					return startErr
				}
				// Hold the authorization row lock through the absence check and
				// revocation. A concurrent admission must obtain a new permit in
				// prepare before starting its container; it will see blocked here.
				if _, err = runnerClient.Inspect(ctx, id); !errors.Is(err, runner.ErrNotFound) {
					if err != nil {
						return err
					}
					return startErr
				}
				st, err := locked(ctx, tx, "task_steps", Str(attempt, "step_id"))
				if err != nil {
					return err
				}
				if _, err = locked(ctx, tx, "approvals", Str(st, "approval_id")); err != nil {
					return err
				}
				task, err := locked(ctx, tx, "dev_tasks", Str(attempt, "task_id"))
				if err != nil {
					return err
				}
				const reason = "Runner 未接受本次运行，请检查模型和执行设置后恢复任务并重新审批"
				if _, err = tx.Exec(ctx, "UPDATE run_attempts SET status='blocked',error=$2,finished_at=now(),version=version+1,updated_at=now() WHERE id=$1", id, reason); err != nil {
					return err
				}
				if _, err = tx.Exec(ctx, "UPDATE budget_reservations SET actual=0,status='settled' WHERE id=$1 AND status='reserved'", id); err != nil {
					return err
				}
				if _, err = tx.Exec(ctx, "INSERT INTO run_events(id,run_id,sequence,type,body) VALUES($1,$2,1,'blocked',$3) ON CONFLICT(run_id,sequence) DO NOTHING", ID(), id, reason); err != nil {
					return err
				}
				if Str(st, "attempt_id") != id {
					return nil
				}
				if _, err = tx.Exec(ctx, "UPDATE task_steps SET status='blocked',error=$2,version=version+1,updated_at=now() WHERE id=$1", Str(st, "id"), reason); err != nil {
					return err
				}
				if _, err = tx.Exec(ctx, "UPDATE approvals SET status='stale',version=version+1,updated_at=now() WHERE id::text=$1 AND action_hash=$2 AND status IN ('pending','approved')", Str(st, "approval_id"), Str(attempt, "action_digest")); err != nil {
					return err
				}
				if Num(task, "epoch") == spec.Epoch && !contains([]string{"paused", "cancelled", "completed"}, Str(task, "status")) {
					err = taskFact(ctx, tx, Str(task, "id"), taskcore.Block, taskcore.Facts{}, Data{"error": reason})
				}
				return err
			})
		}
	}
	if e != nil {
		return e
	}
	if remote.RunID != id || remote.TaskID != Str(r, "task_id") || remote.SpecDigest != Str(r, "spec_digest") || remote.Epoch != Num(r, "epoch") {
		return Fail(409, "runner_identity", "Runner 返回的运行身份不匹配")
	}
	if contains([]string{"queued", "running", "collecting"}, remote.State) {
		return s.transaction(ctx, func(tx pgx.Tx) error {
			if err := s.recordTaskInputReceipts(ctx, tx, spec, remote); err != nil {
				return err
			}
			updated, err := tx.Exec(ctx, `UPDATE run_attempts SET status=CASE
				WHEN status='running' AND $2='queued' THEN status
				WHEN status='collecting' AND $2 IN ('queued','running') THEN status
				ELSE $2 END,started_at=COALESCE(started_at,now()),updated_at=now()
				WHERE id=$1 AND status IN ('queued','running','collecting','outcome_unknown')`, id, remote.State)
			if err != nil {
				return err
			}
			// A concurrent poll may have revoked an admission while this Start
			// response was in flight. Never resurrect a terminal local attempt.
			if updated.RowsAffected() == 0 {
				return nil
			}
			return enqueue(ctx, tx, "poll_run", "poll:"+id+":"+ID(), Data{"run_id": id}, 10*time.Second)
		})
	}
	stream, e := runnerClient.Collect(ctx, id)
	if e != nil {
		return e
	}
	defer stream.Close()
	verified, files, diff, logs, e := decodeRunArchiveWithLogs(stream, spec)
	if e != nil {
		return e
	}
	if verified.State != remote.State {
		return Fail(409, "runner_changed", "Runner 结果状态与产物不一致")
	}
	e = s.finishRun(ctx, r, spec, verified, files, diff, logs)
	if e != nil {
		return e
	}
	return runnerClient.Acknowledge(ctx, id)
}
func (s *Service) finishRun(ctx context.Context, r Data, spec runner.RunSpec, remote runner.Run, files []runner.File, diff string, logs map[string][]byte) error {
	return s.transaction(ctx, func(tx pgx.Tx) error {
		if e := lockExecutionSettings(ctx, tx); e != nil {
			return e
		}
		attempt, e := locked(ctx, tx, "run_attempts", spec.RunID)
		if e != nil {
			return e
		}
		if contains([]string{"succeeded", "failed", "stopped", "blocked"}, Str(attempt, "status")) {
			return nil
		}
		if Str(attempt, "task_id") != spec.TaskID || Str(attempt, "step_id") != spec.StepID || Num(attempt, "epoch") != spec.Epoch || Str(attempt, "spec_digest") != Str(r, "spec_digest") {
			return Conflict
		}
		st, e := locked(ctx, tx, "task_steps", Str(r, "step_id"))
		if e != nil {
			return e
		}
		if contains([]string{"succeeded", "failed", "stopped"}, Str(st, "status")) {
			return nil
		}
		if _, e = locked(ctx, tx, "approvals", Str(st, "approval_id")); e != nil {
			return e
		}
		t, e := locked(ctx, tx, "dev_tasks", Str(r, "task_id"))
		if e != nil {
			return e
		}
		if e = s.recordTaskInputReceipts(ctx, tx, spec, remote); e != nil {
			return e
		}
		current := Num(t, "epoch") == spec.Epoch && Str(st, "attempt_id") == spec.RunID && !contains([]string{"paused", "cancelled", "blocked"}, Str(t, "status"))
		status := remote.State
		if !contains([]string{"succeeded", "failed", "stopped", "blocked", "outcome_unknown"}, status) {
			return Fail(422, "invalid_run_result", "未知运行结果")
		}
		if status == "succeeded" && (!remote.Result.EvidenceValid || remote.Result.InputDigest != spec.InputDigest) {
			status = "failed"
		}
		if spec.Stage != "edit" && mustSnapshotDigest(files) != spec.InputDigest {
			status = "failed"
		}
		if spec.Stage == "edit" {
			for _, path := range changedPaths(spec.Files, files) {
				if !pathAllowed(path, spec.AllowedPaths) {
					status = "failed"
				}
			}
		}
		if spec.Stage == "test" && (remote.Result.ExitCode == nil || *remote.Result.ExitCode != 0) {
			status = "failed"
		}
		if !current {
			status = "stopped"
		}
		snapshot, e := s.storeSnapshot(ctx, tx, spec.TaskID, Str(t, "case_id"), spec.RunID, files)
		if e != nil {
			return e
		}
		patch, e := s.storeArtifact(ctx, tx, spec.TaskID, Str(t, "case_id"), spec.RunID, "diff.patch", "text/plain", []byte(integration.Redact(diff)))
		if e != nil {
			return e
		}
		result, e := s.storeArtifact(ctx, tx, spec.TaskID, Str(t, "case_id"), spec.RunID, "result.json", "application/json", JSON(remote))
		if e != nil {
			return e
		}
		input := Object(st, "input")
		out := Data{"snapshot_id": Str(snapshot, "id"), "diff_artifact_id": Str(patch, "id"), "result_artifact_id": Str(result, "id"), "summary": integration.Redact(remote.Result.Summary), "output_digest": mustSnapshotDigest(files), "tests_passed": Bool(input, "tests_passed"), "test_digest": Str(input, "test_digest")}
		for name, body := range logs {
			artifact, e := s.storeArtifact(ctx, tx, spec.TaskID, Str(t, "case_id"), spec.RunID, name, "text/plain", []byte(integration.Redact(string(body))))
			if e != nil {
				return e
			}
			key := "test_log_artifact_id"
			if name == "tools.ndjson" {
				key = "tools_artifact_id"
			}
			out[key] = Str(artifact, "id")
		}
		if spec.Stage == "test" {
			out["tests_passed"] = status == "succeeded"
			out["test_digest"] = spec.InputDigest
		}
		if _, e = tx.Exec(ctx, "UPDATE run_attempts SET status=$2,result=$3,error=$4,finished_at=now(),version=version+1,updated_at=now() WHERE id=$1", spec.RunID, status, JSON(remote.Result), integration.Redact(remote.Result.Error)); e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, "UPDATE task_steps SET status=$2,output=$3,error=$4,version=version+1,updated_at=now() WHERE id=$1", spec.StepID, status, JSON(out), integration.Redact(remote.Result.Error)); e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, "UPDATE budget_reservations SET actual=$2,status='settled' WHERE id=$1", spec.RunID, remote.Result.Tokens); e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, "INSERT INTO run_events(id,run_id,sequence,type,body,metadata) VALUES($1,$2,1,$3,$4,$5) ON CONFLICT(run_id,sequence) DO NOTHING", ID(), spec.RunID, status, integration.Redact(remote.Result.Summary), JSON(out)); e != nil {
			return e
		}
		if !current {
			return nil
		}
		if _, e = tx.Exec(ctx, "UPDATE approvals SET status='executed',version=version+1,updated_at=now() WHERE id=$1 AND status='approved'", Str(st, "approval_id")); e != nil {
			return e
		}
		if status != "succeeded" {
			e = taskFact(ctx, tx, spec.TaskID, taskcore.Block, taskcore.Facts{}, Data{"error": "阶段 " + spec.Stage + " 未通过；请查看运行结果后恢复或修改任务"})
			return e
		}
		next := stages[indexStage(spec.Stage)+1]
		if e = transitionTask(ctx, tx, t, taskcore.Advance, taskcore.Facts{Revision: Num(t, "revision"), Epoch: spec.Epoch, NextStage: next}, "system:runner"); e != nil {
			return e
		}
		return enqueue(ctx, tx, "prepare_step", fmt.Sprintf("next:%s:%d:%d:%s", spec.TaskID, Num(t, "revision"), spec.Epoch, next), Data{"task_id": spec.TaskID, "revision": Num(t, "revision"), "epoch": spec.Epoch, "stage": next}, 0)
	})
}
func (s *Service) stopTask(ctx context.Context, id string) error {
	runs, e := many(ctx, s.DB, "SELECT to_jsonb(r) FROM run_attempts r WHERE task_id=$1 AND status IN ('queued','running','collecting','outcome_unknown')", id)
	if e != nil {
		return e
	}
	for _, r := range runs {
		runnerClient, _, err := s.runnerForAttempt(ctx, r)
		if err != nil {
			return err
		}
		remote, e := runnerClient.Stop(ctx, Str(r, "id"))
		if e != nil && !errors.Is(e, runner.ErrNotFound) {
			return e
		}
		if e == nil && contains([]string{"running", "queued", "collecting", "outcome_unknown"}, remote.State) {
			return errors.New("runner has not stopped")
		}
		e = s.transaction(ctx, func(tx pgx.Tx) error {
			_, e := tx.Exec(ctx, "UPDATE run_attempts SET status='stopped',finished_at=now(),version=version+1,updated_at=now() WHERE id=$1", Str(r, "id"))
			return e
		})
		if e != nil {
			return e
		}
		if remote.RunID != "" {
			_ = runnerClient.Acknowledge(ctx, remote.RunID)
		}
	}
	return nil
}
