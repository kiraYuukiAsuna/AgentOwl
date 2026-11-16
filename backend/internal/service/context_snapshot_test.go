package service

import (
	"agentowl/backend/internal/runner"
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"strings"
	"testing"
)

func TestRuntimeContextSnapshotAndRevocation(t *testing.T) {
	s, a, task, _, run, _ := queuedSettingsRun(t)
	ctx := context.Background()
	runID := Str(run, "id")
	manifest, err := s.RunnerContextManifest(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if Num(manifest, "version") != 2 || Str(Object(Object(manifest, "facts"), "sponsor"), "principal_id") != a.ID {
		t.Fatal("missing request facts", manifest)
	}
	page, err := s.RunnerContextPage(ctx, runID, "case", "")
	if err != nil {
		t.Fatal(err)
	}
	before := Str(page, "text")
	if _, err = s.DB.Exec(ctx, "UPDATE cases SET description='later unapproved material' WHERE id=$1", Str(task, "case_id")); err != nil {
		t.Fatal(err)
	}
	after, err := s.RunnerContextPage(ctx, runID, "case", "")
	if err != nil || Str(after, "text") != before {
		t.Fatal("frozen context changed", err)
	}
	if _, err = s.RunnerContextPage(ctx, runID, "arbitrary-private-artifact", ""); !errors.Is(err, NotFound) {
		t.Fatal("unknown context accessible", err)
	}
	if _, err = s.DB.Exec(ctx, "UPDATE dev_tasks SET epoch=epoch+1 WHERE id=$1", Str(task, "id")); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RunnerContextPage(ctx, runID, "case", ""); !errors.Is(err, Forbidden) {
		t.Fatal("revoked context accessible", err)
	}
	if _, err = s.RunnerContextManifest(ctx, runID); !errors.Is(err, Forbidden) {
		t.Fatal("revoked manifest accessible", err)
	}
}

func TestRuntimeContextFencesReferenceMessageChanges(t *testing.T) {
	f := newDevelopmentApprovalFixture(t, "policy", 30)
	s, task := f.s, f.task
	_, machineURL := supportCapabilityServer(t, runner.Capabilities{ContextManifestVersion: 2, Reachable: true, Ready: true, SupportsResourceLimits: true, ResourceLimits: runner.ResourceLimitsDisabled, MaxConcurrent: 1, SettingsConfigured: true, ModelConfigured: true})
	supportRuntimeMachine(t, s, machineURL)
	ctx := context.Background()
	connector, channel := supportLarkFixture(t, s)
	mid := ID()
	if _, err := s.DB.Exec(ctx, `INSERT INTO source_messages(id,connector_id,channel_id,external_id,case_id,content,source_revision) VALUES($1,$2,$3,'reference-message',$4,'reference body','one')`, mid, connector, channel, Str(task, "case_id")); err != nil {
		t.Fatal(err)
	}

	if _, err := s.DB.Exec(ctx, `INSERT INTO task_inputs(id,task_id,seq,source_message_id,source_version,kind,actor_ref,body,digest,disposition) VALUES($1,$2,1,$3,1,'source','{}','reference body',$4,'context')`, ID(), Str(task, "id"), mid, Hash([]byte("reference body"))); err != nil {
		t.Fatal(err)
	}
	step, ap := f.prepare(t, "plan")
	mutateTest(t, s, f.a, func(tx pgx.Tx) (Data, error) {
		return s.Decide(ctx, tx, f.a, Str(ap, "id"), Data{"decision": "approve", "expected_version": Num(ap, "version")})
	})
	if err := s.executeApproval(ctx, Str(ap, "id")); err != nil {
		t.Fatal(err)
	}
	run, err := one(ctx, s.DB, `SELECT to_jsonb(r) FROM run_attempts r WHERE step_id=$1`, Str(step, "id"))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.RunnerContextPage(ctx, Str(run, "id"), "case", ""); err != nil {
		t.Fatal(err)
	}
	eventID := ID()
	for _, event := range []Data{{"message_id": "reference-message", "source_revision": "two"}, {"message_id": "reference-message", "source_revision": "one", "deleted": true}} {
		if _, err := s.DB.Exec(ctx, `INSERT INTO ingest_events(id,connector_id,event_id,kind,payload) VALUES($1::uuid,$2,$1::uuid::text,'message',$3) ON CONFLICT(id) DO UPDATE SET payload=EXCLUDED.payload`, eventID, connector, JSON(Data{"channel_id": channel, "event": event})); err != nil {
			t.Fatal(err)
		}
		if _, err := s.RunnerContextPage(ctx, Str(run, "id"), "case", ""); err == nil {
			t.Fatal("pending source change did not fence context")
		} else {
			var failure *Error
			if !errors.As(err, &failure) || failure.Code != "input_changed" {
				t.Fatal("unexpected context error", err)
			}
		}
	}
	if _, err := s.DB.Exec(ctx, `UPDATE ingest_events SET status='processed' WHERE id=$1`, eventID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(ctx, `UPDATE source_messages SET deleted=true,version=version+1 WHERE id=$1`, mid); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RunnerContextPage(ctx, Str(run, "id"), "case", ""); err == nil {
		t.Fatal("deleted reference remained readable")
	}
}

func TestContextRequiresCompatibleRuntime(t *testing.T) {
	s, _ := testService(t)
	_, url := supportCapabilityServer(t, runner.Capabilities{Ready: true, Reachable: true})
	machine := supportRuntimeMachine(t, s, url)
	_, err := s.pickRuntimeMachine(context.Background(), runner.RunSpec{ContextManifest: runner.ContextManifest{Version: 2}})
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "runtime_context_unsupported" {
		t.Fatal("old runtime accepted current context", err)
	}
	if _, err = s.pickRuntimeMachine(context.Background(), runner.RunSpec{ContextManifest: runner.ContextManifest{Version: 1}}); err != nil {
		t.Fatal("legacy run blocked", err)
	}
	info, err := s.runtimeMachineCapabilities(context.Background(), machine)
	if err != nil || Bool(info, "ready") || !strings.Contains(Str(info, "error"), "升级") {
		t.Fatal("missing upgrade status", info, err)
	}
}

func TestContextFreezesReferenceInputsAndIdentityWithoutInlineHistory(t *testing.T) {
	s, a, task, _ := runtimeTaskFixture(t)
	ctx := context.Background()
	connector, channel := supportLarkFixture(t, s)
	supportConfig(t, s, "identity-bindings", Data{"provider": "feishu", "app_id": "cli_test", "subject_id": "ou_requester", "principal_id": a.ID}, nil)
	mid := ID()
	long := strings.Repeat("原始对话🙂\n", 900)
	if _, err := s.DB.Exec(ctx, `INSERT INTO source_messages(id,connector_id,channel_id,external_id,case_id,actor_id,content,thread_id) VALUES($1,$2,$3,'om_context',$4,'ou_requester',$5,'thread_context')`, mid, connector, channel, Str(task, "case_id"), long); err != nil {
		t.Fatal(err)
	}
	source, err := one(ctx, s.DB, "SELECT to_jsonb(m) FROM source_messages m WHERE id=$1", mid)
	if err != nil {
		t.Fatal(err)
	}
	in := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.appendTaskInput(ctx, tx, task, long, "source", Data{"id": "ou_requester"}, source)
	})
	if Str(Object(in, "actor_ref"), "principal_id") != a.ID {
		t.Fatal("message did not share binding")
	}
	if _, err = s.DB.Exec(ctx, "UPDATE task_inputs SET disposition='context' WHERE id=$1", Str(in, "id")); err != nil {
		t.Fatal(err)
	}
	snapshot := Data{"previous_summary": strings.Repeat("完整阶段结果\n", 2000)}
	if err = s.freezeContext(ctx, s.DB, task, snapshot); err != nil {
		t.Fatal(err)
	}
	if Str(snapshot, "context") != "" || strings.Contains(string(JSON(snapshot["context_manifest"])), long) {
		t.Fatal("history was inlined")
	}
	if !strings.Contains(Str(Object(snapshot, "context_documents"), "reference_inputs"), "原始对话") || !strings.Contains(Str(Object(snapshot, "context_documents"), "reference_inputs"), a.ID) {
		t.Fatal("reference source or identity missing")
	}
	if Str(Object(Object(snapshot, "context_facts"), "requester"), "principal_id") != a.ID {
		t.Fatal("requester identity missing")
	}
	manifest := s.buildContextManifest(ctx, snapshot)
	found := false
	for _, ref := range manifest.Refs {
		if ref.ID == "previous_summary" {
			found = true
			if ref.Inline != "" || ref.Digest == "" {
				t.Fatal("summary is not a full frozen reference")
			}
		}
	}
	if !found {
		t.Fatal("summary reference missing")
	}
	if _, err = s.DB.Exec(ctx, "UPDATE source_messages SET content='new message',version=version+1 WHERE id=$1", mid); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(Str(Object(snapshot, "context_documents"), "reference_inputs"), "new message") {
		t.Fatal("reference snapshot changed")
	}
}

func TestContextCannotExposeAnotherChannelsInputs(t *testing.T) {
	f := newDevelopmentApprovalFixture(t, "policy", 30)
	s, a, task := f.s, f.a, f.task
	ctx := context.Background()
	connector, channel := supportLarkFixture(t, s)
	privateID, mid, inputID := ID(), ID(), ID()
	if _, err := s.DB.Exec(ctx, `INSERT INTO cases(id,canonical_case_id,title,channel_id,created_by) VALUES($1,$1,'Private',$2,'someone_else')`, privateID, channel); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(ctx, `INSERT INTO source_messages(id,connector_id,channel_id,external_id,case_id,content) VALUES($1,$2,$3,'private-message',$4,'private context')`, mid, connector, channel, privateID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(ctx, `INSERT INTO task_inputs(id,task_id,seq,source_message_id,source_version,kind,actor_ref,body,digest,disposition) VALUES($1,$2,1,$3,1,'source','{}','private context',$4,'context')`, inputID, Str(task, "id"), mid, Hash([]byte("private context"))); err != nil {
		t.Fatal(err)
	}
	_, ap := f.prepare(t, "plan")
	limited := a
	limited.Role = "developer"
	limited.Roles = nil
	limited.RepositoryIDs = []string{"*"}
	limited.ChannelIDs = nil
	if _, err := s.Get(ctx, s.DB, limited, "development-tasks", Str(task, "id")); !errors.Is(err, NotFound) {
		t.Fatal("private context in task", err)
	}
	if _, err := s.Get(ctx, s.DB, limited, "approvals", Str(ap, "id")); !errors.Is(err, NotFound) {
		t.Fatal("private context in approval", err)
	}
	limited.ChannelIDs = []string{channel}
	if _, err := s.Get(ctx, s.DB, limited, "development-tasks", Str(task, "id")); err != nil {
		t.Fatal(err)
	}
}
