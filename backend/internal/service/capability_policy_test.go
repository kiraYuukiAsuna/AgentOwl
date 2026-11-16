package service

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestAutomaticReplyNeedsObservedSendForCurrentConfiguration(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	connector, channel := supportLarkFixture(t, s)
	supportConfig(t, s, "policies", Data{"name": "auto reply", "kind": "reply_send", "mode": "auto", "enabled": true, "max_tokens": 1000, "max_files": 10, "max_seconds": 60}, nil)
	action := Data{"channel_id": channel, "answer_verified": true, "evidence": []Data{{"knowledge_id": ID()}}}
	assertMode := func(want string) {
		t.Helper()
		mode, _, err := s.evaluatePolicy(ctx, s.DB, "reply_send", action)
		if err != nil || mode != want {
			t.Fatalf("policy mode %q, want %q: %v", mode, want, err)
		}
	}
	assertMode("manual")
	if _, err := s.DB.Exec(ctx, `UPDATE configs SET data=data||'{"capabilities":{"send":{"status":"verified"}}}'::jsonb WHERE id=$1`, connector); err != nil {
		t.Fatal(err)
	}
	assertMode("manual")
	op := ID()
	if _, err := s.DB.Exec(ctx, `INSERT INTO external_operations(id,kind,subject_id,dedupe_key,target,request,status) VALUES($1,'notify','test',$2,$3,$4,'succeeded')`, op, "capability-test:"+op, JSON(Data{"channel_id": channel}), JSON(Data{"channel_version": 1, "connector_version": 1})); err != nil {
		t.Fatal(err)
	}
	assertMode("auto")
	if _, err := s.DB.Exec(ctx, "UPDATE configs SET version=version+1 WHERE id=$1", channel); err != nil {
		t.Fatal(err)
	}
	assertMode("manual")
}

func TestReplyPolicyIgnoresStaleRepositoryAndTestFields(t *testing.T) {
	s, a := testService(t)
	ctx := context.Background()
	_, channel := supportLarkFixture(t, s)
	repo := supportConfig(t, s, "repositories", Data{"name": "repo", "owner": "team", "repo": "auth", "enabled": true, "base_branch": "main", "allowed_paths": []string{"auth"}}, nil)
	policy := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.SaveConfig(ctx, tx, a, "policies", "", Data{"name": "auto reply", "kind": "reply_send", "mode": "auto", "enabled": true, "channel_ids": []string{channel}, "repository_ids": []string{repo}, "allowed_paths": []string{"auth"}, "require_tests": true, "max_tokens": 1000, "max_files": 10, "max_seconds": 60})
	})
	if len(Strings(policy, "repository_ids")) != 0 || len(Strings(policy, "allowed_paths")) != 0 || Bool(policy, "require_tests") {
		t.Fatalf("reply policy kept irrelevant gates: %v", policy)
	}
	op := ID()
	if _, err := s.DB.Exec(ctx, `INSERT INTO external_operations(id,kind,subject_id,dedupe_key,target,request,status) VALUES($1,'notify','test',$2,$3,$4,'succeeded')`, op, "capability-test:"+op, JSON(Data{"channel_id": channel}), JSON(Data{"channel_version": 1, "connector_version": 1})); err != nil {
		t.Fatal(err)
	}
	mode, _, err := s.evaluatePolicy(ctx, s.DB, "reply_send", Data{"channel_id": channel, "answer_verified": true, "evidence": []Data{{"knowledge_id": ID()}}, "max_tokens": 50, "timeout_seconds": 30, "file_count": 0})
	if err != nil || mode != "auto" {
		t.Fatalf("stale repository/test fields blocked reply policy: %q %v", mode, err)
	}
}

func TestKnowledgePolicyIgnoresStaleScopeFields(t *testing.T) {
	s, a := testService(t)
	ctx := context.Background()
	_, channel := supportLarkFixture(t, s)
	repo := supportConfig(t, s, "repositories", Data{"name": "repo", "owner": "team", "repo": "auth", "enabled": true, "base_branch": "main", "allowed_paths": []string{"auth"}}, nil)
	policy := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.SaveConfig(ctx, tx, a, "policies", "", Data{"name": "auto knowledge", "kind": "knowledge_publish", "mode": "auto", "enabled": true, "channel_ids": []string{channel}, "repository_ids": []string{repo}, "allowed_paths": []string{"auth"}, "require_tests": true, "max_tokens": 1000, "max_files": 20, "max_seconds": 60})
	})
	if len(Strings(policy, "channel_ids")) != 0 || len(Strings(policy, "repository_ids")) != 0 || len(Strings(policy, "allowed_paths")) != 0 || Bool(policy, "require_tests") {
		t.Fatalf("knowledge policy kept irrelevant gates: %v", policy)
	}
	k := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.SaveKnowledge(ctx, tx, a, "", Data{"title": "Policy evidence", "body": "Verified answer"})
	})
	ap := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.KnowledgeAction(ctx, tx, a, Str(k, "id"), "submit", Data{"expected_version": 1})
	})
	if Str(ap, "status") != "approved" || Str(ap, "policy_id") != Str(policy, "id") {
		t.Fatalf("knowledge policy did not auto approve: %v", ap)
	}
}
