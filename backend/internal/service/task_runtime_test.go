package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func runtimeTaskFixture(t *testing.T) (*Service, Actor, Data, Data) {
	s, a := testService(t)
	ctx := context.Background()
	repo := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.SaveConfig(ctx, tx, a, "repositories", "", Data{"name": "runtime", "owner": "owner", "repo": "repo", "enabled": true, "allowed_paths": []string{"src"}, "runner_image": "local/test"})
	})
	c := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) { return s.SaveCase(ctx, tx, a, "", Data{"title": "Runtime input test"}) })
	task := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.ConfirmDevelopment(ctx, tx, a, Str(c, "id"), Data{"expected_version": Num(c, "version"), "repository_id": Str(repo, "id"), "goal": "repair", "acceptance_criteria": "tests pass"})
	})
	return s, a, task, repo
}
func TestRuntimeInputsFreezeAndRevoke(t *testing.T) {
	s, a, task, _ := runtimeTaskFixture(t)
	ctx := context.Background()
	id := Str(task, "id")
	input := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.AddTaskInput(ctx, tx, a, id, Data{"expected_version": Num(task, "version"), "body": "Steps to reproduce"})
	})
	current, e := s.Get(ctx, s.DB, a, "development-tasks", id)
	if e != nil {
		t.Fatal(e)
	}
	var se *Error
	if e = s.taskInputGate(ctx, s.DB, current); !errors.As(e, &se) || se.Code != "source_pending" {
		t.Fatal(e)
	}
	revised := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.ReviseTask(ctx, tx, a, id, Data{"expected_version": Num(current, "version"), "goal": "repair", "acceptance_criteria": "tests pass", "input_ids": []string{Str(input, "id")}})
	})
	if Num(revised, "revision") != 2 || Str(revised, "mandate_id") == Str(task, "mandate_id") {
		t.Fatal(revised)
	}
	frozen := Data{}
	if e = s.freezeTaskInputs(ctx, s.DB, revised, frozen); e != nil {
		t.Fatal(e)
	}
	if Str(frozen, "input_set_digest") == "" || len(frozen["task_inputs"].([]Data)) != 1 {
		t.Fatal(frozen)
	}
	newer := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.AddTaskInput(ctx, tx, a, id, Data{"expected_version": Num(revised, "version"), "body": "Further context"})
	})
	_ = newer
	if len(frozen["task_inputs"].([]Data)) != 1 {
		t.Fatal("mutated frozen snapshot")
	}
	current, _ = s.Get(ctx, s.DB, a, "development-tasks", id)
	mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.TaskAction(ctx, tx, a, id, Data{"expected_version": Num(current, "version"), "action": "revoke"})
	})
	current, _ = s.Get(ctx, s.DB, a, "development-tasks", id)
	if e = s.validateTaskMandate(ctx, s.DB, current); e == nil {
		t.Fatal("revoked authorization accepted")
	}
}
func TestRuntimeSourcePendingAndDedup(t *testing.T) {
	s, a, task, repo := runtimeTaskFixture(t)
	ctx := context.Background()
	co, ch := supportLarkFixture(t, s)
	message := supportMessage("runtime-first", "", time.Now().Add(-time.Minute))
	h, b := supportSigned(t, ID(), "im.message.receive_v1", message)
	in, e := s.IngestWebhook(ctx, "feishu", co, h, b)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.consumeSupportEvent(ctx, Str(in, "id")); e != nil {
		t.Fatal(e)
	}
	c, e := one(ctx, s.DB, "SELECT to_jsonb(c) FROM cases c WHERE channel_id=$1", ch)
	if e != nil {
		t.Fatal(e)
	}
	task = mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.ConfirmDevelopment(ctx, tx, a, Str(c, "id"), Data{"expected_version": Num(c, "version"), "repository_id": Str(repo, "id"), "goal": "repair", "acceptance_criteria": "passes"})
	})
	var before int64
	if e = s.DB.QueryRow(ctx, "SELECT generation FROM source_threads WHERE connector_id=$1", co).Scan(&before); e != nil {
		t.Fatal(e)
	}
	if _, e = s.IngestWebhook(ctx, "feishu", co, h, b); e != nil {
		t.Fatal(e)
	}
	var after int64
	s.DB.QueryRow(ctx, "SELECT generation FROM source_threads WHERE connector_id=$1", co).Scan(&after)
	if before != after {
		t.Fatal("duplicate advanced generation")
	}
	follow := supportMessage("runtime-follow", "More evidence", time.Now())
	Object(follow, "message")["root_id"] = "runtime-first"
	h, b = supportSigned(t, ID(), "im.message.receive_v1", follow)
	in, e = s.IngestWebhook(ctx, "feishu", co, h, b)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.taskInputGate(ctx, s.DB, task); e == nil {
		t.Fatal("pending source did not fence task")
	}
	if e = s.consumeSupportEvent(ctx, Str(in, "id")); e != nil {
		t.Fatal(e)
	}
	if e = s.routeTaskSource(ctx, Str(in, "id")); e != nil {
		t.Fatal(e)
	}
	var count int
	if e = s.DB.QueryRow(ctx, "SELECT count(*) FROM task_inputs WHERE task_id=$1", Str(task, "id")).Scan(&count); e != nil || count != 2 {
		t.Fatal(count, e)
	}
	if e = s.routeTaskSource(ctx, Str(in, "id")); e != nil {
		t.Fatal(e)
	}
	s.DB.QueryRow(ctx, "SELECT count(*) FROM task_inputs WHERE task_id=$1", Str(task, "id")).Scan(&count)
	if count != 2 {
		t.Fatal("replayed input")
	}
	h, b = supportSigned(t, ID(), "im.message.recalled_v1", Data{"message_id": "runtime-first", "chat_id": "oc_test", "recall_time": fmt.Sprint(time.Now().UnixMilli())})
	in, e = s.IngestWebhook(ctx, "feishu", co, h, b)
	if e != nil {
		t.Fatal(e)
	}
	if s.validateTaskMandate(ctx, s.DB, task) == nil {
		t.Fatal("pending recall did not fence frozen input")
	}
	if e = s.consumeSupportEvent(ctx, Str(in, "id")); e != nil {
		t.Fatal(e)
	}
	if e = s.routeTaskSource(ctx, Str(in, "id")); e != nil {
		t.Fatal(e)
	}
	current, e := s.Get(ctx, s.DB, a, "development-tasks", Str(task, "id"))
	if e != nil {
		t.Fatal(e)
	}
	if Num(current, "epoch") <= Num(task, "epoch") || Str(current, "status") != "blocked" {
		t.Fatal("recall did not revoke execution", current)
	}
}
func TestRuntimeObserverNeverCreatesTaskOnUnavailableModel(t *testing.T) {
	s, a, _, repo := runtimeTaskFixture(t)
	ctx := context.Background()
	_, ch := supportLarkFixture(t, s)
	mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.SaveConfig(ctx, tx, a, "policies", "", Data{"kind": "create_task", "mode": "manual", "name": "review bugs", "channel_id": ch, "repository_id": Str(repo, "id"), "sponsor_principal_id": a.ID, "condition": "clear defects", "enabled": true})
	})
	c := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.SaveCase(ctx, tx, a, "", Data{"title": "new defect", "channel_id": ch})
	})
	if e := s.evaluateAdmission(ctx, Str(c, "id")); e != nil {
		t.Fatal(e)
	}
	var tasks int
	s.DB.QueryRow(ctx, "SELECT count(*) FROM dev_tasks WHERE case_id=$1", Str(c, "id")).Scan(&tasks)
	if tasks != 0 {
		t.Fatal("observation created task")
	}
	var decisions int
	s.DB.QueryRow(ctx, "SELECT count(*) FROM admission_decisions WHERE case_id=$1", Str(c, "id")).Scan(&decisions)
	if decisions != 1 {
		t.Fatal("missing unavailable decision", decisions)
	}
}

func TestRuntimeAdmissionModesAndDedup(t *testing.T) {
	for _, mode := range []string{"off", "manual", "auto"} {
		t.Run(mode, func(t *testing.T) {
			s, a, _, repo := runtimeTaskFixture(t)
			ctx := context.Background()
			co, ch := supportLarkFixture(t, s)
			h, b := supportSigned(t, ID(), "im.message.receive_v1", supportMessage("admission-root", "Fix the broken login", time.Now()))
			in, e := s.IngestWebhook(ctx, "feishu", co, h, b)
			if e != nil {
				t.Fatal(e)
			}
			if e = s.consumeSupportEvent(ctx, Str(in, "id")); e != nil {
				t.Fatal(e)
			}
			c, e := one(ctx, s.DB, "SELECT to_jsonb(c) FROM cases c WHERE channel_id=$1", ch)
			if e != nil {
				t.Fatal(e)
			}
			msg, e := one(ctx, s.DB, "SELECT to_jsonb(m) FROM source_messages m WHERE case_id=$1", Str(c, "id"))
			if e != nil {
				t.Fatal(e)
			}
			rule := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
				return s.SaveConfig(ctx, tx, a, "policies", "", Data{"kind": "create_task", "mode": map[string]string{"off": "manual", "manual": "manual", "auto": "auto", "deny": "deny"}[mode], "name": "defects", "channel_id": ch, "repository_id": Str(repo, "id"), "sponsor_principal_id": a.ID, "condition": "repair clear defects", "enabled": mode != "off"})
			})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				supportModelResponse(w, Data{"outcome": "accept", "goal": "Repair login", "acceptance_criteria": "Login succeeds", "reason": "Reproduction supplied", "rule_id": Str(rule, "id"), "evidence_ids": []string{Str(msg, "id")}})
			}))
			defer server.Close()
			saveModelSettingTest(t, s, a, Data{"expected_version": 0, "enabled": true, "base_url": server.URL, "allow_http": true, "model_name": "test"})
			for range 2 {
				if e = s.evaluateAdmission(ctx, Str(c, "id")); e != nil {
					t.Fatal(e)
				}
			}
			if mode != "off" {
				ap, err := one(ctx, s.DB, "SELECT to_jsonb(a) FROM approvals a WHERE kind='create_task' AND action->>'case_id'=$1", Str(c, "id"))
				if err != nil {
					t.Fatal(err)
				}
				want := map[string]string{"manual": "pending", "auto": "approved", "deny": "rejected"}[mode]
				if Str(ap, "status") != want {
					t.Fatal(ap)
				}
				if e = s.executeApproval(ctx, Str(ap, "id")); e != nil {
					t.Fatal(e)
				}
				if e = s.executeApproval(ctx, Str(ap, "id")); e != nil {
					t.Fatal(e)
				}
			}
			count := supportCount(t, s, "SELECT count(*) FROM dev_tasks WHERE case_id=$1", Str(c, "id"))
			if mode == "auto" {
				if count != 1 {
					t.Fatal("automatic task missing", count)
				}
				task, e := one(ctx, s.DB, "SELECT to_jsonb(t) FROM dev_tasks t WHERE case_id=$1", Str(c, "id"))
				if e != nil {
					t.Fatal(e)
				}
				if Str(task, "status") != "queued" || supportCount(t, s, "SELECT count(*) FROM approvals WHERE subject_id=$1 AND status='approved'", Str(task, "id")) != 0 {
					t.Fatal("admission bypassed phase approval")
				}
				if e = s.validateTaskMandate(ctx, s.DB, task); e != nil {
					t.Fatal(e)
				}
				mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
					return s.SaveConfig(ctx, tx, a, "policies", Str(rule, "id"), Data{"expected_version": Num(rule, "version"), "enabled": false})
				})
				if e = s.validateTaskMandate(ctx, s.DB, task); e == nil {
					t.Fatal("disabled policy still authorizes task")
				}
			} else if count != 0 {
				t.Fatal("non-auto mode created task")
			}
			if mode == "manual" {
				if n := supportCount(t, s, "SELECT count(*) FROM admission_decisions WHERE case_id=$1 AND outcome='would_accept'", Str(c, "id")); n != 1 {
					t.Fatal("observation missing", n)
				}
				if e = s.evaluateAdmission(ctx, Str(c, "id"), ID()); e != nil {
					t.Fatal(e)
				}
				if n := supportCount(t, s, "SELECT count(*) FROM admission_decisions WHERE case_id=$1", Str(c, "id")); n != 2 {
					t.Fatal("reevaluation reused old record", n)
				}
			}
			if mode == "off" && supportCount(t, s, "SELECT count(*) FROM admission_evaluations") != 0 {
				t.Fatal("off mode evaluated")
			}
		})
	}
}

func TestRuntimeFollowupCreatesApprovedSnapshotBoundary(t *testing.T) {
	s, a, task, _ := runtimeTaskFixture(t)
	ctx := context.Background()
	id := Str(task, "id")
	task = mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.ReviseTask(ctx, tx, a, id, Data{"expected_version": Num(task, "version"), "goal": Str(task, "goal"), "acceptance_criteria": Str(task, "acceptance_criteria"), "followup_mode": "within_scope"})
	})
	input := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.AddTaskInput(ctx, tx, a, id, Data{"expected_version": Num(task, "version"), "body": "Reproduce with an expired session"})
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		supportModelResponse(w, Data{"disposition": "clarification", "reason": "Reproduction within current goal"})
	}))
	defer server.Close()
	saveModelSettingTest(t, s, a, Data{"expected_version": 0, "enabled": true, "base_url": server.URL, "allow_http": true, "model_name": "test"})
	if e := s.evaluateTaskInputs(ctx, id); e != nil {
		t.Fatal(e)
	}
	ev, e := one(ctx, s.DB, "SELECT to_jsonb(e) FROM task_input_evaluations e WHERE task_id=$1", id)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.applyTaskInputs(ctx, Data{"task_id": id, "evaluation_id": Str(ev, "id")}); e != nil {
		t.Fatal(e)
	}
	current, e := s.Get(ctx, s.DB, a, "development-tasks", id)
	if e != nil {
		t.Fatal(e)
	}
	if Num(current, "revision") != Num(task, "revision")+1 || Str(current, "stage") != "plan" || Str(current, "status") != "queued" {
		t.Fatal(current)
	}
	if n := supportCount(t, s, "SELECT count(*) FROM task_revision_inputs WHERE task_id=$1 AND revision=$2 AND input_id=$3", id, Num(current, "revision"), Str(input, "id")); n != 1 {
		t.Fatal("input not bound", n)
	}
	if e = s.applyTaskInputs(ctx, Data{"task_id": id, "evaluation_id": Str(ev, "id")}); e != nil {
		t.Fatal(e)
	}
	again, _ := s.Get(ctx, s.DB, a, "development-tasks", id)
	if Num(again, "revision") != Num(current, "revision") {
		t.Fatal("replayed evaluation")
	}
}

func TestRuntimeLegacyBackfillIsIdempotent(t *testing.T) {
	s, a, task, _ := runtimeTaskFixture(t)
	ctx := context.Background()
	id := Str(task, "id")
	if _, e := s.DB.Exec(ctx, "UPDATE dev_tasks SET mandate_id=NULL WHERE id=$1", id); e != nil {
		t.Fatal(e)
	}
	if _, e := s.DB.Exec(ctx, "DELETE FROM task_mandates WHERE task_id=$1", id); e != nil {
		t.Fatal(e)
	}
	co, ch := supportLarkFixture(t, s)
	if _, e := s.DB.Exec(ctx, `INSERT INTO source_messages(id,connector_id,channel_id,external_id,case_id,actor_id,content,thread_id,source_revision) VALUES($1,$2,$3,'historical',$4,'user','original evidence','historical-thread','one')`, ID(), co, ch, Str(task, "case_id")); e != nil {
		t.Fatal(e)
	}

	for range 2 {
		if e := s.backfillTaskRuntime(ctx); e != nil {
			t.Fatal(e)
		}
	}
	sources, e := s.SourceThreads(ctx, a)
	if e != nil || len(sources["items"].([]Data)) != 1 {
		t.Fatal("historical thread not restored", sources, e)
	}

	current, e := s.Get(ctx, s.DB, a, "development-tasks", id)
	if e != nil {
		t.Fatal(e)
	}
	if supportCount(t, s, "SELECT count(*) FROM task_mandates WHERE task_id=$1", id) != 1 {
		t.Fatal("backfill duplicates")
	}
	if s.validateTaskMandate(ctx, s.DB, current) == nil {
		t.Fatal("unknown historical scope authorized")
	}
	if supportCount(t, s, "SELECT count(*) FROM run_input_receipts") != 0 {
		t.Fatal("fabricated historical receipts")
	}
}

func TestRuntimeMachineCannotReadAnotherRun(t *testing.T) {
	s, _, _, _, run, _ := queuedSettingsRun(t)
	if e := s.CheckRunMachine(context.Background(), Str(run, "id"), Str(run, "runtime_machine_id")); e != nil {
		t.Fatal(e)
	}
	if e := s.CheckRunMachine(context.Background(), Str(run, "id"), ID()); e != Forbidden {
		t.Fatal("cross-machine access", e)
	}
}

func TestRuntimeExcludedInputKeepsHistory(t *testing.T) {
	s, a, task, _ := runtimeTaskFixture(t)
	ctx := context.Background()
	id := Str(task, "id")
	input := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.AddTaskInput(ctx, tx, a, id, Data{"expected_version": Num(task, "version"), "body": "Temporary hypothesis"})
	})
	task, _ = s.Get(ctx, s.DB, a, "development-tasks", id)
	task = mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.ReviseTask(ctx, tx, a, id, Data{"expected_version": Num(task, "version"), "goal": "repair", "acceptance_criteria": "passes", "input_ids": []string{Str(input, "id")}})
	})
	task = mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.ReviseTask(ctx, tx, a, id, Data{"expected_version": Num(task, "version"), "goal": "repair", "acceptance_criteria": "passes", "excluded_input_ids": []string{Str(input, "id")}})
	})
	if supportCount(t, s, "SELECT count(*) FROM task_revision_inputs WHERE task_id=$1 AND revision=2", id) != 1 || supportCount(t, s, "SELECT count(*) FROM task_revision_inputs WHERE task_id=$1 AND revision=3", id) != 0 {
		t.Fatal("historical membership was overwritten")
	}
	if e := s.taskInputGate(ctx, s.DB, task); e != nil {
		t.Fatal(e)
	}
	a.Role = "developer"
	a.Roles = nil
	a.RepositoryIDs = []string{"*"}
	a.ChannelIDs = []string{"*"}
	detail, e := s.TaskRuntimeDetail(ctx, a, id)
	if e != nil || len(detail["inputs"].([]Data)) != 1 {
		t.Fatal(detail, e)
	}
	if _, e = s.SourceThreads(ctx, a); e != nil {
		t.Fatal(e)
	}
	mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.AddTaskInput(ctx, tx, a, id, Data{"expected_version": Num(task, "version"), "body": "New evidence"})
	})
	page, e := s.TaskRuntimeDetail(ctx, a, id, "1")
	if e != nil || Num(page, "pending_inputs") != 1 || len(page["inputs"].([]Data)) != 0 {
		t.Fatal("page cursor changed total pending count", page, e)
	}
}
