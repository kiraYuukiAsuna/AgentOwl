package service

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"agentowl/backend/internal/runner"
	"github.com/jackc/pgx/v5"
)

func TestFetchSnapshotUsesConfiguredQuota(t *testing.T) {
	s, actor := testService(t)
	ctx := context.Background()
	body := make([]byte, (32<<20)+1)
	hash := sha1.New()
	fmt.Fprintf(hash, "blob %d\x00", len(body))
	_, _ = hash.Write(body)
	blob := hex.EncodeToString(hash.Sum(nil))
	commit, tree := strings.Repeat("a", 40), strings.Repeat("b", 40)
	encoded := base64.StdEncoding.EncodeToString(body)
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var result Data
		switch request.URL.Path {
		case "/repos/owner/repo/git/ref/heads/main":
			result = Data{"ref": "refs/heads/main", "object": Data{"sha": commit}}
		case "/repos/owner/repo/git/commits/" + commit:
			result = Data{"sha": commit, "tree": Data{"sha": tree}}
		case "/repos/owner/repo/git/trees/" + tree:
			result = Data{"sha": tree, "tree": []Data{{"path": "large.dat", "type": "blob", "mode": "100644", "sha": blob, "size": len(body)}}}
		case "/repos/owner/repo/git/blobs/" + blob:
			reads.Add(1)
			result = Data{"sha": blob, "encoding": "base64", "size": len(body), "content": encoded}
		default:
			t.Error("unexpected path", request.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(result)
	}))
	defer server.Close()
	repo := supportConfig(t, s, "repositories", Data{"owner": "owner", "repo": "repo", "base_branch": "main", "enabled": true, "config": Data{"base_url": server.URL}}, nil)
	files, _, _, err := s.fetchSnapshot(ctx, repo, "")
	if err != nil || len(files) != 1 || !bytes.Equal(files[0].Data, body) {
		t.Fatal("snapshot retained the former 32 MiB ceiling", err)
	}
	mutateTest(t, s, actor, func(tx pgx.Tx) (Data, error) {
		settings := executionSettingsBody(0)
		settings["workspace_mb"] = 1
		return s.SaveExecutionSettings(ctx, tx, actor, settings)
	})
	if _, _, _, err = s.fetchSnapshot(ctx, repo, ""); err == nil || reads.Load() != 1 {
		t.Fatal("explicit snapshot quota did not reject before downloading the blob", reads.Load(), err)
	}
}

func TestExecutionQuotaZeroAndLargeValues(t *testing.T) {
	for _, value := range []int64{0, 1000000} {
		body := executionSettingsBody(0)
		for _, key := range []string{"workspace_mb", "max_files", "artifact_mb", "max_tool_calls", "max_tokens"} {
			body[key] = value
		}
		settings, err := validateExecutionSettings(body)
		if err != nil {
			t.Fatal(value, err)
		}
		limits := executionLimits(settings)
		if limits.WorkspaceBytes != value<<20 || limits.ArtifactBytes != value<<20 || int64(limits.MaxFiles) != value || int64(limits.MaxTokens) != value || int64(limits.MaxToolCalls) != value {
			t.Fatal("execution quotas changed during conversion", limits)
		}
	}
	for key, max := range map[string]int64{"workspace_mb": math.MaxInt64 >> 20, "artifact_mb": math.MaxInt64 >> 20, "max_files": 1<<53 - 1, "max_tokens": 1<<53 - 1, "max_tool_calls": 1<<53 - 1} {
		body := executionSettingsBody(0)
		body[key] = max
		if _, err := validateExecutionSettings(body); err != nil {
			t.Fatal("representable integer rejected", key, err)
		}
		body[key] = max + 1
		if _, err := validateExecutionSettings(body); err == nil {
			t.Fatal("integer overflow or imprecise JSON number accepted", key)
		}
	}
}

func TestSnapshotAndArchiveUnlimitedQuotas(t *testing.T) {
	spec, entries := artifactFixture(t)
	spec.Limits = runner.Limits{}
	var result runner.Run
	if err := json.Unmarshal(entries[0].data, &result); err != nil {
		t.Fatal(err)
	}
	result.SpecDigest, _ = runner.SpecDigest(spec)
	entries[0].data = JSON(result)
	if _, _, _, err := decodeRunArchive(bytes.NewReader(artifactArchive(t, entries)), spec); err != nil {
		t.Fatal("zero archive quotas rejected", err)
	}
	if err := validateSnapshotQuota(spec.Files, runner.Limits{}); err != nil {
		t.Fatal(err)
	}
	if err := validateSnapshotQuota(spec.Files, runner.Limits{MaxFiles: 1}); err == nil {
		t.Fatal("configured file quota ignored")
	}
	if err := validateSnapshotQuota(spec.Files, runner.Limits{WorkspaceBytes: 1}); err == nil {
		t.Fatal("configured byte quota ignored")
	}
	entries[len(entries)-1].data = []byte("changed")
	if _, _, _, err := decodeRunArchive(bytes.NewReader(artifactArchive(t, entries)), spec); err == nil {
		t.Fatal("unlimited archive bypassed integrity validation")
	}
}

func TestArtifactsBeyondFormerFileAndByteLimits(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	files := make([]runner.File, 50001)
	for i := range files {
		files[i] = runner.File{Path: fmt.Sprintf("file-%d", i), Mode: 0644}
	}
	var snapshot Data
	if err := s.transaction(ctx, func(tx pgx.Tx) error {
		var err error
		snapshot, err = s.storeSnapshot(ctx, tx, "", "", "", files)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	storedFiles, err := s.readSnapshot(ctx, Str(snapshot, "id"))
	if err != nil || len(storedFiles) != len(files) {
		t.Fatal("snapshot retained the former 50000-file ceiling", err)
	}
	body := make([]byte, (128<<20)+1)
	body[len(body)-1] = 1
	var artifact Data
	if err = s.transaction(ctx, func(tx pgx.Tx) error {
		artifact, err = s.storeArtifact(ctx, tx, "", "", "", "large.bin", "application/octet-stream", body)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.readArtifact(ctx, Str(artifact, "id"))
	if err != nil || !bytes.Equal(got, body) {
		t.Fatal("artifact retained the former 128 MiB ceiling", err)
	}
}

func TestUnlimitedRunTokensRetainDailyAccounting(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	used, err := s.ReserveBudget(ctx, "already-used", 123)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SettleBudget(ctx, used, 123); err != nil {
		t.Fatal(err)
	}
	spec := runner.RunSpec{RunID: ID(), Stage: "edit", Limits: runner.Limits{MaxTokens: 0}}
	if err = s.transaction(ctx, func(tx pgx.Tx) error { return s.reserveRunTokenBudget(ctx, tx, &spec) }); err != nil {
		t.Fatal(err)
	}
	if spec.Limits.MaxTokens != 0 || spec.DailyTokenBudget != defaultDailyTokens-123 {
		t.Fatal("unlimited task quota was replaced or lost daily allocation", spec.Limits.MaxTokens, spec.DailyTokenBudget)
	}
	row, err := one(ctx, s.DB, "SELECT to_jsonb(b) FROM budget_reservations b WHERE id=$1", spec.RunID)
	if err != nil || Num(row, "amount") != spec.DailyTokenBudget || row["actual"] != nil || Str(row, "status") != "reserved" {
		t.Fatal("daily balance was not reserved", err)
	}
	if _, err = s.ReserveBudget(ctx, "competing", 1); err == nil {
		t.Fatal("concurrent work reused the reserved daily budget")
	}
	testSpec := runner.RunSpec{RunID: ID(), Stage: "test"}
	if err = s.transaction(ctx, func(tx pgx.Tx) error { return s.reserveRunTokenBudget(ctx, tx, &testSpec) }); err != nil || testSpec.DailyTokenBudget != 0 {
		t.Fatal("non-model test consumed daily budget", err)
	}
	if err = s.SettleBudget(ctx, spec.RunID, 456); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReserveBudget(ctx, "released", defaultDailyTokens-123-456); err != nil {
		t.Fatal("actual model usage did not release unused allocation", err)
	}
}

func TestUnlimitedPreparedRunBindsDailyAllocation(t *testing.T) {
	s, _, _, step, run, _ := queuedSettingsRun(t)
	ctx := context.Background()
	approval, err := locked(ctx, s.DB, "approvals", Str(step, "approval_id"))
	if err != nil {
		t.Fatal(err)
	}
	for _, frozen := range []Data{Object(step, "input"), Object(approval, "action")} {
		for _, key := range []string{"workspace_bytes", "max_files", "artifact_bytes", "max_tool_calls", "max_tokens"} {
			if Num(Object(frozen, "limits"), key) != 0 {
				t.Fatal("unlimited configuration changed during approval preparation", key)
			}
		}
	}
	spec, err := s.runSpec(ctx, run)
	if err != nil || spec.Limits.MaxTokens != 0 || spec.DailyTokenBudget != defaultDailyTokens {
		t.Fatal("frozen unlimited run lost its independent daily allocation", spec.DailyTokenBudget, err)
	}
	digest, _ := runner.SpecDigest(spec)
	spec.DailyTokenBudget--
	changed, _ := runner.SpecDigest(spec)
	if digest != Str(run, "spec_digest") || digest == changed {
		t.Fatal("daily allocation was not bound to the run digest")
	}
}

func TestFinitePoliciesDoNotApproveUnlimitedDevelopment(t *testing.T) {
	f := newDevelopmentApprovalFixture(t, "policy", 30)
	f.policy(t, "edit", "auto")
	base := Data{"repository_id": Str(f.repo, "id"), "timeout_seconds": 30, "file_count": 1, "max_tokens": 1, "limits": Data{"max_files": 1}}
	for _, unlimited := range []string{"tokens", "files", "larger_files"} {
		action := decodeData(base)
		switch unlimited {
		case "tokens":
			action["max_tokens"] = 0
		case "files":
			Object(action, "limits")["max_files"] = 0
		case "larger_files":
			Object(action, "limits")["max_files"] = 50001
		}
		mode, _, err := f.s.evaluatePolicy(context.Background(), f.s.DB, "edit", action)
		if err != nil || mode != "manual" {
			t.Fatal("finite policy approved an unbounded task", unlimited, mode, err)
		}
	}
}
