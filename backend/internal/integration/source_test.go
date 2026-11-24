package integration

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSourceAdapterReadAndUnknownWrite(t *testing.T) {
	for _, provider := range []string{"lark", "github"} {
		t.Run(provider, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal" {
					_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "tenant_access_token": "test-token", "expire": 7200})
					return
				}
				if r.Method == "POST" {
					w.WriteHeader(503)
					_, _ = w.Write([]byte(`{"message":"temporary failure"}`))
					return
				}
				if provider == "lark" {
					_, _ = w.Write([]byte(`{"code":0,"data":{"items":[{"message_id":"message-1","chat_id":"chat-1","message_type":"text","body":{"content":"{\"text\":\"reproduce defect\"}"},"create_time":"1700000000000","sender":{"id":"user-1","sender_type":"user"}}],"has_more":false}}`))
				} else {
					_, _ = w.Write([]byte(`[{"id":1,"number":2,"title":"defect","body":"reproduce defect","user":{"id":3,"login":"user","type":"User"},"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-02T00:00:00Z"}]`))
				}
			}))
			defer server.Close()
			var source Source
			if provider == "lark" {
				c, e := NewLarkClient(LarkConfig{BaseURL: server.URL, AppID: "app", AppSecret: "secret", AllowHTTP: true})
				if e != nil {
					t.Fatal(e)
				}
				source = LarkSource{c}
			} else {
				c, e := NewGitHubClient(GitHubConfig{BaseURL: server.URL, Owner: "owner", Repo: "repo", Token: "test", AllowHTTP: true})
				if e != nil {
					t.Fatal(e)
				}
				source = GitHubSource{c}
			}
			page, e := source.ReadThread(context.Background(), SourceRead{ChannelID: "chat-1", Repository: "owner/repo"})
			if e != nil {
				t.Fatal(e)
			}
			if len(page.Events) != 1 || page.Events[0].MessageID == "" || page.Events[0].SourceRevision == "" || page.Events[0].Text == "" || page.NextCursor != "" {
				t.Fatal(page)
			}
			_, e = source.Send(context.Background(), SourceWrite{OperationID: "operation-1", ChannelID: "chat-1", Issue: 2, Body: "response"})
			var api *APIError
			if !errors.As(e, &api) || !api.OutcomeUnknown {
				t.Fatal("uncertain write treated as safe retry", e)
			}
		})
	}
}

func TestLarkMentionPostRender(t *testing.T) {
	raw := string(renderLarkPostWithMention("ou_user", `<at user_id="ou_user"></at> Answer body`))
	if !strings.Contains(raw, `"tag":"at"`) || !strings.Contains(raw, `"user_id":"ou_user"`) || strings.Contains(raw, `<at user_id`) {
		t.Fatalf("post mention render mismatch: %s", raw)
	}
}
