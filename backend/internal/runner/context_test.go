package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestContextPagesPreserveWholeUnicodeAndBindCursor(t *testing.T) {
	body := strings.Repeat("用户🙂日志\n", 1000)
	cursor, whole := "", ""
	for {
		page, err := PageContext("log", body, cursor)
		if err != nil {
			t.Fatal(err)
		}
		if len([]rune(page.Text)) > 1536 || page.TotalCharacters != len([]rune(body)) {
			t.Fatal("invalid page size")
		}
		whole += page.Text
		if page.NextCursor == "" {
			break
		}
		if _, err = PageContext("log", body+"changed", page.NextCursor); err == nil {
			t.Fatal("cursor accepted changed source")
		}
		cursor = page.NextCursor
	}
	if whole != body {
		t.Fatal("context lost characters")
	}
	for _, cursor := range []string{"-1", "1536", "hash:0", "hash:999999999999999999999"} {
		if _, err := PageContext("log", body, cursor); err == nil {
			t.Fatal("invalid cursor accepted")
		}
	}
}

func TestPagedContextCallbackKeepsMachineScope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("machine_id") != "machine-one" || r.URL.Query().Get("protocol") != "3" || r.URL.Query().Get("cursor") != "hash:1536" || r.Header.Get("Authorization") != "Bearer callback-secret" {
			t.Error("lost callback parameters")
		}
		_ = json.NewEncoder(w).Encode(ContextPage{ID: "case", Text: "next"})
	}))
	defer server.Close()
	provider := NewHTTPAuthorizer(server.URL, "callback-secret", "machine-one", server.Client())
	page, err := provider.ContextRefPage(context.Background(), "run", "case", "hash:1536")
	if err != nil || page.Text != "next" {
		t.Fatal(page, err)
	}
}

func TestContextHeaderContainsFactsAndPointersOnly(t *testing.T) {
	spec := RunSpec{ContextManifest: ContextManifest{Version: 2, Facts: json.RawMessage(`{"requester":{"name":"Alice","binding_status":"bound"}}`), Refs: []ContextRef{{ID: "case", Kind: "snapshot", Title: "问题详情"}}}, TaskInputs: []TaskInput{{ID: "input", Body: "private full source"}}}
	header := contextPrompt(spec)
	if strings.Contains(header, "private full source") || !strings.Contains(header, "Alice") || !strings.Contains(header, "case") {
		t.Fatal(header)
	}
}
