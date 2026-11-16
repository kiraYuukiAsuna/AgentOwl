package integration

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Error(err)
	}
}
func mustLark(t *testing.T, cfg LarkConfig) *LarkClient {
	t.Helper()
	l, err := NewLarkClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return l
}
func mustGitHub(t *testing.T, cfg GitHubConfig) *GitHubClient {
	t.Helper()
	g, err := NewGitHubClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return g
}
func mustModel(t *testing.T, cfg ModelConfig) *ModelClient {
	t.Helper()
	m, err := NewModelClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func signedLark(body []byte, key string, now time.Time) http.Header {
	ts := strconv.FormatInt(now.Unix(), 10)
	nonce := "test-nonce"
	h := sha256.New()
	h.Write([]byte(ts + nonce + key))
	h.Write(body)
	return http.Header{"X-Lark-Request-Timestamp": {ts}, "X-Lark-Request-Nonce": {nonce}, "X-Lark-Signature": {hex.EncodeToString(h.Sum(nil))}}
}
func signedCardAction(body []byte, token string, now time.Time) http.Header {
	ts := strconv.FormatInt(now.Unix(), 10)
	nonce := "test-nonce"
	h := sha1.New()
	h.Write([]byte(ts + nonce + token))
	h.Write(body)
	return http.Header{"X-Lark-Request-Timestamp": {ts}, "X-Lark-Request-Nonce": {nonce}, "X-Lark-Signature": {hex.EncodeToString(h.Sum(nil))}}
}
func larkFixture(kind string, event any) []byte {
	raw, _ := json.Marshal(map[string]any{"schema": "2.0", "header": map[string]any{"event_id": "ev1", "event_type": kind, "app_id": "app", "tenant_key": "tenant", "token": "verify", "create_time": "1793088000000000"}, "event": event})
	return raw
}
func messageFixture() []byte {
	return larkFixture("im.message.receive_v1", map[string]any{"sender": map[string]any{"sender_id": map[string]string{"open_id": "ou_user"}, "sender_type": "user", "tenant_key": "external-tenant"}, "message": map[string]string{"message_id": "om_1", "chat_id": "oc_1", "root_id": "om_root", "parent_id": "om_parent", "thread_id": "omt_1", "create_time": "1793088000000", "message_type": "text", "content": `{"text":"login fails access_token=do-not-store"}`}})
}

func TestLarkWebhookVerificationAndIdentity(t *testing.T) {
	now := time.Unix(1793088000, 0)
	l := mustLark(t, LarkConfig{AppID: "app", TenantKey: "tenant", VerificationToken: "verify", EncryptKey: "encrypt", Clock: func() time.Time { return now }})
	body := messageFixture()
	headers := signedLark(body, "encrypt", now)
	got, err := l.ParseWebhook(headers, body)
	if err != nil || got.Event == nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Event.ActorID != "ou_user" || got.Event.ActorTenantID != "external-tenant" || got.Event.TenantID != "tenant" || got.Event.ThreadID != "omt_1" || strings.Contains(got.Event.Text, "do-not-store") {
		t.Fatalf("identity/content mismatch: %#v", got.Event)
	}
	for _, tc := range []struct {
		name    string
		body    []byte
		headers http.Header
	}{
		{"tampered", append(append([]byte(nil), body...), ' '), headers},
		{"expired", body, signedLark(body, "encrypt", now.Add(-6*time.Minute))},
		{"future", body, signedLark(body, "encrypt", now.Add(6*time.Minute))},
		{"missing_signature", body, http.Header{}},
		{"wrong_token", bytes.ReplaceAll(body, []byte(`"verify"`), []byte(`"wrong"`)), nil},
		{"wrong_app", bytes.ReplaceAll(body, []byte(`"app"`), []byte(`"another"`)), nil},
		{"wrong_tenant", bytes.ReplaceAll(body, []byte(`"tenant"`), []byte(`"another"`)), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.headers == nil {
				tc.headers = signedLark(tc.body, "encrypt", now)
			}
			if _, err := l.ParseWebhook(tc.headers, tc.body); !errors.Is(err, ErrInvalidWebhook) {
				t.Fatalf("want rejected, got %v", err)
			}
		})
	}
	challenge, err := l.ParseWebhook(nil, []byte(`{"type":"url_verification","token":"verify","challenge":"challenge-value"}`))
	if err != nil || challenge.Challenge != "challenge-value" {
		t.Fatalf("challenge: %v", err)
	}
	if _, err := l.ParseWebhook(nil, []byte(`{"type":"url_verification","token":"bad","challenge":"x"}`)); err == nil {
		t.Fatal("accepted bad challenge token")
	}
}

func encryptLarkFixture(t *testing.T, plain []byte, key string) []byte {
	t.Helper()
	digest := sha256.Sum256([]byte(key))
	block, err := aes.NewCipher(digest[:])
	if err != nil {
		t.Fatal(err)
	}
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	plain = append(append([]byte(nil), plain...), bytes.Repeat([]byte{byte(pad)}, pad)...)
	iv := []byte("0123456789abcdef")
	ciphertext := make([]byte, len(plain))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ciphertext, plain)
	raw, _ := json.Marshal(map[string]string{"encrypt": base64.StdEncoding.EncodeToString(append(iv, ciphertext...))})
	return raw
}
func TestLarkEncryptedWebhookAndBadPadding(t *testing.T) {
	now := time.Unix(1793088000, 0)
	l := mustLark(t, LarkConfig{AppID: "app", TenantKey: "tenant", VerificationToken: "verify", EncryptKey: "encrypt", Clock: func() time.Time { return now }})
	body := encryptLarkFixture(t, messageFixture(), "encrypt")
	got, err := l.ParseWebhook(signedLark(body, "encrypt", now), body)
	if err != nil || got.Event == nil || got.Event.ActorID != "ou_user" || got.Event.TenantID != "tenant" {
		t.Fatalf("encrypted event: %#v %v", got, err)
	}
	for _, raw := range []string{"bad-base64", base64.StdEncoding.EncodeToString(make([]byte, 16)), base64.StdEncoding.EncodeToString(make([]byte, 32))} {
		if _, err := decryptLark(raw, "encrypt"); err == nil {
			t.Fatal("accepted malformed encrypted payload")
		}
	}
}

func TestLarkCardAction(t *testing.T) {
	now := time.Unix(1793088000, 0)
	l := mustLark(t, LarkConfig{AppID: "app", VerificationToken: "verify", EncryptKey: "encrypt", Clock: func() time.Time { return now }})
	body := []byte(`{"open_id":"ou_card","open_message_id":"om_card","open_chat_id":"oc_card","tenant_key":"outside","token":"verify","action":{"name":"claim","value":{"operation_id":"operation","nonce":"nonce","action":"claim"}}}`)
	got, err := l.ParseCardAction(signedCardAction(body, "verify", now), body)
	if err != nil || got.CardAction == nil || got.CardAction.AppID != "app" || got.CardAction.ActorID != "ou_card" || got.CardAction.ActorTenantID != "outside" || got.CardAction.MessageID != "om_card" || got.CardAction.ChannelID != "oc_card" {
		t.Fatalf("card action: %#v %v", got, err)
	}
	for _, tc := range []struct {
		name    string
		body    []byte
		headers http.Header
	}{
		{"tampered", append(append([]byte(nil), body...), ' '), signedCardAction(body, "verify", now)},
		{"expired", body, signedCardAction(body, "verify", now.Add(-6*time.Minute))},
		{"wrong_token_signature", body, signedCardAction(body, "wrong", now)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := l.ParseCardAction(tc.headers, tc.body); !errors.Is(err, ErrInvalidWebhook) {
				t.Fatalf("want rejected, got %v", err)
			}
		})
	}
	got, err = l.ParseWebhook(signedCardAction(body, "verify", now), body)
	if err != nil || got.CardAction == nil || got.CardAction.ActorID != "ou_card" {
		t.Fatalf("card action on event webhook: %#v %v", got, err)
	}
}

func TestLarkTenantCacheHistoryAndSend(t *testing.T) {
	var tokenCalls, sendCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			tokenCalls.Add(1)
			var input map[string]string
			_ = json.NewDecoder(r.Body).Decode(&input)
			if input["app_id"] != "app" || input["app_secret"] != "secret" {
				t.Error("wrong token credentials")
			}
			writeJSON(t, w, map[string]any{"code": 0, "tenant_access_token": "cached-token", "expire": 7200})
		case "/open-apis/im/v1/messages":
			if r.Header.Get("Authorization") != "Bearer cached-token" {
				t.Error("missing authorization")
			}
			if r.Method == http.MethodGet {
				if r.URL.Query().Get("container_id_type") != "thread" || r.URL.Query().Get("page_token") != "cursor" {
					t.Error("history lost pagination/context")
				}
				writeJSON(t, w, map[string]any{"code": 0, "data": map[string]any{"items": []any{}, "has_more": true, "page_token": "next"}})
				return
			}
			sendCalls.Add(1)
			var input map[string]any
			_ = json.NewDecoder(r.Body).Decode(&input)
			if input["uuid"] != "op-123" || input["content"] != `{"text":"answer"}` {
				t.Error("send lost durable operation ID/content")
			}
			writeJSON(t, w, map[string]any{"code": 0, "data": map[string]string{"message_id": "om_sent", "chat_id": "oc"}})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	l := mustLark(t, LarkConfig{BaseURL: srv.URL, AppID: "app", AppSecret: "secret", HTTPClient: srv.Client()})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			page, err := l.History(context.Background(), HistoryRequest{ContainerType: "thread", ContainerID: "thread", PageToken: "cursor"})
			if err != nil || page.PageToken != "next" {
				t.Errorf("history %v", err)
			}
		}()
	}
	wg.Wait()
	if tokenCalls.Load() != 1 {
		t.Fatalf("token calls=%d", tokenCalls.Load())
	}
	message, err := l.SendMessage(context.Background(), SendMessageRequest{ReceiveID: "oc", MessageType: "text", Content: json.RawMessage(`{"text":"answer"}`), UUID: "op-123"})
	if err != nil || message.MessageID != "om_sent" || sendCalls.Load() != 1 {
		t.Fatalf("send: %v", err)
	}
	if _, err := l.History(context.Background(), HistoryRequest{ContainerType: "thread", ContainerID: "thread", StartTime: time.Now()}); err == nil {
		t.Fatal("thread time filter silently accepted")
	}
	if _, err := l.LookupMessage(context.Background(), ""); !errors.Is(err, ErrUnsupportedOperation) {
		t.Fatal("UUID lookup must not imply reconciliation")
	}
}

func TestLarkAddReactionUsesTenantToken(t *testing.T) {
	var tokenCalls, reactionCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			tokenCalls.Add(1)
			writeJSON(t, w, map[string]any{"code": 0, "tenant_access_token": "cached-token", "expire": 7200})
		case "/open-apis/im/v1/messages/om_test/reactions":
			reactionCalls.Add(1)
			if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer cached-token" {
				t.Error("reaction lost authorization or method")
			}
			var input map[string]any
			_ = json.NewDecoder(r.Body).Decode(&input)
			want := map[string]any{"reaction_type": map[string]any{"emoji_type": "Typing"}}
			if json.Marshal(input); !reflect.DeepEqual(input, want) {
				t.Errorf("reaction payload mismatch: %#v", input)
			}
			writeJSON(t, w, map[string]any{"code": 0, "data": map[string]string{"reaction_id": "reaction-1"}})
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	l := mustLark(t, LarkConfig{BaseURL: srv.URL, AppID: "app", AppSecret: "secret", HTTPClient: srv.Client()})
	reaction, err := l.AddReaction(context.Background(), AddReactionRequest{MessageID: "om_test", EmojiType: "Typing"})
	if err != nil || reaction.ReactionID != "reaction-1" || reactionCalls.Load() != 1 || tokenCalls.Load() != 1 {
		t.Fatalf("reaction: %#v %v token=%d calls=%d", reaction, err, tokenCalls.Load(), reactionCalls.Load())
	}
}

func githubSignature(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}
func TestGitHubWebhookSignedBodyIdentity(t *testing.T) {
	if got := githubSignature("It's a Secret to Everybody", []byte("Hello, World!")); got != "sha256=757107ea0eb2509fc211221cce984b8a37570b6d7586c22c46f4379c8b043e17" {
		t.Fatal("official HMAC fixture mismatch")
	}
	g := mustGitHub(t, GitHubConfig{Owner: "owner", Repo: "repo", WebhookSecret: "hook-secret"})
	body := []byte(`{"action":"created","repository":{"full_name":"owner/repo"},"sender":{"id":42,"login":"dev","type":"User"},"issue":{"id":10,"number":7,"updated_at":"2026-10-27T01:00:00Z"},"comment":{"id":11,"body":"auth fails","html_url":"https://github.com/owner/repo/issues/7#issuecomment-11","updated_at":"2026-10-27T01:00:00Z"}}`)
	h := http.Header{"X-Hub-Signature-256": {githubSignature("hook-secret", body)}, "X-Github-Event": {"issue_comment"}, "X-Github-Delivery": {"delivery1"}}
	first, err := g.ParseWebhook(h, body)
	if err != nil || first.Event.MessageID != "comment:11" || first.Event.ActorID != "42" {
		t.Fatalf("parse: %v", err)
	}
	h.Set("X-GitHub-Delivery", "attacker-changed-header")
	second, err := g.ParseWebhook(h, body)
	if err != nil || first.Event.ID != second.Event.ID {
		t.Fatal("delivery-header change bypasses source dedup")
	}
	if _, err := g.ParseWebhook(h, append(body, ' ')); !errors.Is(err, ErrInvalidWebhook) {
		t.Fatal("tampered webhook accepted")
	}
	wrong := bytes.ReplaceAll(body, []byte("owner/repo"), []byte("other/repo"))
	h.Set("X-Hub-Signature-256", githubSignature("hook-secret", wrong))
	if _, err := g.ParseWebhook(h, wrong); err == nil {
		t.Fatal("other repository accepted")
	}
}

func TestGitHubGitObjectsAndNonForcedUpdate(t *testing.T) {
	content := []byte("hello\n")
	sha := blobSHA(content)
	commit := strings.Repeat("a", 40)
	tree := strings.Repeat("b", 40)
	var updates atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" || r.Header.Get("X-GitHub-Api-Version") != "2022-11-28" {
			t.Error("missing GitHub headers")
		}
		switch r.URL.Path {
		case "/repos/owner/repo/git/blobs/" + sha:
			writeJSON(t, w, map[string]any{"sha": sha, "encoding": "base64", "size": len(content), "content": base64.StdEncoding.EncodeToString(content)})
		case "/repos/owner/repo/git/trees/" + tree:
			writeJSON(t, w, map[string]any{"sha": tree, "truncated": true, "tree": []any{}})
		case "/repos/owner/repo/git/refs/heads/feature#one":
			if !strings.Contains(r.RequestURI, "%23") {
				t.Error("ref not escaped")
			}
			var input map[string]any
			_ = json.NewDecoder(r.Body).Decode(&input)
			if input["force"] != false || input["sha"] != commit {
				t.Error("unsafe ref update")
			}
			updates.Add(1)
			writeJSON(t, w, GitRef{Ref: "refs/heads/feature#one", Object: GitObject{SHA: commit}})
		case "/repos/owner/repo/git/trees":
			var input struct {
				BaseTree string           `json:"base_tree"`
				Tree     []map[string]any `json:"tree"`
			}
			_ = json.NewDecoder(r.Body).Decode(&input)
			if input.BaseTree != tree || len(input.Tree) != 1 || input.Tree[0]["sha"] != nil {
				t.Error("delete not explicit null")
			}
			writeJSON(t, w, GitTree{SHA: tree})
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	g := mustGitHub(t, GitHubConfig{BaseURL: srv.URL, Owner: "owner", Repo: "repo", Token: "token", HTTPClient: srv.Client()})
	got, err := g.GetBlob(context.Background(), sha)
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("blob: %v", err)
	}
	if _, err := g.GetTree(context.Background(), tree, true); !errors.Is(err, ErrTruncatedTree) {
		t.Fatal("truncated tree reported complete")
	}
	if _, err := g.UpdateRef(context.Background(), "heads/feature#one", commit); err != nil || updates.Load() != 1 {
		t.Fatalf("update: %v", err)
	}
	if _, err := g.CreateTree(context.Background(), tree, []GitTreeEntry{{Path: "old.go", Type: "blob", Mode: "100644", Delete: true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.CreateTree(context.Background(), tree, []GitTreeEntry{{Path: "../escape", Type: "blob", Mode: "100644", SHA: sha}}); err == nil {
		t.Fatal("unsafe tree path accepted")
	}
	if _, err := g.UpdateRef(context.Background(), "heads/../main", commit); err == nil {
		t.Fatal("unsafe ref accepted")
	}
}

func chatFixture(content string, toolCalls []any, finish string) map[string]any {
	message := map[string]any{"role": "assistant", "content": content}
	if toolCalls != nil {
		message["tool_calls"] = toolCalls
	}
	return map[string]any{"id": "completion", "model": "test", "choices": []any{map[string]any{"message": message, "finish_reason": finish}}, "usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}}
}
func TestModelToolValidationAndUsage(t *testing.T) {
	params := json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"path":{"type":"string"}},"required":["path"]}`)
	var reply any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer model-secret" {
			t.Error("wrong model request")
		}
		writeJSON(t, w, reply)
	}))
	defer srv.Close()
	m := mustModel(t, ModelConfig{BaseURL: srv.URL + "/v1", APIKey: "model-secret", Model: "test", HTTPClient: srv.Client()})
	req := ChatRequest{Messages: []ChatMessage{{Role: "user", Content: "read"}}, Tools: []ToolDefinition{{Name: "read_file", Parameters: params}}}
	for _, tc := range []struct {
		name, tool, args, finish string
		wantErr                  bool
	}{
		{"valid", "read_file", `{"path":"README.md"}`, "tool_calls", false},
		{"unknown_tool", "shell", `{"path":"README.md"}`, "tool_calls", true},
		{"invalid_type", "read_file", `{"path":17}`, "tool_calls", true},
		{"extra_field", "read_file", `{"path":"x","command":"rm"}`, "tool_calls", true},
		{"duplicate_key", "read_file", `{"path":"a","path":"b"}`, "tool_calls", true},
		{"truncated", "read_file", `{"path":"a"}`, "length", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reply = chatFixture("", []any{map[string]any{"id": "call1", "type": "function", "function": map[string]string{"name": tc.tool, "arguments": tc.args}}}, tc.finish)
			got, err := m.Complete(context.Background(), req)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v", err)
			}
			if !tc.wantErr && (got.Usage.TotalTokens != 15 || len(got.Message.ToolCalls) != 1) {
				t.Fatalf("result %#v", got)
			}
		})
	}
	withoutUsage := chatFixture("done", nil, "stop")
	delete(withoutUsage, "usage")
	reply = withoutUsage
	if _, err := m.Complete(context.Background(), ChatRequest{Messages: []ChatMessage{{Role: "user", Content: "x"}}}); err == nil {
		t.Fatal("missing usage silently treated as free")
	}
}

func TestModelAnswerCitationGate(t *testing.T) {
	var citation = "evidence1"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input map[string]any
		_ = json.NewDecoder(r.Body).Decode(&input)
		if input["response_format"] == nil {
			t.Error("schema format not requested")
		}
		answer, _ := json.Marshal(Answer{Disposition: "answer", Text: "Use the documented login command.", CitationIDs: []string{citation}, MissingInfo: []string{}})
		writeJSON(t, w, chatFixture(string(answer), nil, "stop"))
	}))
	defer srv.Close()
	m := mustModel(t, ModelConfig{BaseURL: srv.URL, Model: "test", HTTPClient: srv.Client()})
	input := AnswerInput{Question: "login", Evidence: []Evidence{{ID: "evidence1", Text: "supported instructions"}}}
	if _, _, err := m.Answer(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	citation = "not-retrieved"
	if _, _, err := m.Answer(context.Background(), input); !errors.Is(err, ErrInvalidModelOutput) {
		t.Fatal("fabricated citation accepted")
	}
	if _, err := parseSchema([]byte(`{"type":"object","$ref":"file:///secret"}`)); err == nil {
		t.Fatal("unsupported schema silently accepted")
	}
}

func TestModelMultimodalMessagesAndAnswerImages(t *testing.T) {
	var requests []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input map[string]any
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Fatal(err)
		}
		requests = append(requests, input)
		answer, _ := json.Marshal(Answer{Disposition: "answer", Text: "Use the screenshot and documented login command.", CitationIDs: []string{"evidence1"}, MissingInfo: []string{}})
		writeJSON(t, w, chatFixture(string(answer), nil, "stop"))
	}))
	defer srv.Close()
	m := mustModel(t, ModelConfig{BaseURL: srv.URL, Model: "test", HTTPClient: srv.Client()})
	input := AnswerInput{Question: "login", Evidence: []Evidence{{ID: "evidence1", Text: "supported instructions"}}, Images: []InputImage{{ID: "img-1", Name: "screen.png", MediaType: "image/png", Data: []byte("png-bytes")}}}
	if _, _, err := m.Answer(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 {
		t.Fatal(len(requests))
	}
	messages, ok := requests[0]["messages"].([]any)
	if !ok || len(messages) != 2 {
		t.Fatalf("messages %#v", requests[0]["messages"])
	}
	user, ok := messages[1].(map[string]any)
	if !ok {
		t.Fatalf("user %#v", messages[1])
	}
	parts, ok := user["content"].([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("content %#v", user["content"])
	}
	text, _ := parts[0].(map[string]any)
	image, _ := parts[1].(map[string]any)
	if text["type"] != "text" || !strings.Contains(text["text"].(string), "supported instructions") {
		t.Fatalf("text part %#v", text)
	}
	imageURL, ok := image["image_url"].(map[string]any)
	url, ok2 := imageURL["url"].(string)
	if !ok || !ok2 || image["type"] != "image_url" || !strings.HasPrefix(url, "data:image/png;base64,") {
		t.Fatalf("image part %#v", image)
	}
	if _, _, err := m.Answer(context.Background(), AnswerInput{Question: "login", Evidence: []Evidence{{ID: "evidence1", Text: "supported instructions"}}, Images: []InputImage{{ID: "img-1", MediaType: "image/png", Data: []byte("a")}, {ID: "img-1", MediaType: "image/png", Data: []byte("b")}}}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("duplicate image id accepted: %v", err)
	}
}

func TestHTTPBoundsRedirectsAndUnknownWrite(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch r.URL.Path {
		case "/oversize":
			_, _ = io.WriteString(w, strings.Repeat("x", 200))
		case "/redirect":
			http.Redirect(w, r, "/secret", http.StatusTemporaryRedirect)
		case "/error":
			w.WriteHeader(503)
			_, _ = io.WriteString(w, "app_secret=do-not-leak")
		default:
			t.Error("redirect was followed")
		}
	}))
	defer srv.Close()
	c, err := newClient("test", srv.URL, srv.Client(), time.Second, 100, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/oversize", "/redirect", "/error"} {
		_, _, err := c.request(context.Background(), http.MethodPost, path, nil, nil, map[string]string{"x": "y"})
		var apiErr *APIError
		if !errors.As(err, &apiErr) || strings.Contains(err.Error(), "do-not-leak") {
			t.Fatalf("unsafe/missing error: %v", err)
		}
		if path != "/redirect" && !apiErr.OutcomeUnknown {
			t.Fatal("ambiguous write marked failed")
		}
	}
	if calls.Load() != 3 {
		t.Fatal("request retried or redirected")
	}
	if _, err := newClient("test", "https://secret@example.com", nil, 0, 0, false); err == nil {
		t.Fatal("credential URL accepted")
	}
	if _, err := newClient("test", "http://remote.example", nil, 0, 0, false); err == nil {
		t.Fatal("unencrypted remote accepted")
	}
}

func TestVaultAndRedaction(t *testing.T) {
	v, err := NewVault(bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	one, err := v.Seal([]byte("secret"), []byte("connector:1:key:2"))
	if err != nil {
		t.Fatal(err)
	}
	two, _ := v.Seal([]byte("secret"), []byte("connector:1:key:2"))
	if one == two {
		t.Fatal("nonce reused")
	}
	plain, err := v.Open(one, []byte("connector:1:key:2"))
	if err != nil || string(plain) != "secret" {
		t.Fatal("roundtrip failed")
	}
	if _, err := v.Open(one, []byte("connector:other:key:2")); err == nil {
		t.Fatal("credential could be swapped")
	}
	redacted := Redact("Bearer abc.def access_token=raw-token https://example.com/?token=link-secret\n-----BEGIN PRIVATE KEY-----\nprivate\n-----END PRIVATE KEY----- explicit", "explicit")
	for _, secret := range []string{"abc.def", "raw-token", "link-secret", "private", "explicit"} {
		if strings.Contains(redacted, secret) {
			t.Fatalf("secret remained: %s", secret)
		}
	}
}

type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

func TestModelTransportDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{syscall.ECONNREFUSED, "连接被拒绝"},
		{&net.DNSError{Err: "no such host", Name: "secret.example"}, "域名解析失败"},
		{context.DeadlineExceeded, "请求超时"},
	} {
		model := mustModel(t, ModelConfig{BaseURL: "https://model.example/v1", Model: "test", HTTPClient: doerFunc(func(*http.Request) (*http.Response, error) { return nil, tc.err })})
		_, _, err := model.Classify(context.Background(), "test")
		if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "secret.example") {
			t.Fatalf("unexpected error for %v: %v", tc.err, err)
		}
	}
}

func TestHTTPTimeoutMarksUnknownWithoutRetry(t *testing.T) {
	var calls atomic.Int32
	doer := doerFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	c, err := newClient("test", "https://example.invalid", doer, 15*time.Millisecond, 100, false)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, _, err = c.request(context.Background(), http.MethodPost, "/send", nil, nil, nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || !apiErr.OutcomeUnknown || calls.Load() != 1 || time.Since(start) > time.Second {
		t.Fatalf("timeout was not bounded: %v", err)
	}
}

func TestLarkInvalidTokenClearsCacheWithoutReplaying(t *testing.T) {
	var tokenCalls, businessCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "tenant_access_token") {
			n := tokenCalls.Add(1)
			writeJSON(t, w, map[string]any{"code": 0, "tenant_access_token": "token-" + strconv.Itoa(int(n)), "expire": 7200})
			return
		}
		n := businessCalls.Add(1)
		if n == 1 {
			writeJSON(t, w, map[string]any{"code": 99991663, "msg": "expired"})
			return
		}
		if r.Header.Get("Authorization") != "Bearer token-2" {
			t.Error("stale token reused")
		}
		writeJSON(t, w, map[string]any{"code": 0, "data": map[string]any{"items": []any{}, "has_more": false}})
	}))
	defer srv.Close()
	l := mustLark(t, LarkConfig{BaseURL: srv.URL, AppID: "app", AppSecret: "secret", HTTPClient: srv.Client()})
	if _, err := l.History(context.Background(), HistoryRequest{ContainerID: "chat"}); err == nil || businessCalls.Load() != 1 {
		t.Fatal("invalid token was swallowed or automatically retried")
	}
	if _, err := l.History(context.Background(), HistoryRequest{ContainerID: "chat"}); err != nil || tokenCalls.Load() != 2 {
		t.Fatalf("refresh: %v", err)
	}
}

func TestGitHubApprovedPublishRequests(t *testing.T) {
	base := strings.Repeat("a", 40)
	tree := strings.Repeat("b", 40)
	commit := strings.Repeat("c", 40)
	blob := blobSHA([]byte("contents"))
	marker, _ := OperationMarker("operation-1")
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch r.URL.Path {
		case "/repos/owner/repo/git/blobs":
			var in map[string]string
			_ = json.NewDecoder(r.Body).Decode(&in)
			if in["encoding"] != "base64" || in["content"] != base64.StdEncoding.EncodeToString([]byte("contents")) {
				t.Error("blob body mismatch")
			}
			writeJSON(t, w, GitObject{SHA: blob})
		case "/repos/owner/repo/git/commits":
			var in struct {
				Tree    string   `json:"tree"`
				Parents []string `json:"parents"`
				Message string   `json:"message"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			if in.Tree != tree || len(in.Parents) != 1 || in.Parents[0] != base || in.Message != "fix auth" {
				t.Error("commit body mismatch")
			}
			writeJSON(t, w, GitCommit{SHA: commit, Tree: GitObject{SHA: tree}, Parents: []GitObject{{SHA: base}}})
		case "/repos/owner/repo/git/refs":
			var in map[string]string
			_ = json.NewDecoder(r.Body).Decode(&in)
			if in["ref"] != "refs/heads/agentowl/task" || in["sha"] != commit {
				t.Error("ref body mismatch")
			}
			writeJSON(t, w, GitRef{Ref: in["ref"], Object: GitObject{SHA: commit}})
		case "/repos/owner/repo/pulls":
			pr := GitHubPullRequest{ID: 1, Number: 3, Body: marker, Head: GitHubBranch{Ref: "agentowl/task", SHA: commit}, Base: GitHubBranch{Ref: "main"}}
			if r.Method == http.MethodGet {
				if r.URL.Query().Get("head") != "owner:agentowl/task" || r.URL.Query().Get("state") != "all" {
					t.Error("reconcile filters missing")
				}
				writeJSON(t, w, []GitHubPullRequest{pr})
				return
			}
			var in CreatePullRequestRequest
			_ = json.NewDecoder(r.Body).Decode(&in)
			if in.Head != "agentowl/task" || in.Base != "main" || !in.Draft || !strings.Contains(in.Body, marker) {
				t.Error("PR body mismatch")
			}
			writeJSON(t, w, pr)
		default:
			t.Errorf("unexpected publish endpoint %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	g := mustGitHub(t, GitHubConfig{BaseURL: srv.URL, Owner: "owner", Repo: "repo", HTTPClient: srv.Client()})
	if _, err := g.CreateBlob(context.Background(), []byte("contents")); err != nil {
		t.Fatal(err)
	}
	if _, err := g.CreateCommit(context.Background(), "fix auth", tree, base); err != nil {
		t.Fatal(err)
	}
	if _, err := g.CreateRef(context.Background(), "heads/agentowl/task", commit); err != nil {
		t.Fatal(err)
	}
	if _, err := g.CreatePullRequest(context.Background(), CreatePullRequestRequest{Title: "fix", Body: marker, Head: "agentowl/task", Base: "main", Draft: true}); err != nil {
		t.Fatal(err)
	}
	if pr, err := g.LookupPullRequest(context.Background(), "owner:agentowl/task", "main", "operation-1", 1); err != nil || pr == nil || pr.Number != 3 {
		t.Fatalf("lookup: %v", err)
	}
	if calls.Load() != 5 {
		t.Fatalf("unexpected retries: %d", calls.Load())
	}
}

func TestModelClassificationRejectsMalformedOutput(t *testing.T) {
	var content = `{"support_relevance":"in_scope","type":"bug","module":"token","reason":"refresh failed","missing_info":[],"needs_human":true}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writeJSON(t, w, chatFixture(content, nil, "stop")) }))
	defer srv.Close()
	m := mustModel(t, ModelConfig{BaseURL: srv.URL, Model: "test", JSONMode: "json_object", HTTPClient: srv.Client()})
	result, usage, err := m.Classify(context.Background(), "refresh error")
	if err != nil || result.SupportRelevance != "in_scope" || usage.TotalTokens != 15 {
		t.Fatalf("classify: %v", err)
	}
	content = `{"support_relevance":"in_scope","type":"bug","module":"token","reason":"r","missing_info":[],"needs_human":false,"command":"execute"}`
	if _, _, err := m.Classify(context.Background(), "x"); !errors.Is(err, ErrInvalidModelOutput) {
		t.Fatal("extra instruction field accepted")
	}
}

func TestJSONObjectCombinesSchemaWithSystemMessage(t *testing.T) {
	for _, withSystem := range []bool{true, false} {
		t.Run(strconv.FormatBool(withSystem), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Messages       []wireMessage `json:"messages"`
					ResponseFormat struct {
						Type string `json:"type"`
					} `json:"response_format"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if body.ResponseFormat.Type != "json_object" || len(body.Messages) != 2 || body.Messages[0].Role != "system" || body.Messages[1].Role != "user" {
					t.Fatalf("unexpected request: %+v", body)
				}
				if body.Messages[1].Content != "user input" {
					t.Fatalf("user content changed: %#v", body.Messages[1].Content)
				}
				if current, ok := body.Messages[0].Content.(string); !ok || !strings.Contains(current, `"required":["answer"]`) || (withSystem && !strings.Contains(current, "original system rule")) {
					t.Fatalf("system instructions missing: %#v", body.Messages[0].Content)
				}
				writeJSON(t, w, chatFixture(`{"answer":"ok"}`, nil, "stop"))
			}))
			defer server.Close()
			m := mustModel(t, ModelConfig{BaseURL: server.URL, Model: "test", JSONMode: "json_object", HTTPClient: server.Client()})
			messages := []ChatMessage{{Role: "user", Content: "user input"}}
			if withSystem {
				messages = append([]ChatMessage{{Role: "system", Content: "original system rule"}}, messages...)
			}
			_, err := m.Complete(context.Background(), ChatRequest{Messages: messages, ResponseSchema: &JSONSchema{Name: "test", Schema: json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"],"additionalProperties":false}`)}})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUpstreamDiagnosticKeepsUsefulFieldsAndRedacts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			writeJSON(t, w, map[string]any{"code": 0, "tenant_access_token": "sensitive-bearer-value", "expire": 7200})
		case "/open-apis/im/v1/messages":
			w.WriteHeader(400)
			_, _ = io.WriteString(w, `{"code":230002,"msg":"Bot/User can NOT be out of the chat.","error":{"log_id":"20261029195111ABC"}}`)
		case "/model":
			w.Header().Set("X-Request-Id", "request-123")
			w.WriteHeader(401)
			_, _ = io.WriteString(w, `{"error":{"code":"invalid_api_key","message":"sensitive prompt and secret"}}`)
		case "/redact":
			w.WriteHeader(403)
			_, _ = io.WriteString(w, `{"message":"credential sensitive-bearer-value access_token=another-secret https://private.example/token"}`)
		}
	}))
	defer srv.Close()
	l, err := NewLarkClient(LarkConfig{BaseURL: srv.URL, AppID: "test", AppSecret: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = l.History(context.Background(), HistoryRequest{ContainerType: "chat", ContainerID: "oc_test", PageSize: 1})
	var api *APIError
	if !errors.As(err, &api) || api.StatusCode != 400 || api.Code != "230002" || api.RequestID != "20261029195111ABC" || !strings.Contains(api.Diagnostic(), "Bot/User can NOT") {
		t.Fatalf("missing Feishu diagnostics: %+v", api)
	}
	c, err := newClient("model", srv.URL, srv.Client(), time.Second, 1024, false)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = c.request(context.Background(), http.MethodPost, "/model", nil, http.Header{"Authorization": {"Bearer sensitive-bearer-value"}}, map[string]string{"prompt": "sensitive prompt"})
	if !errors.As(err, &api) || api.Code != "invalid_api_key" || api.RequestID != "request-123" || strings.Contains(api.Diagnostic(), "sensitive") {
		t.Fatalf("unsafe model error: %+v", api)
	}
	c, err = newClient("github", srv.URL, srv.Client(), time.Second, 1024, false)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = c.request(context.Background(), http.MethodGet, "/redact", nil, http.Header{"Authorization": {"Bearer sensitive-bearer-value"}}, nil)
	if !errors.As(err, &api) {
		t.Fatal(err)
	}
	for _, secret := range []string{"sensitive-bearer-value", "another-secret", "private.example"} {
		if strings.Contains(api.Diagnostic(), secret) {
			t.Fatalf("leaked %s: %s", secret, api.Diagnostic())
		}
	}
}

func TestGitHubWritePermissionDiagnostic(t *testing.T) {
	headers := http.Header{"X-Accepted-Github-Permissions": {"contents=write"}}
	e := apiResponseError("github", http.MethodPost, http.StatusForbidden, []byte(`{"message":"Resource not accessible by personal access token"}`), headers, "Bearer private-token")
	if !strings.Contains(e.Diagnostic(), "personal access token") || !strings.Contains(e.Diagnostic(), "Contents") || strings.Contains(e.Diagnostic(), "private-token") {
		t.Fatalf("unexpected diagnostic: %s", e.Diagnostic())
	}
}

func TestUpstreamDiagnosticDoesNotExposeCredentialsInIDs(t *testing.T) {
	const token = "sensitive-token-123"
	e := apiResponseError("lark", http.MethodGet, 400, []byte(`{"code":"sensitive-token-123","error":{"log_id":"sensitive-token-123"}}`), http.Header{"X-Request-Id": []string{token}}, "Bearer "+token)
	if strings.Contains(e.Diagnostic(), token) {
		t.Fatal("credential leaked in diagnostic")
	}
}

func TestLarkBusinessErrorDiagnostics(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "tenant_access_token") {
			writeJSON(t, w, map[string]any{"code": 0, "tenant_access_token": "token", "expire": 7200})
			return
		}
		writeJSON(t, w, map[string]any{"code": 230002, "msg": "Bot/User can NOT be out of the chat.", "error": map[string]any{"log_id": "business-log"}})
	}))
	defer server.Close()
	l, err := NewLarkClient(LarkConfig{BaseURL: server.URL, AppID: "test", AppSecret: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = l.History(context.Background(), HistoryRequest{ContainerID: "oc_test"})
	var api *APIError
	if !errors.As(err, &api) || api.Code != "230002" || api.StatusCode != 200 || api.RequestID != "business-log" || !strings.Contains(api.Diagnostic(), "Bot/User") {
		t.Fatalf("unexpected diagnostics: %v", err)
	}
}

func TestLongConnectionAuthenticatesEventContext(t *testing.T) {
	l := mustLark(t, LarkConfig{AppID: "app", TenantKey: "tenant"})
	raw := larkFixture("im.message.receive_v1", map[string]any{"sender": map[string]any{"sender_id": map[string]string{"open_id": "ou_user"}, "sender_type": "user", "tenant_key": "tenant"}, "message": map[string]any{"message_id": "om_1", "chat_id": "oc_1", "create_time": strconv.FormatInt(time.Now().UnixMilli(), 10), "message_type": "text", "content": `{"text":"hello"}`}})
	// The same unsigned payload must never pass the HTTP webhook path.
	if _, err := l.ParseWebhook(nil, raw); err == nil {
		t.Fatal("unsigned webhook accepted")
	}
	result, err := l.ParseLongConnectionEvent(raw)
	if err != nil || result.Event == nil {
		t.Fatalf("trusted socket event: %v", err)
	}
	var payload map[string]any
	if err = json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	header := payload["header"].(map[string]any)
	header["app_id"] = "different-app"
	bad, _ := json.Marshal(payload)
	if _, err = l.ParseLongConnectionEvent(bad); err == nil {
		t.Fatal("wrong app accepted")
	}
}

func TestLongConnectionCardAuthenticatesEventContext(t *testing.T) {
	l := mustLark(t, LarkConfig{AppID: "app", TenantKey: "tenant"})
	raw := larkFixture("card.action.trigger", map[string]any{"operator": map[string]string{"open_id": "ou_user", "tenant_key": "external-tenant"}, "context": map[string]string{"open_message_id": "om_card", "open_chat_id": "oc_test"}, "action": map[string]any{"value": map[string]any{"operation_id": "operation", "nonce": "nonce", "action": "feedback_resolved"}}})
	if _, err := l.ParseWebhook(nil, raw); err == nil {
		t.Fatal("unsigned HTTP callback accepted")
	}
	result, err := l.ParseLongConnectionEvent(raw)
	if err != nil || result.CardAction == nil || result.CardAction.ActorTenantID != "external-tenant" {
		t.Fatalf("trusted socket callback rejected: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	header := payload["header"].(map[string]any)
	for _, field := range []string{"app_id", "tenant_key", "event_type"} {
		old := header[field]
		header[field] = "invalid"
		bad, _ := json.Marshal(payload)
		if _, err := l.ParseLongConnectionEvent(bad); err == nil {
			t.Fatalf("callback with invalid %s accepted", field)
		}
		header[field] = old
	}
}
