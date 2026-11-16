package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"agentowl/backend/internal/platform"
	"agentowl/backend/internal/service"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestLarkIdentityBrowserFlowAndCSRF(t *testing.T) {
	dsn := os.Getenv("AGENTOWL_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AGENTOWL_TEST_DATABASE_URL required")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := "http_identity_" + strings.ReplaceAll(service.ID(), "-", "")
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE") }()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = platform.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	s, err := service.New(db, platform.Config{AuthMode: "local", EncryptionKey: bytes.Repeat([]byte{1}, 32), DataDir: t.TempDir(), BootstrapName: "Admin", BootstrapEmail: "admin@example.test", BootstrapPassword: "test-only-password", SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Bootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/token") {
			_ = json.NewEncoder(w).Encode(service.Data{"access_token": "test-user-token"})
			return
		}
		_ = json.NewEncoder(w).Encode(service.Data{"code": 0, "data": service.Data{"open_id": "ou_http", "name": "Test Person"}})
	}))
	defer provider.Close()
	app := service.ID()
	secret, err := s.Vault.Seal(service.JSON(service.Data{"app_secret": "test-only-secret"}), []byte("connectors:"+app))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(ctx, "INSERT INTO configs(id,kind,name,data,secrets) VALUES($1,'connectors','Login App',$2,$3)", app, service.JSON(service.Data{"provider": "feishu", "app_id": "cli_http", "user_login_enabled": true, "base_url": provider.URL}), secret); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(New(s).Handler())
	defer server.Close()
	s.Config.PublicURL = server.URL
	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request := func(method, path, body, csrf string) (int, http.Header, service.Data) {
		t.Helper()
		req, e := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		if e != nil {
			t.Fatal(e)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Origin", server.URL)
		req.Header.Set("X-CSRF-Token", csrf)
		req.Header.Set("Idempotency-Key", service.ID())
		res, e := browser.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		defer res.Body.Close()
		raw, e := io.ReadAll(res.Body)
		if e != nil {
			t.Fatal(e)
		}
		result := service.Data{}
		_ = json.Unmarshal(raw, &result)
		return res.StatusCode, res.Header, result
	}
	status, _, me := request("POST", "/auth/login", `{"email":"admin@example.test","password":"test-only-password"}`, "")
	if status != 200 {
		t.Fatal("local login", status, me)
	}
	csrf := service.Str(me, "csrf_token")
	body := string(service.JSON(service.Data{"connector_id": app}))
	status, _, _ = request("POST", "/api/v1/me/identities/bind", body, "")
	if status != 403 {
		t.Fatal("binding missed CSRF", status)
	}
	status, _, result := request("POST", "/api/v1/me/identities/bind", body, csrf)
	if status != 200 {
		t.Fatal(status, result)
	}
	location, e := url.Parse(service.Str(result, "url"))
	if e != nil {
		t.Fatal(e)
	}
	state := location.Query().Get("state")
	u, _ := url.Parse(server.URL + "/auth/lark/callback")
	found := false
	for _, cookie := range jar.Cookies(u) {
		found = found || cookie.Name == "agentowl_lark_login" && cookie.Value == state
	}
	if !found {
		t.Fatal("browser did not receive state cookie")
	}
	status, headers, _ := request("GET", "/auth/lark/callback?state="+url.QueryEscape(state)+"&code=test-code", "", "")
	if status != 303 || headers.Get("Location") != "/profile?identity_bound=1" {
		t.Fatal(status, headers.Get("Location"))
	}
	status, _, identities := request("GET", "/api/v1/me/identities", "", "")
	if status != 200 || !strings.Contains(string(service.JSON(identities)), "ou_http") {
		t.Fatal(status, identities)
	}
	status, headers, _ = request("GET", "/auth/lark/login?connector_id="+app, "", "")
	if status != 302 {
		t.Fatal(status)
	}
	location, e = url.Parse(headers.Get("Location"))
	if e != nil {
		t.Fatal(e)
	}
	state = location.Query().Get("state")
	status, headers, _ = request("GET", "/auth/lark/callback?state="+url.QueryEscape(state)+"&code=test-code", "", "")
	if status != 303 || headers.Get("Location") != "/" {
		t.Fatal(status, headers.Get("Location"))
	}
	status, _, me = request("GET", "/api/v1/me", "", "")
	if status != 200 {
		t.Fatal("Feishu login failed", status)
	}
	csrf = service.Str(me, "csrf_token")
	var id string
	var version int
	if err = db.QueryRow(ctx, "SELECT id,version FROM configs WHERE kind='identity-bindings'").Scan(&id, &version); err != nil {
		t.Fatal(err)
	}
	status, _, result = request("DELETE", "/api/v1/me/identities/"+id, string(service.JSON(service.Data{"expected_version": version})), csrf)
	if status != 200 {
		t.Fatal(status, result)
	}
	status, _, _ = request("GET", "/api/v1/me", "", "")
	if status != 401 {
		t.Fatal("unbinding failed to revoke browser session", status)
	}
	status, headers, _ = request("GET", "/auth/lark/callback?state="+state+"&code=test-code", "", "")
	if status != 303 || !strings.HasPrefix(headers.Get("Location"), "/login?login_error=") {
		t.Fatal("replayed callback", status)
	}
}
