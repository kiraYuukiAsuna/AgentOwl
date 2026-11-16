package service

import (
	"agentowl/backend/internal/taskcore"
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"agentowl/backend/internal/integration"
	"agentowl/backend/internal/runner"
	"github.com/jackc/pgx/v5"
)

// An operation is claimed once. A process interruption after the claim is an
// unknown remote outcome, never permission to repeat a write.
func (s *Service) dispatchExternal(ctx context.Context, id string) error {
	meta, err := one(ctx, s.DB, "SELECT jsonb_build_object('kind',kind) FROM external_operations WHERE id=$1", id)
	if err != nil {
		return err
	}
	var op Data
	claimed := false
	err = s.transaction(ctx, func(tx pgx.Tx) error {
		if Str(meta, "kind") == "publish_pr" {
			// Claim publication under the same settings locks used by preparation
			// and saves, before taking operation or approval row locks.
			if _, e := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(728690107)"); e != nil {
				return e
			}
			if _, e := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended('system_settings:model',0))"); e != nil {
				return e
			}
		}
		var e error
		op, e = locked(ctx, tx, "external_operations", id)
		if e != nil {
			return e
		}
		if Str(op, "status") != "pending" {
			return nil
		}
		allowed, err := s.approvalNotificationAllowed(ctx, tx, op)
		if err != nil {
			return err
		}
		if !allowed {
			_, err = tx.Exec(ctx, "UPDATE external_operations SET status='cancelled',error='审批通知已关闭或不再适用',version=version+1,updated_at=now() WHERE id=$1", id)
			return err
		}
		if e = s.validateExternal(ctx, tx, op); e != nil {
			return e
		}
		op, e = one(ctx, tx, "UPDATE external_operations SET status='dispatching',version=version+1,updated_at=now() WHERE id=$1 AND status='pending' AND version=$2 RETURNING to_jsonb(external_operations)", id, Num(op, "version"))
		claimed = e == nil
		return e
	})
	if err != nil {
		return err
	}
	if !claimed {
		if contains([]string{"dispatching", "outcome_unknown"}, Str(op, "status")) {
			return s.reconcileExternal(ctx, id)
		}
		return nil
	}
	if Str(op, "kind") == "publish_pr" {
		return s.publishPR(ctx, op)
	}
	result, err := s.DispatchMessage(ctx, op)
	if err != nil {
		return s.externalFailure(ctx, op, err)
	}
	return s.finishExternal(ctx, op, "succeeded", result, "")
}

func (s *Service) validateExternal(ctx context.Context, q DB, op Data) error {
	if err := guardJob(ctx, q); err != nil {
		return err
	}
	if approvalID := Str(op, "approval_id"); approvalID != "" {
		ap, err := locked(ctx, q, "approvals", approvalID)
		if err != nil {
			return err
		}
		if err = s.validateApproval(ctx, q, ap, true); err != nil {
			return err
		}
		if Str(op, "kind") == "publish_pr" {
			req := Object(op, "request")
			t, err := locked(ctx, q, "dev_tasks", Str(req, "task_id"))
			if err != nil {
				return err
			}
			st, err := locked(ctx, q, "task_steps", Str(req, "step_id"))
			if err != nil {
				return err
			}
			if Str(t, "id") != Str(op, "subject_id") || Num(t, "epoch") != Num(req, "epoch") || Num(t, "revision") != Num(req, "revision") || Str(t, "status") != "publishing" || Str(st, "approval_id") != approvalID || Str(st, "task_id") != Str(t, "id") || Str(st, "stage") != "publish_pr" || Str(st, "status") != "running" {
				return Conflict
			}
			if err = s.taskInputGate(ctx, q, t); err != nil {
				return err
			}
			input := Object(st, "input")
			for _, key := range []string{"snapshot_id", "baseline_snapshot_id", "input_digest", "base_commit", "base_tree", "target_base_commit", "expected_branch_head"} {
				if Str(req, key) != Str(input, key) {
					return Conflict
				}
			}
			if Num(req, "pr_number") != Num(input, "pr_number") {
				return Conflict
			}
			if !Bool(req, "tests_passed") || !Bool(input, "tests_passed") || Str(req, "branch") != "agentowl/"+Str(t, "id") || Str(Object(op, "target"), "repository_id") != Str(t, "repository_id") {
				return Conflict
			}
		} else if Str(op, "kind") == "reply_send" {
			if Str(ap, "subject_type") != "reply" || Str(ap, "subject_id") != Str(op, "subject_id") || Str(Object(ap, "action"), "body") != Str(Object(op, "request"), "body") {
				return Conflict
			}
		}
	} else if Str(op, "kind") != "notify" && Str(op, "kind") != "read_mark" {
		return Forbidden
	}
	return nil
}

// checkpoint commits the exact material needed to identify an ambiguous effect.
// It also revalidates the approval and current execution fence before each write.
func (s *Service) externalCheckpoint(ctx context.Context, op Data, patch Data) error {
	return s.transaction(ctx, func(tx pgx.Tx) error {
		current, e := locked(ctx, tx, "external_operations", Str(op, "id"))
		if e != nil {
			return e
		}
		if Str(current, "status") != "dispatching" || Num(current, "version") != Num(op, "version") {
			return Conflict
		}
		if e = s.validateExternal(ctx, tx, current); e != nil {
			return e
		}
		response := Object(current, "response")
		for k, v := range patch {
			response[k] = v
		}
		updated, e := one(ctx, tx, "UPDATE external_operations SET response=$2,version=version+1,updated_at=now() WHERE id=$1 AND version=$3 RETURNING to_jsonb(external_operations)", Str(op, "id"), JSON(response), Num(op, "version"))
		if e != nil {
			return e
		}
		for k, v := range updated {
			op[k] = v
		}
		return nil
	})
}

func retryableExternalError(cause error) bool {
	if errors.Is(cause, integration.ErrInvalidInput) || errors.Is(cause, integration.ErrUnsupportedOperation) {
		return false
	}
	var local *Error
	if errors.As(cause, &local) {
		return false
	}
	var api *integration.APIError
	if errors.As(cause, &api) {
		return api.StatusCode == 0 || api.StatusCode == 408 || api.StatusCode == 429 || api.StatusCode >= 500 || api.OutcomeUnknown
	}
	return true
}

func (s *Service) retryExternal(ctx context.Context, op Data, cause error) (bool, error) {
	if !contains([]string{"reply_send", "notify"}, Str(op, "kind")) || !retryableExternalError(cause) {
		return false, nil
	}
	settings, err := s.retrySettings(ctx, s.DB)
	if err != nil {
		return false, err
	}
	if Num(settings, "external_attempts") <= 0 || Num(op, "retry_attempts") >= Num(settings, "external_attempts") {
		return false, nil
	}
	delay := time.Duration(Num(settings, "external_delay_seconds")) * time.Second
	var api *integration.APIError
	if errors.As(cause, &api) && api.RetryAfter > delay {
		delay = api.RetryAfter
	}
	nextAttempt := Num(op, "retry_attempts") + 1
	return true, s.transaction(ctx, func(tx pgx.Tx) error {
		current, e := locked(ctx, tx, "external_operations", Str(op, "id"))
		if e != nil {
			return e
		}
		if Num(current, "version") != Num(op, "version") || Str(current, "status") != "dispatching" {
			return Conflict
		}
		updated, e := one(ctx, tx, "UPDATE external_operations SET status='pending',retry_attempts=retry_attempts+1,error=$2,version=version+1,updated_at=now() WHERE id=$1 AND status='dispatching' AND version=$3 RETURNING to_jsonb(external_operations)", Str(op, "id"), safeError(cause), Num(op, "version"))
		if e != nil {
			return e
		}
		if e = enqueue(ctx, tx, "dispatch_external", fmt.Sprintf("external-retry:%s:%d", Str(op, "id"), nextAttempt), Data{"operation_id": Str(op, "id")}, delay); e != nil {
			return e
		}
		return audit(ctx, tx, Actor{ID: "system:worker"}, "external_retry", "external_operations", Str(op, "id"), fmt.Sprintf("第 %d 次重试：%s", Num(updated, "retry_attempts"), safeError(cause)))
	})
}

func (s *Service) externalFailure(ctx context.Context, op Data, cause error) error {
	if retried, err := s.retryExternal(ctx, op, cause); err != nil || retried {
		return err
	}
	status := "outcome_unknown"
	var api *integration.APIError
	var local *Error
	// Only an explicit rejected request proves no effect. Context/transport errors
	// and unclassified errors conservatively remain unknown.
	if errors.As(cause, &api) && !api.OutcomeUnknown && api.StatusCode >= 400 && api.StatusCode < 500 {
		status = "failed"
	}
	if errors.As(cause, &local) {
		status = "failed"
	}
	if errors.Is(cause, integration.ErrInvalidInput) || errors.Is(cause, integration.ErrUnsupportedOperation) {
		status = "failed"
	}
	return s.finishExternal(ctx, op, status, Data{}, safeError(cause))
}

func (s *Service) finishExternal(ctx context.Context, op Data, status string, result Data, message string) error {
	return s.transaction(ctx, func(tx pgx.Tx) error {
		current, e := locked(ctx, tx, "external_operations", Str(op, "id"))
		if e != nil {
			return e
		}
		if Num(current, "version") != Num(op, "version") || !contains([]string{"dispatching", "outcome_unknown"}, Str(current, "status")) {
			return Conflict
		}
		response := Object(current, "response")
		for k, v := range Object(result, "response") {
			response[k] = v
		}
		// Record observed remote facts even if approval was withdrawn in flight;
		// only a still-current execution may advance business state.
		advance := s.validateExternal(ctx, tx, current) == nil
		_, e = tx.Exec(ctx, "UPDATE external_operations SET status=$2,response=$3,remote_id=$4,error=$5,version=version+1,updated_at=now() WHERE id=$1 AND version=$6", Str(op, "id"), status, JSON(response), fallback(Str(result, "remote_id"), Str(current, "remote_id")), message, Num(op, "version"))
		if e != nil {
			return e
		}
		req := Object(current, "request")
		if Str(current, "kind") == "reply_send" {
			if e = s.enqueueReplyOutcome(ctx, tx, Str(current, "approval_id"), status, message); e != nil {
				return e
			}
		}
		if Str(current, "kind") == "notify" && status != "succeeded" && Str(req, "case_id") != "" {
			eventType, body := "notification_failed", "通知发送失败"
			if status == "outcome_unknown" {
				eventType, body = "notification_outcome_unknown", "通知结果待核对"
			}
			if strings.TrimSpace(message) != "" {
				body += "：" + boundedText(message, 1000)
			}
			if e = caseEvent(ctx, tx, Str(req, "case_id"), eventType, "system", body, Data{"operation_id": Str(current, "id"), "status": status, "error": message}); e != nil {
				return e
			}
		}
		if status == "outcome_unknown" {
			return enqueue(ctx, tx, "reconcile_external", fmt.Sprintf("reconcile:%s:%d", Str(op, "id"), Num(op, "version")), Data{"operation_id": Str(op, "id")}, 30*time.Second)
		}
		if !advance {
			return audit(ctx, tx, Actor{ID: "system:worker"}, "remote_effect_after_fence_change", "external_operations", Str(op, "id"), status)
		}
		if Str(op, "kind") == "reply_send" {
			replyStatus := "failed"
			if status == "succeeded" {
				replyStatus = "sent"
			}
			if _, e = tx.Exec(ctx, "UPDATE replies SET status=$2,version=version+1,updated_at=now() WHERE id=$1", Str(req, "reply_id"), replyStatus); e != nil {
				return e
			}
			if status == "succeeded" {
				if e = caseEvent(ctx, tx, Str(req, "case_id"), "reply_sent", "system", "机器人回复已发送", Data{"operation_id": Str(op, "id")}); e != nil {
					return e
				}
			}
		}
		if Str(op, "kind") == "publish_pr" {
			stepStatus, taskStatus := "failed", "blocked"
			if status == "succeeded" {
				stepStatus, taskStatus = "succeeded", "pr_open"
			}
			if _, e = tx.Exec(ctx, "UPDATE task_steps SET status=$2,output=$3,error=$4,version=version+1,updated_at=now() WHERE id=$1", Str(req, "step_id"), stepStatus, JSON(response), message); e != nil {
				return e
			}
			if e = taskFact(ctx, tx, Str(req, "task_id"), taskcore.Published, taskcore.Facts{Status: taskStatus, Epoch: Num(req, "epoch"), Revision: Num(req, "revision")}, Data{"head_commit": Str(response, "commit_sha"), "pr_url": Str(response, "pr_url"), "error": message}); e != nil {
				return e
			}
		}
		if status == "succeeded" && Str(op, "approval_id") != "" {
			_, e = tx.Exec(ctx, "UPDATE approvals SET status='executed',version=version+1,updated_at=now() WHERE id=$1 AND status='approved'", Str(op, "approval_id"))
			if e != nil {
				return e
			}
		}
		return audit(ctx, tx, Actor{ID: "system:worker"}, "external_"+status, "external_operations", Str(op, "id"), message)
	})
}

type publicationGit interface {
	CreateBlob(context.Context, []byte) (integration.GitObject, error)
	CreateTree(context.Context, string, []integration.GitTreeEntry) (integration.GitTree, error)
}

// Snapshot manifests, not a model-provided patch, are authoritative. Binary files,
// executable bits, additions and deletions all participate in the Git tree.
func createPublicationTree(ctx context.Context, gh publicationGit, baseTree string, before, after []runner.File, beforeWrite func() error) (string, error) {
	if _, e := runner.SnapshotDigest(before); e != nil {
		return "", e
	}
	if _, e := runner.SnapshotDigest(after); e != nil {
		return "", e
	}
	old := map[string]runner.File{}
	next := map[string]runner.File{}
	for _, f := range before {
		old[f.Path] = f
	}
	for _, f := range after {
		next[f.Path] = f
	}
	paths := []string{}
	for p := range old {
		paths = append(paths, p)
	}
	for p := range next {
		if _, ok := old[p]; !ok {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	entries := []integration.GitTreeEntry{}
	for _, p := range paths {
		a, aok := old[p]
		b, bok := next[p]
		if aok && bok && a.Mode == b.Mode && bytes.Equal(a.Data, b.Data) {
			continue
		}
		mode := "100644"
		if bok && b.Mode == 0755 || !bok && a.Mode == 0755 {
			mode = "100755"
		}
		entry := integration.GitTreeEntry{Path: p, Type: "blob", Mode: mode, Delete: !bok}
		if bok {
			if e := beforeWrite(); e != nil {
				return "", e
			}
			blob, e := gh.CreateBlob(ctx, b.Data)
			if e != nil {
				return "", e
			}
			entry.SHA = blob.SHA
		}
		entries = append(entries, entry)
	}
	if len(entries) == 0 {
		return "", Fail(409, "empty_diff", "发布快照没有代码变更")
	}
	if e := beforeWrite(); e != nil {
		return "", e
	}
	tree, e := gh.CreateTree(ctx, baseTree, entries)
	return tree.SHA, e
}

func (s *Service) publishPR(ctx context.Context, op Data) error {
	req := Object(op, "request")
	gh, repo, err := s.repoGitHub(ctx, s.DB, Str(Object(op, "target"), "repository_id"))
	if err != nil {
		return s.externalFailure(ctx, op, err)
	}
	before, err := s.readSnapshot(ctx, Str(req, "baseline_snapshot_id"))
	if err != nil {
		return s.externalFailure(ctx, op, err)
	}
	after, err := s.readSnapshot(ctx, Str(req, "snapshot_id"))
	if err != nil {
		return s.externalFailure(ctx, op, err)
	}
	digest, err := runner.SnapshotDigest(after)
	if err != nil || digest != Str(req, "input_digest") {
		return s.externalFailure(ctx, op, Conflict)
	}
	base, err := gh.GetRef(ctx, "heads/"+Str(repo, "base_branch"))
	if err != nil {
		return s.externalFailure(ctx, op, err)
	}
	targetBase := fallback(Str(req, "target_base_commit"), Str(req, "base_commit"))
	if base.Object.SHA != targetBase {
		return s.externalFailure(ctx, op, Fail(409, "baseline_changed", "目标分支已更新，需要重新生成证据和审批"))
	}
	commit, err := gh.GetCommit(ctx, Str(req, "base_commit"))
	if err != nil {
		return s.externalFailure(ctx, op, err)
	}
	if commit.Tree.SHA != Str(req, "base_tree") {
		return s.externalFailure(ctx, op, Conflict)
	}
	checkpoint := func(p Data) error { return s.externalCheckpoint(ctx, op, p) }
	tree, err := createPublicationTree(ctx, gh, Str(req, "base_tree"), before, after, func() error { return checkpoint(Data{"phase": "git_objects"}) })
	if err != nil {
		return s.externalFailure(ctx, op, err)
	}
	if err = checkpoint(Data{"phase": "commit", "tree_sha": tree}); err != nil {
		return err
	}
	marker, _ := integration.OperationMarker(Str(op, "id"))
	commit, err = gh.CreateCommit(ctx, Str(req, "title")+"\n\n"+marker, tree, Str(req, "base_commit"))
	if err != nil {
		return s.externalFailure(ctx, op, err)
	}
	if err = checkpoint(Data{"phase": "branch", "commit_sha": commit.SHA, "branch": Str(req, "branch"), "base_branch": Str(repo, "base_branch")}); err != nil {
		return err
	}
	priorMarker := ""
	if Str(req, "expected_branch_head") != "" {
		previous, e := one(ctx, s.DB, "SELECT to_jsonb(o) FROM external_operations o WHERE kind='publish_pr' AND subject_id=$1 AND status IN ('succeeded','failed') AND ((remote_id=$2 AND $2<>'0') OR ($2='0' AND response->>'branch_created'='true' AND response->>'commit_sha'=$3)) ORDER BY updated_at DESC LIMIT 1", Str(req, "task_id"), strconv.FormatInt(Num(req, "pr_number"), 10), Str(req, "expected_branch_head"))
		if e != nil {
			return s.externalFailure(ctx, op, Fail(409, "pr_ownership_unverified", "无法核实该 PR 属于当前开发任务"))
		}
		if Str(Object(previous, "target"), "repository_id") != Str(Object(op, "target"), "repository_id") {
			return s.externalFailure(ctx, op, Conflict)
		}
		priorMarker, _ = integration.OperationMarker(Str(previous, "id"))
		if Num(req, "pr_number") == 0 {
			baseline, e := gh.GetCommit(ctx, Str(req, "expected_branch_head"))
			if e != nil {
				return s.externalFailure(ctx, op, e)
			}
			if !strings.Contains(baseline.Message, priorMarker) {
				return s.externalFailure(ctx, op, Fail(409, "branch_ownership_unverified", "分支提交不包含本任务的已确认发布标记"))
			}
		}
	}
	pr, err := publishReferenceAndPR(ctx, gh, req, Str(repo, "base_branch"), targetBase, commit.SHA, marker, priorMarker, checkpoint)
	if err != nil {
		return s.externalFailure(ctx, op, err)
	}
	if !publicationMatches(pr, Str(req, "branch"), Str(repo, "base_branch"), commit.SHA, marker) {
		return s.externalFailure(ctx, op, &integration.APIError{Provider: "github", Code: "publication_identity_mismatch", OutcomeUnknown: true})
	}
	return s.finishExternal(ctx, op, "succeeded", publicationResult(pr, commit.SHA), "")
}

type publicationReferenceGit interface {
	GetRef(context.Context, string) (integration.GitRef, error)
	CreateRef(context.Context, string, string) (integration.GitRef, error)
	UpdateRef(context.Context, string, string) (integration.GitRef, error)
	GetPullRequest(context.Context, int) (integration.GitHubPullRequest, error)
	CreatePullRequest(context.Context, integration.CreatePullRequestRequest) (integration.GitHubPullRequest, error)
	UpdatePullRequest(context.Context, int, integration.UpdatePullRequestRequest) (integration.GitHubPullRequest, error)
}

// GitHub's REST ref update is fast-forward-only, not an expected-SHA CAS. We
// recheck the approved head before sending and never force. Divergent concurrent
// updates are rejected by GitHub; an ambiguous response is reconciled read-only.
func publishReferenceAndPR(ctx context.Context, gh publicationReferenceGit, req Data, baseBranch, targetBase, commitSHA, marker, priorMarker string, checkpoint func(Data) error) (integration.GitHubPullRequest, error) {
	var empty integration.GitHubPullRequest
	branch, expected := Str(req, "branch"), Str(req, "expected_branch_head")
	number := int(Num(req, "pr_number"))
	updating := expected != ""
	if updating && (expected != Str(req, "base_commit") || priorMarker == "") {
		return empty, Conflict
	}
	if number > 0 && !updating {
		return empty, Conflict
	}
	if number > 0 {
		pr, err := gh.GetPullRequest(ctx, number)
		if err != nil {
			return empty, err
		}
		if pr.State != "open" || pr.Merged || !publicationMatches(pr, branch, baseBranch, expected, priorMarker) {
			return empty, Fail(409, "pr_changed", "PR 状态或分支已变化，需要重新确认开发修订")
		}
	}
	base, err := gh.GetRef(ctx, "heads/"+baseBranch)
	if err != nil {
		return empty, err
	}
	if base.Object.SHA != targetBase {
		return empty, Fail(409, "baseline_changed", "目标分支已更新，需要重新生成证据和审批")
	}
	ref, err := gh.GetRef(ctx, "heads/"+branch)
	if updating {
		if err != nil {
			return empty, err
		}
		if ref.Object.SHA != expected {
			return empty, Fail(409, "branch_changed", "PR 分支已被并发修改，需要重新冻结当前代码证据")
		}
		if err = checkpoint(Data{"phase": "branch_update", "expected_branch_head": expected, "pr_number": number}); err != nil {
			return empty, err
		}
		if _, err = gh.UpdateRef(ctx, "heads/"+branch, commitSHA); err != nil {
			return empty, err
		}
		phase := "pr_update"
		if number == 0 {
			phase = "pr_create"
		}
		if err = checkpoint(Data{"phase": phase, "branch_updated": true, "branch_created": true}); err != nil {
			return empty, err
		}
		title, body := Str(req, "title"), Str(req, "body")+"\n\n"+priorMarker+"\n"+marker
		if number == 0 {
			return gh.CreatePullRequest(ctx, integration.CreatePullRequestRequest{Title: title, Body: body, Head: branch, Base: baseBranch, Draft: true, MaintainerCanModify: false})
		}
		return gh.UpdatePullRequest(ctx, number, integration.UpdatePullRequestRequest{Title: &title, Body: &body})
	}
	if err == nil {
		return empty, Fail(409, "branch_exists", "任务分支已存在，需要核对远端结果")
	}
	var api *integration.APIError
	if !errors.As(err, &api) || api.StatusCode != 404 {
		return empty, err
	}
	if err = checkpoint(Data{"phase": "branch_create"}); err != nil {
		return empty, err
	}
	if _, err = gh.CreateRef(ctx, "heads/"+branch, commitSHA); err != nil {
		return empty, err
	}
	if err = checkpoint(Data{"phase": "pr_create", "branch_created": true}); err != nil {
		return empty, err
	}
	return gh.CreatePullRequest(ctx, integration.CreatePullRequestRequest{Title: Str(req, "title"), Body: Str(req, "body") + "\n\n" + marker, Head: branch, Base: baseBranch, Draft: true, MaintainerCanModify: false})
}

func publicationMatches(pr integration.GitHubPullRequest, branch, base, sha, marker string) bool {
	return pr.Number > 0 && pr.ID > 0 && pr.Head.Ref == branch && pr.Base.Ref == base && pr.Head.SHA == sha && strings.Contains(pr.Body, marker) && strings.HasPrefix(pr.HTMLURL, "https://")
}
func publicationResult(pr integration.GitHubPullRequest, sha string) Data {
	return Data{"remote_id": strconv.Itoa(pr.Number), "response": Data{"pr_number": pr.Number, "pr_url": pr.HTMLURL, "commit_sha": sha, "pr_state": pr.State, "merged": pr.Merged}}
}

func (s *Service) reconcileExternal(ctx context.Context, id string) error {
	op, err := one(ctx, s.DB, "SELECT to_jsonb(o) FROM external_operations o WHERE id=$1", id)
	if err != nil {
		return err
	}
	if !contains([]string{"dispatching", "outcome_unknown"}, Str(op, "status")) {
		return nil
	}
	if Str(op, "kind") != "publish_pr" {
		result, found, e := s.ReconcileMessage(ctx, op)
		if e != nil {
			return e
		}
		if found {
			return s.finishExternal(ctx, op, "succeeded", result, "")
		}
		return s.markExternalUnknown(ctx, op)
	}
	req, response := Object(op, "request"), Object(op, "response")
	gh, repo, err := s.repoGitHub(ctx, s.DB, Str(Object(op, "target"), "repository_id"))
	if err != nil {
		return err
	}
	sha := Str(response, "commit_sha")
	if sha == "" {
		return s.markExternalUnknown(ctx, op)
	}
	base := fallback(Str(response, "base_branch"), Str(repo, "base_branch"))
	pr, err := gh.LookupPullRequest(ctx, Str(repo, "owner")+":"+Str(req, "branch"), base, id, 20)
	if err != nil {
		return err
	}
	marker, _ := integration.OperationMarker(id)
	if pr != nil && publicationMatches(*pr, Str(req, "branch"), base, sha, marker) {
		return s.finishExternal(ctx, op, "succeeded", publicationResult(*pr, sha), "")
	}
	return s.markExternalUnknown(ctx, op)
}

func (s *Service) markExternalUnknown(ctx context.Context, op Data) error {
	if Str(op, "status") == "outcome_unknown" {
		return nil
	}
	return s.finishExternal(ctx, op, "outcome_unknown", Data{}, "远端结果无法确认；仅查询核对，禁止自动重发")
}

func (s *Service) trackPR(ctx context.Context, taskID string) error {
	task, err := one(ctx, s.DB, "SELECT to_jsonb(t) FROM dev_tasks t WHERE id=$1", taskID)
	if err != nil {
		return err
	}
	if Str(task, "status") != "pr_open" {
		return nil
	}
	op, err := one(ctx, s.DB, "SELECT to_jsonb(o) FROM external_operations o WHERE kind='publish_pr' AND subject_id=$1 AND status='succeeded' ORDER BY updated_at DESC LIMIT 1", taskID)
	if err != nil {
		return err
	}
	gh, repo, err := s.repoGitHub(ctx, s.DB, Str(task, "repository_id"))
	if err != nil {
		return err
	}
	if prPollInterval(repo) == 0 {
		return nil
	}
	number := int(Num(Object(op, "response"), "pr_number"))
	pr, err := gh.GetPullRequest(ctx, number)
	if err != nil {
		return err
	}
	tracking := Data{"head_sha": pr.Head.SHA, "state": pr.State, "merged": pr.Merged, "merge_commit_sha": pr.MergeCommitSHA, "release_verified": false}
	ci, ciErr := gh.GetCombinedStatus(ctx, pr.Head.SHA)
	if ciErr == nil && ci.SHA == pr.Head.SHA {
		tracking["ci"] = ci
	} else {
		tracking["ci_error"] = "无法核实当前 HEAD 的提交状态"
	}
	checks := []integration.GitHubCheckRun{}
	checksComplete := false
	for page := 1; page <= 20; page++ {
		items, e := gh.ListCheckRuns(ctx, pr.Head.SHA, page)
		if e != nil {
			tracking["checks_error"] = "无法读取全部检查结果"
			break
		}
		valid := true
		for _, item := range items.CheckRuns {
			if item.HeadSHA != pr.Head.SHA {
				valid = false
				break
			}
		}
		if !valid {
			tracking["checks_error"] = "检查结果与当前 HEAD 不一致"
			break
		}
		checks = append(checks, items.CheckRuns...)
		if len(checks) >= items.TotalCount {
			checksComplete = true
			break
		}
		if len(items.CheckRuns) == 0 {
			break
		}
	}
	tracking["checks"], tracking["checks_complete"] = checks, checksComplete
	reviews := []integration.GitHubReview{}
	reviewsComplete := false
	for page := 1; page <= 20; page++ {
		items, e := gh.ListPullRequestReviews(ctx, number, page)
		if e != nil {
			tracking["reviews_error"] = "无法读取全部审查记录"
			break
		}
		reviews = append(reviews, items...)
		if len(items) < 100 {
			reviewsComplete = true
			break
		}
	}
	tracking["reviews"], tracking["reviews_complete"] = reviews, reviewsComplete
	return s.transaction(ctx, func(tx pgx.Tx) error {
		current, e := locked(ctx, tx, "dev_tasks", taskID)
		if e != nil {
			return e
		}
		if Str(current, "status") != "pr_open" || Num(current, "epoch") != Num(task, "epoch") || Num(current, "revision") != Num(task, "revision") {
			return nil
		}
		step, e := locked(ctx, tx, "task_steps", Str(Object(op, "request"), "step_id"))
		if e != nil {
			return e
		}
		output := Object(step, "output")
		previous := Object(output, "pr_tracking")
		changed := prTrackingKey(previous) != prTrackingKey(decodeData(tracking))
		if Hash(JSON(output["pr_tracking"])) != Hash(JSON(tracking)) {
			output["pr_tracking"] = tracking
			if _, e = tx.Exec(ctx, "UPDATE task_steps SET output=$2,version=version+1,updated_at=now() WHERE id=$1", Str(step, "id"), JSON(output)); e != nil {
				return e
			}
			ciState := ci.State
			if ciErr != nil || ci.SHA != pr.Head.SHA {
				ciState = "unknown"
			}
			if changed {
				if e = caseEvent(ctx, tx, Str(current, "case_id"), "pr_status_synced", "system", fmt.Sprintf("PR 状态：%s；CI：%s；检查 %d 项；审查 %d 条。发布状态仍需人工确认。", pr.State, ciState, len(checks), len(reviews)), tracking); e != nil {
					return e
				}
			}
		}
		status := "pr_open"
		message := ""
		if pr.Head.SHA != Str(current, "head_commit") {
			status = "blocked"
			message = "PR HEAD 已被外部修改，现有自测证据不再覆盖当前代码"
		} else if pr.Merged {
			status = "completed"
			message = "PR 已合并，开发任务已完成"
			if _, e = tx.Exec(ctx, "UPDATE cases SET status='awaiting_release',version=version+1,updated_at=now() WHERE id=$1 AND status NOT IN ('resolved','closed')", Str(current, "case_id")); e != nil {
				return e
			}
		} else if pr.State == "closed" {
			status = "blocked"
			message = "PR 已关闭且未合并"
		}
		if status == "pr_open" {
			return nil
		}
		taskError := message
		if status == "completed" {
			taskError = ""
		}
		if e = taskFact(ctx, tx, taskID, taskcore.Tracked, taskcore.Facts{Status: status}, Data{"error": taskError}); e != nil {
			return e
		}
		return caseEvent(ctx, tx, Str(current, "case_id"), "pr_"+status, "system", fallback(message, "PR 已合并，开发任务完成"), Data{"pr_url": pr.HTMLURL, "head_commit": pr.Head.SHA})
	})
}
