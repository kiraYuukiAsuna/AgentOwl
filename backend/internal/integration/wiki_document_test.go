package integration

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResolveWikiDocument(t *testing.T) {
	var denied bool
	var objectType = "docx"
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "tenant_access_token") {
			fmt.Fprint(w, `{"code":0,"tenant_access_token":"test-token","expire":7200}`)
			return
		}
		calls++
		if r.URL.Path != "/open-apis/wiki/v2/spaces/get_node" || r.URL.Query().Get("token") != "wiki_test" || r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("unexpected Wiki request")
		}
		if denied {
			fmt.Fprint(w, `{"code":131006,"msg":"permission denied"}`)
			return
		}
		fmt.Fprintf(w, `{"code":0,"data":{"node":{"node_token":"wiki_test","obj_token":"docx_test","obj_type":%q}}}`, objectType)
	}))
	defer server.Close()
	l, err := NewLarkClient(LarkConfig{BaseURL: server.URL, AppID: "app", AppSecret: "test-secret"})
	if err != nil {
		t.Fatal(err)
	}
	id, err := l.ResolveDocumentID(context.Background(), "", "https://example.larkoffice.com/wiki/wiki_test?from=share")
	if err != nil || id != "docx_test" {
		t.Fatal(id, err)
	}
	before := calls
	id, err = l.ResolveDocumentID(context.Background(), "", "https://example.larkoffice.com/docx/docx_test")
	if err != nil || id != "docx_test" || calls != before {
		t.Fatal("docx should not call wiki", err)
	}
	denied = true
	if _, err = l.ResolveDocumentID(context.Background(), "", "https://example.larkoffice.com/wiki/wiki_test"); err == nil {
		t.Fatal("permission error ignored")
	}
	denied = false
	objectType = "sheet"
	if _, err = l.ResolveDocumentID(context.Background(), "", "https://example.larkoffice.com/wiki/wiki_test"); err == nil {
		t.Fatal("non-docx accepted")
	}
}
