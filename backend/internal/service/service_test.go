package service

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"agentowl/backend/internal/platform"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func testService(t *testing.T) (*Service, Actor) {
	t.Helper()
	dsn := os.Getenv("AGENTOWL_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AGENTOWL_TEST_DATABASE_URL required for PostgreSQL integration tests")
	}
	ctx := context.Background()
	admin, e := pgxpool.New(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	schema := "test_" + strings.ReplaceAll(ID(), "-", "")
	if _, e = admin.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS pg_trgm WITH SCHEMA public; CREATE SCHEMA "+schema); e != nil {
		admin.Close()
		t.Fatal(e)
	}
	c, e := pgxpool.ParseConfig(dsn)
	if e != nil {
		t.Fatal(e)
	}
	c.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	db, e := pgxpool.NewWithConfig(ctx, c)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		db.Close()
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	})
	if e = platform.Migrate(ctx, db); e != nil {
		t.Fatal(e)
	}
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	cfg := platform.Config{EncryptionKey: key, DataDir: t.TempDir(), AuthMode: "local", PublicURL: "http://localhost:3000", BootstrapName: "Test Admin", BootstrapEmail: "admin@example.test", BootstrapPassword: "test-only-password-123", SessionTTL: time.Hour}
	s, e := New(db, cfg)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.Bootstrap(ctx); e != nil {
		t.Fatal(e)
	}
	row, e := one(ctx, db, "SELECT to_jsonb(p) FROM principals p WHERE email=$1", cfg.BootstrapEmail)
	if e != nil {
		t.Fatal(e)
	}
	return s, actorFrom(row)
}
func mutateTest(t *testing.T, s *Service, a Actor, fn func(pgx.Tx) (Data, error)) Data {
	t.Helper()
	m, _, e := s.Mutate(context.Background(), a, ID(), ID(), func(tx pgx.Tx) (Data, int, error) { d, e := fn(tx); return d, 200, e })
	if e != nil {
		t.Fatal(e)
	}
	return m
}
func TestIdempotencyAndConcurrentVersion(t *testing.T) {
	s, a := testService(t)
	ctx := context.Background()
	key := ID()
	var wg sync.WaitGroup
	results := make(chan Data, 8)
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m, _, e := s.Mutate(ctx, a, key, "same-payload", func(tx pgx.Tx) (Data, int, error) {
				d, e := s.SaveCase(ctx, tx, a, "", Data{"title": "Authentication cannot refresh"})
				return d, 201, e
			})
			results <- m
			errs <- e
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	id := ""
	for m := range results {
		if id != "" && Str(m, "id") != id {
			t.Fatal("duplicate idempotent case")
		}
		id = Str(m, "id")
	}
	var n int
	_ = s.DB.QueryRow(ctx, "SELECT count(*) FROM cases").Scan(&n)
	if n != 1 {
		t.Fatalf("cases=%d", n)
	}
	_, _, e := s.Mutate(ctx, a, key, "different", func(tx pgx.Tx) (Data, int, error) { t.Fatal("must not run"); return nil, 0, nil })
	var problem *Error
	if !errors.As(e, &problem) || problem.Code != "idempotency_conflict" {
		t.Fatalf("%v", e)
	}
	successes := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, e := s.Mutate(ctx, a, ID(), ID(), func(tx pgx.Tx) (Data, int, error) {
				d, e := s.SaveCase(ctx, tx, a, id, Data{"title": "Changed", "expected_version": 1})
				return d, 200, e
			})
			successes <- e
		}()
	}
	wg.Wait()
	close(successes)
	ok, conflict := 0, 0
	for e := range successes {
		if e == nil {
			ok++
		} else if e == Conflict {
			conflict++
		} else {
			t.Fatal(e)
		}
	}
	if ok != 1 || conflict != 1 {
		t.Fatalf("success=%d conflict=%d", ok, conflict)
	}
}
func TestKnowledgeApprovalAndStaleEvidence(t *testing.T) {
	s, a := testService(t)
	ctx := context.Background()
	k := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.SaveKnowledge(ctx, tx, a, "", Data{"title": "Refresh token", "body": "Sign in again when revoked.", "visibility": "public"})
	})
	ap := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.KnowledgeAction(ctx, tx, a, Str(k, "id"), "submit", Data{"expected_version": 1})
	})
	ap = mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.Decide(ctx, tx, a, Str(ap, "id"), Data{"expected_version": Num(ap, "version"), "decision": "approve"})
	})
	if e := s.executeApproval(ctx, Str(ap, "id")); e != nil {
		t.Fatal(e)
	}
	k, e := s.Get(ctx, s.DB, a, "knowledge", Str(k, "id"))
	if e != nil || Str(k, "status") != "published" {
		t.Fatalf("publish %v %v", k, e)
	}
	c := Data{"channel_id": ""}
	if _, e = s.evidence(ctx, s.DB, c, []string{Str(k, "id")}); e != nil {
		t.Fatal(e)
	}
	mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.SaveKnowledge(ctx, tx, a, Str(k, "id"), Data{"expected_version": Num(k, "version"), "body": "New evidence"})
	})
	if _, e = s.evidence(ctx, s.DB, c, []string{Str(k, "id")}); e == nil {
		t.Fatal("draft knowledge must not be used")
	}
}
func TestPermissionsSessionsAndSecrets(t *testing.T) {
	s, a := testService(t)
	ctx := context.Background()
	viewer := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.SaveUser(ctx, tx, a, "", Data{"name": "Viewer", "email": "viewer@example.test", "password": "a-password-for-tests", "role": "viewer"})
	})
	v := actorFrom(viewer)
	c := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) { return s.SaveCase(ctx, tx, a, "", Data{"title": "Private case"}) })
	if _, e := s.Get(ctx, s.DB, v, "cases", Str(c, "id")); e != NotFound {
		t.Fatalf("case leaked: %v", e)
	}
	conn := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.SaveConfig(ctx, tx, a, "connectors", "", Data{"name": "GitHub", "provider": "github", "token": "secret-value-for-encryption", "status": "verified", "sender_id": "forged", "bot_open_id": "forged", "capabilities": Data{"send": Data{"status": "verified"}}, "capability_probes": []Data{{"status": "verified"}}})
	})
	if Str(conn, "status") == "verified" || strings.Contains(string(JSON(conn)), "secret-value") {
		t.Fatal("secret/status leaked")
	}
	if Str(conn, "sender_id") != "" || Str(conn, "bot_open_id") != "" || Str(Object(Object(conn, "capabilities"), "send"), "status") != "unverified" || lenArray(conn["capability_probes"]) != 0 {
		t.Fatal("client forged derived identity or capability evidence")
	}
	raw, e := locked(ctx, s.DB, "configs", Str(conn, "id"))
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(string(JSON(raw)), "secret-value") {
		t.Fatal("plaintext secret stored")
	}
	_, token, e := s.LocalLogin(ctx, "viewer@example.test", "a-password-for-tests")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Authenticate(ctx, token); e != nil {
		t.Fatal(e)
	}
	mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.SaveUser(ctx, tx, a, Str(viewer, "id"), Data{"expected_version": 1, "active": false})
	})
	if _, e = s.Authenticate(ctx, token); e == nil {
		t.Fatal("disabled session still valid")
	}
}

func TestSelfProfileUpdateAndPasswordChange(t *testing.T) {
	s, a := testService(t)
	ctx := context.Background()
	user := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.SaveUser(ctx, tx, a, "", Data{"name": "Original", "email": "profile@example.test", "password": "old-password-for-tests", "role": "viewer"})
	})
	login1, token1, e := s.LocalLogin(ctx, "profile@example.test", "old-password-for-tests")
	if e != nil {
		t.Fatal(e)
	}
	_, token2, e := s.LocalLogin(ctx, "profile@example.test", "old-password-for-tests")
	if e != nil {
		t.Fatal(e)
	}
	profileActor := actorFrom(user)
	profileActor.CSRF = Str(login1, "csrf_token")
	updated := mutateTest(t, s, profileActor, func(tx pgx.Tx) (Data, error) {
		return s.SaveProfile(ctx, tx, profileActor, Data{"expected_version": Num(user, "version"), "name": "Renamed"})
	})
	if Str(updated, "name") != "Renamed" || Str(updated, "csrf_token") != profileActor.CSRF {
		t.Fatalf("profile update response mismatch: %#v", updated)
	}
	err := s.transaction(ctx, func(tx pgx.Tx) error {
		_, e := s.SaveProfile(ctx, tx, profileActor, Data{"expected_version": Num(updated, "version"), "name": "Renamed", "current_password": "wrong", "new_password": "new-password-for-tests"})
		return e
	})
	var fault *Error
	if !errors.As(err, &fault) || fault.Code != "invalid_credentials" {
		t.Fatalf("wrong password accepted: %v", err)
	}
	changed := mutateTest(t, s, profileActor, func(tx pgx.Tx) (Data, error) {
		return s.SaveProfile(ctx, tx, profileActor, Data{"expected_version": Num(updated, "version"), "name": "Renamed Again", "current_password": "old-password-for-tests", "new_password": "new-password-for-tests"})
	})
	if Str(changed, "name") != "Renamed Again" {
		t.Fatalf("password change lost profile update: %#v", changed)
	}
	if _, _, e = s.LocalLogin(ctx, "profile@example.test", "old-password-for-tests"); e == nil {
		t.Fatal("old password still works")
	}
	if _, _, e = s.LocalLogin(ctx, "profile@example.test", "new-password-for-tests"); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Authenticate(ctx, token1); e != nil {
		t.Fatal("current session was revoked", e)
	}
	if _, e = s.Authenticate(ctx, token2); e == nil {
		t.Fatal("other session survived password change")
	}
}

func TestIdentityBindingsRejectUnsupportedProviders(t *testing.T) {
	s, a := testService(t)
	ctx := context.Background()
	valid := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.SaveConfig(ctx, tx, a, "identity-bindings", "", Data{"principal_id": a.ID, "provider": "feishu", "app_id": "cli_test", "subject_id": "ou_test"})
	})
	if Str(valid, "provider") != "feishu" || Str(valid, "app_id") != "cli_test" || Str(valid, "subject_id") != "ou_test" {
		t.Fatal(valid)
	}
	if err := s.transaction(ctx, func(tx pgx.Tx) error {
		_, e := s.SaveConfig(ctx, tx, a, "identity-bindings", "", Data{"principal_id": a.ID, "provider": "github", "subject_id": "octocat"})
		return e
	}); err == nil {
		t.Fatal("github identity binding accepted")
	}
	if err := s.transaction(ctx, func(tx pgx.Tx) error {
		_, e := s.SaveConfig(ctx, tx, a, "identity-bindings", "", Data{"principal_id": a.ID, "provider": "lark", "subject_id": "ou_test"})
		return e
	}); err == nil {
		t.Fatal("identity binding without app_id accepted")
	}
}

func TestConfigAndUserDeletion(t *testing.T) {
	s, a := testService(t)
	ctx := context.Background()
	policy := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.SaveConfig(ctx, tx, a, "policies", "", Data{"name": "temporary policy", "kind": "knowledge_publish", "mode": "manual", "enabled": true})
	})
	approvalID, subjectID := ID(), ID()
	if _, e := s.DB.Exec(ctx, "INSERT INTO approvals(id,kind,subject_id,subject_type,status,summary,action,action_hash,policy_id,expires_at) VALUES($1,'knowledge_publish',$2,'knowledge','pending','temporary','{}','hash',$3,now()+interval '1 hour')", approvalID, subjectID, Str(policy, "id")); e != nil {
		t.Fatal(e)
	}
	deleted := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.DeleteConfig(ctx, tx, a, "policies", Str(policy, "id"), Data{"expected_version": Num(policy, "version")})
	})
	if !Bool(deleted, "deleted") || supportCount(t, s, "SELECT count(*) FROM configs WHERE id=$1", Str(policy, "id")) != 0 || supportCount(t, s, "SELECT count(*) FROM config_versions WHERE config_id=$1", Str(policy, "id")) != 0 {
		t.Fatalf("policy was not deleted cleanly: %#v", deleted)
	}
	if supportCount(t, s, "SELECT count(*) FROM approvals WHERE id=$1 AND status='stale'", approvalID) != 1 || supportCount(t, s, "SELECT count(*) FROM audit_events WHERE resource_id=$1 AND action='delete'", Str(policy, "id")) != 1 {
		t.Fatal("delete did not stale approvals or audit")
	}
	connector := supportConfig(t, s, "connectors", Data{"name": "connector", "provider": "feishu"}, nil)
	supportConfig(t, s, "channels", Data{"name": "feedback", "connector_id": connector, "external_id": "oc_feedback", "enabled": true}, nil)
	err := s.transaction(ctx, func(tx pgx.Tx) error {
		_, e := s.DeleteConfig(ctx, tx, a, "connectors", connector, Data{"expected_version": int64(1)})
		return e
	})
	var fault *Error
	if !errors.As(err, &fault) || fault.Code != "config_in_use" || supportCount(t, s, "SELECT count(*) FROM configs WHERE id=$1", connector) != 1 {
		t.Fatalf("in-use connector deletion was not blocked: %v", err)
	}
	forceConnector := supportConfig(t, s, "connectors", Data{"name": "force connector", "provider": "feishu"}, nil)
	forceChannel := supportConfig(t, s, "channels", Data{"name": "force feedback", "connector_id": forceConnector, "external_id": "oc_force", "enabled": true}, nil)
	if _, e := s.DB.Exec(ctx, "UPDATE principals SET channel_ids=array_append(channel_ids,$1) WHERE id=$2", forceChannel, a.ID); e != nil {
		t.Fatal(e)
	}
	mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.SaveConfig(ctx, tx, a, "owner-routes", "", Data{"module": "force", "primary_id": a.ID, "channel_id": forceChannel, "enabled": true})
	})
	forceDeleted := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.DeleteConfig(ctx, tx, a, "channels", forceChannel, Data{"expected_version": int64(1), "force": true})
	})
	if !Bool(forceDeleted, "deleted") || supportCount(t, s, "SELECT count(*) FROM configs WHERE id=$1", forceChannel) != 0 {
		t.Fatalf("force delete did not remove channel: %#v", forceDeleted)
	}
	if supportCount(t, s, "SELECT count(*) FROM principals WHERE id=$1 AND $2=ANY(channel_ids)", a.ID, forceChannel) != 0 || supportCount(t, s, "SELECT count(*) FROM configs WHERE kind='owner-routes' AND data->>'channel_id'=$1", forceChannel) != 0 {
		t.Fatal("force delete did not detach safe channel references")
	}
	user := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.SaveUser(ctx, tx, a, "", Data{"name": "Temporary", "email": "temporary@example.test", "password": "a-password-for-tests", "role": "viewer"})
	})
	deletedUser := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.DeleteUser(ctx, tx, a, Str(user, "id"), Data{"expected_version": Num(user, "version")})
	})
	if !Bool(deletedUser, "deleted") || supportCount(t, s, "SELECT count(*) FROM principals WHERE id=$1", Str(user, "id")) != 0 {
		t.Fatalf("user was not deleted: %#v", deletedUser)
	}
	err = s.transaction(ctx, func(tx pgx.Tx) error {
		_, e := s.DeleteUser(ctx, tx, a, a.ID, Data{"expected_version": int64(1)})
		return e
	})
	if !errors.As(err, &fault) || fault.Code != "validation" {
		t.Fatalf("self delete was not blocked: %v", err)
	}
}
func TestApprovalDenyAndJobFence(t *testing.T) {
	s, a := testService(t)
	ctx := context.Background()
	k := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.SaveKnowledge(ctx, tx, a, "", Data{"title": "Auth", "body": "Evidence"})
	})
	ap := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.KnowledgeAction(ctx, tx, a, Str(k, "id"), "submit", Data{"expected_version": 1})
	})
	ap = mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.Decide(ctx, tx, a, Str(ap, "id"), Data{"expected_version": Num(ap, "version"), "decision": "approve"})
	})
	mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.SaveConfig(ctx, tx, a, "policies", "", Data{"name": "deny publish", "kind": "knowledge_publish", "mode": "deny", "enabled": true})
	})
	if e := s.executeApproval(ctx, Str(ap, "id")); e == nil {
		t.Fatal("new deny rule did not stop approved action")
	}
	jid := ID()
	_, e := s.DB.Exec(ctx, "INSERT INTO jobs(id,kind,dedupe_key,payload,status,lease_token,lease_until) VALUES($1::uuid,'test',$1::text,'{}','running','current',now()+interval '30 seconds')", jid)
	if e != nil {
		t.Fatal(e)
	}
	oldCtx := context.WithValue(ctx, leaseKey{}, Data{"id": jid, "lease_token": "old"})
	called := false
	e = s.transaction(oldCtx, func(tx pgx.Tx) error { called = true; return nil })
	if e == nil || called {
		t.Fatal("expired worker committed")
	}
}

func TestPolicyVersionAndSourceWithdrawal(t *testing.T) {
	s, a := testService(t)
	ctx := context.Background()
	policy := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.SaveConfig(ctx, tx, a, "policies", "", Data{"name": "auto knowledge", "kind": "knowledge_publish", "mode": "auto", "enabled": true, "max_tokens": 1000, "max_files": 20, "max_seconds": 60})
	})
	k := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.SaveKnowledge(ctx, tx, a, "", Data{"title": "Policy evidence", "body": "Verified answer"})
	})
	ap := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.KnowledgeAction(ctx, tx, a, Str(k, "id"), "submit", Data{"expected_version": 1})
	})
	if Str(ap, "status") != "approved" || Num(ap, "policy_version") != 1 {
		t.Fatal("auto policy decision lost version", ap)
	}
	mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.SaveConfig(ctx, tx, a, "policies", Str(policy, "id"), Data{"expected_version": 1, "enabled": false})
	})
	if e := s.executeApproval(ctx, Str(ap, "id")); e != nil {
		t.Fatal(e)
	}
	current, e := locked(ctx, s.DB, "knowledge", Str(k, "id"))
	if e != nil || Str(current, "status") == "published" {
		t.Fatal("invalidated policy still published")
	}
	source := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.SaveConfig(ctx, tx, a, "knowledge-sources", "", Data{"name": "Scoped source", "kind": "lark", "visibility": "public"})
	})
	_, e = s.DB.Exec(ctx, "UPDATE knowledge SET source_id=$2,status='published' WHERE id=$1", Str(k, "id"), Str(source, "id"))
	if e != nil {
		t.Fatal(e)
	}
	source = mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.SaveConfig(ctx, tx, a, "knowledge-sources", Str(source, "id"), Data{"expected_version": 1, "visibility": "internal", "status": "synced"})
	})
	if Str(source, "status") == "synced" {
		t.Fatal("client forged source permission status")
	}
	current, e = locked(ctx, s.DB, "knowledge", Str(k, "id"))
	if e != nil || Str(current, "status") != "withdrawn" {
		t.Fatal("source change left published knowledge", e)
	}
}

func TestFailedJobRetryAndDashboard(t *testing.T) {
	s, a := testService(t)
	ctx := context.Background()
	jid := ID()
	_, e := s.DB.Exec(ctx, "INSERT INTO jobs(id,kind,dedupe_key,payload,status,attempts,lease_token,lease_until) VALUES($1::uuid,'classify',$1::text,'{}','running',5,'owner',now()+interval '45 seconds')", jid)
	if e != nil {
		t.Fatal(e)
	}
	j, e := locked(ctx, s.DB, "jobs", jid)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.failJob(ctx, j, Fail(422, "test_failure", "Synthetic fixture")); e != nil {
		t.Fatal(e)
	}
	viewer := Actor{ID: ID(), Role: "viewer"}
	if _, e = s.Get(ctx, s.DB, viewer, "jobs", jid); e != NotFound {
		t.Fatal("job payload leaked")
	}
	j = mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.RetryJob(ctx, tx, a, jid, Data{"expected_attempts": 5, "reason": "Local test"})
	})
	if Str(j, "status") != "pending" || Num(j, "attempts") != 0 {
		t.Fatal("retry did not schedule same job")
	}
	_, _, e = s.Mutate(ctx, a, ID(), ID(), func(tx pgx.Tx) (Data, int, error) {
		d, e := s.RetryJob(ctx, tx, a, jid, Data{"expected_attempts": 5})
		return d, 200, e
	})
	if e != Conflict {
		t.Fatal("duplicate retry accepted", e)
	}
	c := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) { return s.SaveCase(ctx, tx, a, "", Data{"title": "Closed case"}) })
	mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.CaseAction(ctx, tx, a, Str(c, "id"), Data{"expected_version": 1, "action": "close"})
	})
	d, e := s.Dashboard(ctx, a)
	if e != nil || Num(d, "open_cases") != 0 || Num(d, "unassigned_cases") != 0 {
		t.Fatal("closed case counted as pending", d, e)
	}
}

func TestRepositoryScopeRequiredForCodeArtifacts(t *testing.T) {
	s, a := testService(t)
	ctx := context.Background()
	viewer := Actor{ID: ID(), Role: "oncall"}
	c := mutateTest(t, s, viewer, func(tx pgx.Tx) (Data, error) {
		return s.SaveCase(ctx, tx, viewer, "", Data{"title": "Case visible without repository access"})
	})
	repo := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.SaveConfig(ctx, tx, a, "repositories", "", Data{"name": "Private code", "owner": "owner", "repo": "private", "enabled": true})
	})
	task := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.ConfirmDevelopment(ctx, tx, a, Str(c, "id"), Data{"expected_version": 1, "repository_id": Str(repo, "id"), "goal": "Fix", "acceptance_criteria": "Tests pass"})
	})
	artifact := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.storeArtifact(ctx, tx, Str(task, "id"), Str(c, "id"), "", "code.patch", "text/plain", []byte("private code"))
	})
	if _, e := s.Get(ctx, s.DB, viewer, "cases", Str(c, "id")); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Get(ctx, s.DB, viewer, "artifacts", Str(artifact, "id")); e != NotFound {
		t.Fatal("case permission exposed repository code", e)
	}
	viewer.RepositoryIDs = []string{Str(repo, "id")}
	if _, e := s.Get(ctx, s.DB, viewer, "artifacts", Str(artifact, "id")); e != nil {
		t.Fatal(e)
	}
}
