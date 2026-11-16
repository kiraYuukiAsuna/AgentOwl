package integration

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestLarkDocumentReadRejectsMixedRevision(t *testing.T) {
	var metadata atomic.Int32
	var changed atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			_, _ = w.Write([]byte(`{"code":0,"tenant_access_token":"test-token","expire":7200}`))
		case "/open-apis/bot/v3/info":
			_, _ = w.Write([]byte(`{"code":0,"bot":{"activate_status":2,"open_id":"ou_test","app_name":"test"}}`))
		case "/open-apis/docx/v1/documents/doc_test":
			n := metadata.Add(1)
			revision := 1
			if changed.Load() && n%2 == 0 {
				revision = 2
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"document": map[string]any{"document_id": "doc_test", "revision_id": revision, "title": "Auth"}}})
		case "/open-apis/docx/v1/documents/doc_test/raw_content":
			_, _ = w.Write([]byte(`{"code":0,"data":{"content":"auth access_token=secret-value"}}`))
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	client, e := NewLarkClient(LarkConfig{BaseURL: server.URL, AppID: "test", AppSecret: "secret"})
	if e != nil {
		t.Fatal(e)
	}
	info, e := client.BotInfo(context.Background())
	if e != nil || info.OpenID != "ou_test" {
		t.Fatal(info, e)
	}
	doc, e := client.ReadDocument(context.Background(), "doc_test")
	if e != nil || doc.RevisionID != 1 || strings.Contains(doc.Content, "secret-value") {
		t.Fatal(doc, e)
	}
	changed.Store(true)
	_, e = client.ReadDocument(context.Background(), "doc_test")
	if !errors.Is(e, ErrInvalidInput) {
		t.Fatalf("mixed revision accepted: %v", e)
	}
}

func TestGitHubIdentityAndReviewReadAreScoped(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatal("read adapter wrote remotely")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/owner/repo":
			_, _ = w.Write([]byte(`{"id":1,"full_name":"owner/repo","private":true,"default_branch":"main"}`))
		case "/user":
			_, _ = w.Write([]byte(`{"id":7,"login":"sender","type":"User"}`))
		case "/repos/owner/repo/pulls/12/reviews":
			if r.URL.Query().Get("page") != "2" || r.URL.Query().Get("per_page") != "100" {
				t.Error("missing review pagination")
			}
			_, _ = w.Write([]byte(`[{"id":8,"state":"APPROVED","user":{"id":7,"login":"reviewer"},"commit_id":"0123456789012345678901234567890123456789","submitted_at":"2026-10-27T00:00:00Z"}]`))
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	client, e := NewGitHubClient(GitHubConfig{BaseURL: server.URL, Owner: "owner", Repo: "repo", Token: "test-token"})
	if e != nil {
		t.Fatal(e)
	}
	repo, e := client.GetRepository(context.Background())
	if e != nil || repo.FullName != "owner/repo" {
		t.Fatal(repo, e)
	}
	user, e := client.AuthenticatedUser(context.Background())
	if e != nil || user.ID != 7 {
		t.Fatal(user, e)
	}
	reviews, e := client.ListPullRequestReviews(context.Background(), 12, 2)
	if e != nil || len(reviews) != 1 || reviews[0].State != "APPROVED" {
		t.Fatal(reviews, e)
	}
}

func TestRedactionHandlesRawLarkTokensAndWholeCookieHeaders(t *testing.T) {
	input := "Cookie: session=first-value; other=second-value\nAuthorization: Basic dXNlcjpwYXNz\nhttps://user:password@example.test/path\nu-abcdefghijklmnopqrstuvwxyz123456\n-----BEGIN PRIVATE KEY-----\ntruncated-private-material"
	redacted := Redact(input)
	for _, secret := range []string{"first-value", "second-value", "dXNlcjpwYXNz", "password", "abcdefghijklmnopqrstuvwxyz123456", "truncated-private-material"} {
		if strings.Contains(redacted, secret) {
			t.Fatalf("sensitive material remains: %s", secret)
		}
	}
}
