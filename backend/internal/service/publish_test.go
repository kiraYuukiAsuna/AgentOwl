package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"agentowl/backend/internal/integration"
	"agentowl/backend/internal/runner"
	"github.com/jackc/pgx/v5"
)

type publicationFake struct {
	blobs     [][]byte
	entries   []integration.GitTreeEntry
	treeCalls int
}

type publicationRefFake struct {
	head                                             string
	pr                                               integration.GitHubPullRequest
	createdRefs, updatedRefs, createdPRs, updatedPRs int
	updateError                                      error
}

func (f *publicationRefFake) GetRef(_ context.Context, ref string) (integration.GitRef, error) {
	if ref == "heads/main" {
		return integration.GitRef{Object: integration.GitObject{SHA: "main-head"}}, nil
	}
	if f.head == "" {
		return integration.GitRef{}, &integration.APIError{StatusCode: 404}
	}
	return integration.GitRef{Object: integration.GitObject{SHA: f.head}}, nil
}
func (f *publicationRefFake) CreateRef(_ context.Context, _ string, sha string) (integration.GitRef, error) {
	f.createdRefs++
	f.head = sha
	return integration.GitRef{Object: integration.GitObject{SHA: sha}}, nil
}
func (f *publicationRefFake) UpdateRef(_ context.Context, _ string, sha string) (integration.GitRef, error) {
	f.updatedRefs++
	if f.updateError != nil {
		return integration.GitRef{}, f.updateError
	}
	f.head = sha
	f.pr.Head.SHA = sha
	return integration.GitRef{Object: integration.GitObject{SHA: sha}}, nil
}
func (f *publicationRefFake) GetPullRequest(context.Context, int) (integration.GitHubPullRequest, error) {
	return f.pr, nil
}
func (f *publicationRefFake) CreatePullRequest(_ context.Context, r integration.CreatePullRequestRequest) (integration.GitHubPullRequest, error) {
	f.createdPRs++
	f.pr = integration.GitHubPullRequest{ID: 1, Number: 7, Body: r.Body, Title: r.Title, State: "open", Draft: r.Draft, HTMLURL: "https://github.com/o/r/pull/7", Head: integration.GitHubBranch{Ref: r.Head, SHA: f.head}, Base: integration.GitHubBranch{Ref: r.Base}}
	return f.pr, nil
}
func (f *publicationRefFake) UpdatePullRequest(_ context.Context, _ int, r integration.UpdatePullRequestRequest) (integration.GitHubPullRequest, error) {
	f.updatedPRs++
	if r.Title != nil {
		f.pr.Title = *r.Title
	}
	if r.Body != nil {
		f.pr.Body = *r.Body
	}
	return f.pr, nil
}

func TestPublicationCreatesThenUpdatesOwnedPR(t *testing.T) {
	ctx := context.Background()
	fake := &publicationRefFake{}
	request := Data{"branch": "agentowl/task", "base_commit": "main-head", "title": "first fix", "body": "approved description"}
	checkpoints := []string{}
	checkpoint := func(p Data) error { checkpoints = append(checkpoints, Str(p, "phase")); return nil }
	prior := "<!-- agentowl-operation:first -->"
	next := "<!-- agentowl-operation:revision -->"
	pr, e := publishReferenceAndPR(ctx, fake, request, "main", "main-head", "first-head", prior, "", checkpoint)
	if e != nil || !publicationMatches(pr, "agentowl/task", "main", "first-head", prior) || !pr.Draft || fake.createdRefs != 1 || fake.createdPRs != 1 {
		t.Fatalf("initial publish failed: %#v %v", pr, e)
	}
	request["pr_number"], request["expected_branch_head"], request["base_commit"], request["title"] = 7, "first-head", "first-head", "revised fix"
	pr, e = publishReferenceAndPR(ctx, fake, request, "main", "main-head", "revised-head", next, prior, checkpoint)
	if e != nil || !publicationMatches(pr, "agentowl/task", "main", "revised-head", next) || !strings.Contains(pr.Body, prior) || pr.Title != "revised fix" || fake.updatedRefs != 1 || fake.updatedPRs != 1 || fake.createdPRs != 1 || fake.createdRefs != 1 {
		t.Fatalf("PR revision failed: %#v %v", pr, e)
	}
	if strings.Join(checkpoints, ",") != "branch_create,pr_create,branch_update,pr_update" {
		t.Fatalf("writes missing checkpoint: %v", checkpoints)
	}
}

func TestPublicationRecoversConfirmedOwnedBranchWithoutPR(t *testing.T) {
	fake := &publicationRefFake{head: "owned-head"}
	request := Data{"branch": "agentowl/task", "base_commit": "owned-head", "expected_branch_head": "owned-head", "title": "retry reviewed fix", "body": "new approved evidence"}
	prior := "<!-- agentowl-operation:known-failure -->"
	marker := "<!-- agentowl-operation:new-approval -->"
	pr, e := publishReferenceAndPR(context.Background(), fake, request, "main", "main-head", "new-head", marker, prior, func(Data) error { return nil })
	if e != nil || fake.updatedRefs != 1 || fake.createdRefs != 0 || fake.createdPRs != 1 || fake.updatedPRs != 0 || !publicationMatches(pr, "agentowl/task", "main", "new-head", marker) {
		t.Fatalf("owned branch recovery failed: %#v %v", pr, e)
	}
}

func TestPublicationRejectsConcurrentBranchChangesAndUnknownUpdate(t *testing.T) {
	prior := "<!-- agentowl-operation:first -->"
	for _, test := range []struct {
		name, head  string
		updateError error
	}{
		{"changed", "concurrent-head", nil},
		{"ambiguous", "approved-head", &integration.APIError{OutcomeUnknown: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &publicationRefFake{head: test.head, updateError: test.updateError, pr: integration.GitHubPullRequest{ID: 1, Number: 7, Body: prior, State: "open", HTMLURL: "https://github.com/o/r/pull/7", Head: integration.GitHubBranch{Ref: "agentowl/task", SHA: "approved-head"}, Base: integration.GitHubBranch{Ref: "main"}}}
			request := Data{"branch": "agentowl/task", "base_commit": "approved-head", "expected_branch_head": "approved-head", "pr_number": 7}
			_, e := publishReferenceAndPR(context.Background(), fake, request, "main", "main-head", "new-head", "<!-- agentowl-operation:revision -->", prior, func(Data) error { return nil })
			if e == nil || fake.createdRefs != 0 || fake.createdPRs != 0 || fake.updatedPRs != 0 {
				t.Fatalf("unsafe branch change proceeded: %v", e)
			}
			if test.name == "changed" && fake.updatedRefs != 0 {
				t.Fatal("changed branch overwritten")
			}
			if test.name == "ambiguous" && fake.updatedRefs != 1 {
				t.Fatal("ambiguous update was retried")
			}
		})
	}
}

func (f *publicationFake) CreateBlob(_ context.Context, b []byte) (integration.GitObject, error) {
	f.blobs = append(f.blobs, append([]byte(nil), b...))
	return integration.GitObject{SHA: "0123456789012345678901234567890123456789"}, nil
}
func (f *publicationFake) CreateTree(_ context.Context, _ string, e []integration.GitTreeEntry) (integration.GitTree, error) {
	f.entries = e
	f.treeCalls++
	return integration.GitTree{SHA: "abcdefabcdefabcdefabcdefabcdefabcdefabcd"}, nil
}

func TestPublicationTreePreservesBinaryDeletionAndMode(t *testing.T) {
	f := &publicationFake{}
	checks := 0
	before := []runner.File{{Path: "delete.go", Mode: 0644, Data: []byte("old")}, {Path: "run", Mode: 0644, Data: []byte("same")}, {Path: "keep", Mode: 0644, Data: []byte("keep")}}
	after := []runner.File{{Path: "run", Mode: 0755, Data: []byte("same")}, {Path: "keep", Mode: 0644, Data: []byte("keep")}, {Path: "asset.bin", Mode: 0644, Data: []byte{0, 255, 1}}}
	_, e := createPublicationTree(context.Background(), f, "base", before, after, func() error { checks++; return nil })
	if e != nil {
		t.Fatal(e)
	}
	if checks != 3 || f.treeCalls != 1 || len(f.blobs) != 2 || len(f.entries) != 3 {
		t.Fatalf("unexpected calls: checks=%d blobs=%d entries=%d", checks, len(f.blobs), len(f.entries))
	}
	if string(f.blobs[0]) != string([]byte{0, 255, 1}) || f.entries[0].Path != "asset.bin" || !f.entries[1].Delete || f.entries[2].Mode != "100755" {
		t.Fatalf("tree lost snapshot semantics: %#v", f.entries)
	}
}
func TestPublicationFencePreventsEveryWrite(t *testing.T) {
	f := &publicationFake{}
	revoked := errors.New("revoked")
	_, e := createPublicationTree(context.Background(), f, "base", nil, []runner.File{{Path: "x", Mode: 0644, Data: []byte("new")}}, func() error { return revoked })
	if !errors.Is(e, revoked) || len(f.blobs) != 0 || f.treeCalls != 0 {
		t.Fatalf("write occurred after fence: %v", e)
	}
	f = &publicationFake{}
	checks := 0
	_, e = createPublicationTree(context.Background(), f, "base", nil, []runner.File{{Path: "x", Mode: 0644, Data: []byte("new")}}, func() error {
		checks++
		if checks == 2 {
			return revoked
		}
		return nil
	})
	if !errors.Is(e, revoked) || len(f.blobs) != 1 || f.treeCalls != 0 {
		t.Fatalf("tree written after revocation: %v", e)
	}
}
func TestPublicationRejectsInvalidOrEmptySnapshot(t *testing.T) {
	f := &publicationFake{}
	file := runner.File{Path: "x", Mode: 0644, Data: []byte("x")}
	for _, after := range [][]runner.File{{file}, {file, file}, {{Path: "../escape", Mode: 0644}}} {
		if _, e := createPublicationTree(context.Background(), f, "base", []runner.File{file}, after, func() error { return nil }); e == nil {
			t.Fatal("invalid or unchanged snapshot accepted")
		}
	}
	if len(f.blobs) != 0 || f.treeCalls != 0 {
		t.Fatal("invalid snapshot wrote Git objects")
	}
}
func TestPublicationReconcileRequiresExactEvidence(t *testing.T) {
	pr := integration.GitHubPullRequest{ID: 1, Number: 2, HTMLURL: "https://github.com/o/r/pull/2", Body: "body <!-- agentowl-operation:op -->", Head: integration.GitHubBranch{Ref: "agentowl/task", SHA: "approved"}, Base: integration.GitHubBranch{Ref: "main"}}
	marker := "<!-- agentowl-operation:op -->"
	if !publicationMatches(pr, "agentowl/task", "main", "approved", marker) {
		t.Fatal("exact publication rejected")
	}
	for _, change := range []func(*integration.GitHubPullRequest){func(p *integration.GitHubPullRequest) { p.Head.SHA = "other" }, func(p *integration.GitHubPullRequest) { p.Head.Ref = "other" }, func(p *integration.GitHubPullRequest) { p.Base.Ref = "other" }, func(p *integration.GitHubPullRequest) { p.Body = "forged" }, func(p *integration.GitHubPullRequest) { p.HTMLURL = "javascript:alert(1)" }} {
		copy := pr
		change(&copy)
		if publicationMatches(copy, "agentowl/task", "main", "approved", marker) {
			t.Fatal("mismatched remote result accepted")
		}
	}
}
func TestExternalOperationCASAndMissingApproval(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	id := ID()
	if _, e := s.DB.Exec(ctx, "INSERT INTO external_operations(id,kind,subject_id,dedupe_key,status,target,request) VALUES($1::uuid,'notify','case',$1::text,'dispatching','{}','{}')", id); e != nil {
		t.Fatal(e)
	}
	op, e := one(ctx, s.DB, "SELECT to_jsonb(o) FROM external_operations o WHERE id=$1", id)
	if e != nil {
		t.Fatal(e)
	}
	stale := Data{}
	for k, v := range op {
		stale[k] = v
	}
	if e = s.externalCheckpoint(ctx, op, Data{"phase": "first"}); e != nil {
		t.Fatal(e)
	}
	if e = s.externalCheckpoint(ctx, stale, Data{"phase": "stale"}); !errors.Is(e, Conflict) {
		t.Fatalf("stale checkpoint accepted: %v", e)
	}
	if e = s.finishExternal(ctx, stale, "succeeded", Data{"remote_id": "wrong"}, ""); !errors.Is(e, Conflict) {
		t.Fatalf("stale completion accepted: %v", e)
	}
	job := ID()
	if _, e = s.DB.Exec(ctx, "INSERT INTO jobs(id,kind,dedupe_key,payload,status,lease_token,lease_until) VALUES($1::uuid,'dispatch_external',$1::text,'{}','running','new',now()+interval '1 minute')", job); e != nil {
		t.Fatal(e)
	}
	lost := context.WithValue(ctx, leaseKey{}, Data{"id": job, "lease_token": "old"})
	if e = s.externalCheckpoint(lost, op, Data{"phase": "lost"}); e == nil {
		t.Fatal("stale job lease updated operation")
	}
	id = ID()
	if _, e = s.DB.Exec(ctx, "INSERT INTO external_operations(id,kind,subject_id,dedupe_key,target,request) VALUES($1::uuid,'publish_pr','task',$1::text,'{}','{}')", id); e != nil {
		t.Fatal(e)
	}
	if e = s.dispatchExternal(ctx, id); !errors.Is(e, Forbidden) {
		t.Fatalf("publish without approval accepted: %v", e)
	}
	var status string
	if e = s.DB.QueryRow(ctx, "SELECT status FROM external_operations WHERE id=$1", id).Scan(&status); e != nil || status != "pending" {
		t.Fatalf("unapproved operation claimed: %s %v", status, e)
	}
}

func TestNotifyFailureCreatesVisibleCaseEvent(t *testing.T) {
	s, a := testService(t)
	ctx := context.Background()
	_, channel := supportLarkFixture(t, s)
	c := supportCase(t, s, a, channel)
	for _, tc := range []struct {
		status, eventType, body string
	}{
		{"failed", "notification_failed", "通知发送失败"},
		{"outcome_unknown", "notification_outcome_unknown", "通知结果待核对"},
	} {
		t.Run(tc.status, func(t *testing.T) {
			id := ID()
			if _, e := s.DB.Exec(ctx, "INSERT INTO external_operations(id,kind,subject_id,dedupe_key,status,target,request) VALUES($1::uuid,'notify',$2,$1::text,'dispatching',$3,$4)", id, Str(c, "id"), JSON(Data{"channel_id": channel}), JSON(Data{"case_id": Str(c, "id"), "body": "notify"})); e != nil {
				t.Fatal(e)
			}
			op, e := one(ctx, s.DB, "SELECT to_jsonb(o) FROM external_operations o WHERE id=$1", id)
			if e != nil {
				t.Fatal(e)
			}
			if e = s.finishExternal(ctx, op, tc.status, Data{}, "forbidden: no permission"); e != nil {
				t.Fatal(e)
			}
			event, e := one(ctx, s.DB, "SELECT to_jsonb(e) FROM case_events e WHERE case_id=$1 AND type=$2 ORDER BY created_at DESC LIMIT 1", Str(c, "id"), tc.eventType)
			if e != nil || !strings.Contains(Str(event, "body"), tc.body) || !strings.Contains(Str(event, "body"), "forbidden") || Str(Object(event, "metadata"), "operation_id") != id || Str(Object(event, "metadata"), "status") != tc.status {
				t.Fatalf("missing visible notification failure event: %#v %v", event, e)
			}
		})
	}
}

func TestExternalWriteRetryIsConfigurable(t *testing.T) {
	s, a := testService(t)
	ctx := context.Background()
	mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		settings := executionSettingsBody(0)
		settings["external_retry_attempts"], settings["external_retry_delay_seconds"] = 1, 1
		return s.SaveExecutionSettings(ctx, tx, a, settings)
	})
	_, channel := supportLarkFixture(t, s)
	c := supportCase(t, s, a, channel)
	id := ID()
	if _, e := s.DB.Exec(ctx, "INSERT INTO external_operations(id,kind,subject_id,dedupe_key,status,target,request) VALUES($1::uuid,'notify',$2,$1::text,'dispatching',$3,$4)", id, Str(c, "id"), JSON(Data{"channel_id": channel}), JSON(Data{"case_id": Str(c, "id"), "body": "notify"})); e != nil {
		t.Fatal(e)
	}
	op, e := one(ctx, s.DB, "SELECT to_jsonb(o) FROM external_operations o WHERE id=$1", id)
	if e != nil {
		t.Fatal(e)
	}
	transient := &integration.APIError{Provider: "lark", StatusCode: 503, OutcomeUnknown: true}
	if e = s.externalFailure(ctx, op, transient); e != nil {
		t.Fatal(e)
	}
	current, e := locked(ctx, s.DB, "external_operations", id)
	if e != nil || Str(current, "status") != "pending" || Num(current, "retry_attempts") != 1 {
		t.Fatalf("external write was not scheduled for retry: %v %v", current, e)
	}
	if supportCount(t, s, "SELECT count(*) FROM jobs WHERE kind='dispatch_external' AND dedupe_key=$1", "external-retry:"+id+":1") != 1 {
		t.Fatal("retry dispatch job missing")
	}
	if supportCount(t, s, "SELECT count(*) FROM case_events WHERE case_id=$1 AND type LIKE 'notification_%'", Str(c, "id")) != 0 {
		t.Fatal("retryable failure created visible terminal notification event")
	}
	if _, e = s.DB.Exec(ctx, "UPDATE external_operations SET status='dispatching' WHERE id=$1", id); e != nil {
		t.Fatal(e)
	}
	current, e = locked(ctx, s.DB, "external_operations", id)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.externalFailure(ctx, current, transient); e != nil {
		t.Fatal(e)
	}
	current, e = locked(ctx, s.DB, "external_operations", id)
	if e != nil || Str(current, "status") != "outcome_unknown" || Num(current, "retry_attempts") != 1 {
		t.Fatalf("external write did not stop after configured retries: %v %v", current, e)
	}
	if supportCount(t, s, "SELECT count(*) FROM case_events WHERE case_id=$1 AND type='notification_outcome_unknown'", Str(c, "id")) != 1 {
		t.Fatal("final unknown result was not made visible")
	}
}
