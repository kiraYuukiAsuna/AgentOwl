package service

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func identityFixture(t *testing.T, s *Service, onIdentity ...func()) (string, *atomic.Int64) {
	t.Helper()
	calls := &atomic.Int64{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch r.URL.Path {
		case "/open-apis/authen/v2/oauth/token":
			var b Data
			if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			if Str(b, "client_id") != "cli_login" || Str(b, "client_secret") != "test-only-secret" || Str(b, "redirect_uri") != s.Config.PublicURL+"/auth/lark/callback" || len(Str(b, "code_verifier")) < 43 {
				t.Error("invalid code exchange")
				w.WriteHeader(400)
				return
			}
			_ = json.NewEncoder(w).Encode(Data{"code": 0, "access_token": "test-only-user-token", "refresh_token": "must-not-persist"})
		case "/open-apis/authen/v1/user_info":
			for _, hook := range onIdentity {
				hook()
			}
			if r.Header.Get("Authorization") != "Bearer test-only-user-token" {
				t.Error("wrong user token")
			}
			_ = json.NewEncoder(w).Encode(Data{"code": 0, "data": Data{"open_id": "ou_person", "name": "飞书成员", "tenant_key": "tenant_other"}})
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(server.Close)
	id := supportConfig(t, s, "connectors", Data{"provider": "feishu", "app_id": "cli_login", "name": "团队飞书", "user_login_enabled": true, "base_url": server.URL}, Data{"app_secret": "test-only-secret"})
	return id, calls
}

func TestLarkBindLoginAndUnbind(t *testing.T) {
	s, a := testService(t)
	ctx := context.Background()
	app, _ := identityFixture(t, s)
	_, session, err := s.LocalLogin(ctx, s.Config.BootstrapEmail, s.Config.BootstrapPassword)
	if err != nil {
		t.Fatal(err)
	}
	location, state, err := s.LarkLoginStart(ctx, app, a, session)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(location)
	if err != nil {
		t.Fatal(err)
	}
	var verifier string
	if err = s.DB.QueryRow(ctx, "SELECT verifier FROM lark_login_transactions WHERE state_hash=$1", Hash([]byte(state))).Scan(&verifier); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(verifier))
	if u.Host != "accounts.feishu.cn" || u.Query().Get("state") != state || u.Query().Get("code_challenge_method") != "S256" || u.Query().Get("code_challenge") != base64.RawURLEncoding.EncodeToString(hash[:]) || u.Query().Get("scope") != "" {
		t.Fatal("unexpected authorization parameters")
	}
	token, bound, err := s.LarkLoginFinish(ctx, state, "code", session)
	if err != nil || !bound || token != "" {
		t.Fatal(bound, err)
	}
	who, bindingID, err := resolvePlatformIdentity(ctx, s.DB, "feishu", "cli_login", "ou_person")
	if err != nil || who.ID != a.ID || who.Role != a.Role {
		t.Fatal("binding changed principal", err)
	}
	if _, _, err = resolvePlatformIdentity(ctx, s.DB, "feishu", "other_app", "ou_person"); !errors.Is(err, NotFound) {
		t.Fatal("cross-app identity", err)
	}
	if _, _, err = s.LarkLoginFinish(ctx, state, "code", session); err == nil {
		t.Fatal("replayed binding")
	}
	identities, err := s.MyIdentities(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	encoded := string(JSON(identities))
	if !strings.Contains(encoded, "飞书成员") || strings.Contains(encoded, "token") || strings.Contains(encoded, "secret") {
		t.Fatal("unsafe identity view", encoded)
	}
	_, state, err = s.LarkLoginStart(ctx, app, Actor{}, "")
	if err != nil {
		t.Fatal(err)
	}
	token, bound, err = s.LarkLoginFinish(ctx, state, "code", "")
	if err != nil || bound || token == "" {
		t.Fatal(bound, err)
	}
	if who, err = s.Authenticate(ctx, token); err != nil || who.ID != a.ID {
		t.Fatal(err)
	}
	current, err := one(ctx, s.DB, "SELECT to_jsonb(c) FROM configs c WHERE id=$1", bindingID)
	if err != nil {
		t.Fatal(err)
	}
	mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.UnbindIdentity(ctx, tx, a, bindingID, Data{"expected_version": Num(current, "version")})
	})
	if _, err = s.Authenticate(ctx, token); err == nil {
		t.Fatal("unbound session survived")
	}
	if _, err = s.Authenticate(ctx, session); err != nil {
		t.Fatal("local login changed", err)
	}
	if _, _, err = resolvePlatformIdentity(ctx, s.DB, "feishu", "cli_login", "ou_person"); !errors.Is(err, NotFound) {
		t.Fatal("unbound card identity survived", err)
	}
	_, state, err = s.LarkLoginStart(ctx, app, Actor{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.LarkLoginFinish(ctx, state, "code", ""); err == nil {
		t.Fatal("unregistered user was provisioned")
	}
	if supportCount(t, s, "SELECT count(*) FROM principals") != 1 {
		t.Fatal("login created a principal")
	}
	if supportCount(t, s, "SELECT count(*) FROM configs WHERE data::text LIKE '%test-only-user-token%' OR data::text LIKE '%must-not-persist%'") != 0 {
		t.Fatal("user token persisted")
	}
}

func TestLarkBindingRequiresSameLiveSessionAndUnchangedApp(t *testing.T) {
	for _, mode := range []string{"different_session", "expired", "logout", "app_changed", "disabled"} {
		t.Run(mode, func(t *testing.T) {
			s, a := testService(t)
			ctx := context.Background()
			app, calls := identityFixture(t, s)
			_, session, err := s.newSession(ctx, a.ID)
			if err != nil {
				t.Fatal(err)
			}
			_, state, err := s.LarkLoginStart(ctx, app, a, session)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "different_session":
				_, session, err = s.newSession(ctx, a.ID)
			case "expired":
				_, err = s.DB.Exec(ctx, "UPDATE lark_login_transactions SET expires_at=now()-interval '1 second'")
			case "logout":
				err = s.Logout(ctx, session)
			case "app_changed":
				_, err = s.DB.Exec(ctx, "UPDATE configs SET version=version+1 WHERE id=$1", app)
			case "disabled":
				_, err = s.DB.Exec(ctx, `UPDATE configs SET data=data||'{"user_login_enabled":false}'::jsonb WHERE id=$1`, app)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err = s.LarkLoginFinish(ctx, state, "code", session); err == nil {
				t.Fatal("invalid binding accepted")
			}
			if calls.Load() != 0 {
				t.Fatal("invalid transaction reached identity provider")
			}
		})
	}
}

func TestConcurrentLarkBindingsCannotTakeOverIdentity(t *testing.T) {
	s, a := testService(t)
	ctx := context.Background()
	app, _ := identityFixture(t, s)
	b := Actor{ID: ID(), Name: "Other", Role: "viewer"}
	if _, err := s.DB.Exec(ctx, "INSERT INTO principals(id,name,email,role) VALUES($1,'Other','other@example.test','viewer')", b.ID); err != nil {
		t.Fatal(err)
	}
	type attempt struct{ state, session string }
	attempts := []attempt{}
	for _, actor := range []Actor{a, b} {
		_, session, err := s.newSession(ctx, actor.ID)
		if err != nil {
			t.Fatal(err)
		}
		_, state, err := s.LarkLoginStart(ctx, app, actor, session)
		if err != nil {
			t.Fatal(err)
		}
		attempts = append(attempts, attempt{state, session})
	}
	var wg sync.WaitGroup
	var successes atomic.Int64
	for _, attempt := range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := s.LarkLoginFinish(ctx, attempt.state, "code", attempt.session); err == nil {
				successes.Add(1)
			}
		}()
	}
	wg.Wait()
	if successes.Load() != 1 || supportCount(t, s, "SELECT count(*) FROM configs WHERE kind='identity-bindings'") != 1 {
		t.Fatal("identity takeover or duplicate")
	}
}

func TestLarkLoginSessionFollowsCurrentConfiguration(t *testing.T) {
	for _, change := range []string{"binding_disabled", "principal_disabled", "connector_disabled", "binding_reassigned"} {
		t.Run(change, func(t *testing.T) {
			s, a := testService(t)
			ctx := context.Background()
			app, _ := identityFixture(t, s)
			bindingID := supportConfig(t, s, "identity-bindings", Data{"provider": "feishu", "app_id": "cli_login", "subject_id": "ou_person", "principal_id": a.ID}, nil)
			_, state, err := s.LarkLoginStart(ctx, app, Actor{}, "")
			if err != nil {
				t.Fatal(err)
			}
			session, _, err := s.LarkLoginFinish(ctx, state, "code", "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.Authenticate(ctx, session); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "binding_disabled":
				_, err = s.DB.Exec(ctx, `UPDATE configs SET data=data||'{"enabled":false}'::jsonb WHERE id=$1`, bindingID)
			case "principal_disabled":
				_, err = s.DB.Exec(ctx, "UPDATE principals SET active=false WHERE id=$1", a.ID)
			case "connector_disabled":
				_, err = s.DB.Exec(ctx, `UPDATE configs SET data=data||'{"status":"disabled"}'::jsonb WHERE id=$1`, app)
			case "binding_reassigned":
				_, err = s.DB.Exec(ctx, "UPDATE configs SET data=data||jsonb_build_object('principal_id',$2::text) WHERE id=$1", bindingID, ID())
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.Authenticate(ctx, session); err == nil {
				t.Fatal("revoked login still works")
			}
		})
	}
}

func TestLarkBindRechecksSessionAfterProviderExchange(t *testing.T) {
	s, a := testService(t)
	ctx := context.Background()
	originalApp := supportConfig(t, s, "connectors", Data{"provider": "feishu", "app_id": "cli_origin", "user_login_enabled": true}, nil)
	originalBinding := supportConfig(t, s, "identity-bindings", Data{"provider": "feishu", "app_id": "cli_origin", "subject_id": "ou_origin", "principal_id": a.ID}, nil)
	_, session, err := s.newSession(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(ctx, `UPDATE sessions SET identity_binding_id=$2 WHERE id_hash=$1`, Hash([]byte(session)), originalBinding); err != nil {
		t.Fatal(err)
	}
	app, _ := identityFixture(t, s, func() {
		if _, err := s.DB.Exec(ctx, `UPDATE configs SET data=data||'{"user_login_enabled":false}'::jsonb WHERE id=$1`, originalApp); err != nil {
			t.Error(err)
		}
	})
	_, state, err := s.LarkLoginStart(ctx, app, a, session)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.LarkLoginFinish(ctx, state, "code", session); err == nil {
		t.Fatal("revoked session completed binding")
	}
	if supportCount(t, s, `SELECT count(*) FROM configs WHERE kind='identity-bindings' AND data->>'app_id'='cli_login'`) != 0 {
		t.Fatal("new binding persisted after revocation")
	}
}
