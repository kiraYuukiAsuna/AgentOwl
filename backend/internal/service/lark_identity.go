package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/oauth2"
)

func (s *Service) LarkLoginApps(ctx context.Context) ([]Data, error) {
	return many(ctx, s.DB, `SELECT jsonb_build_object('id',id,'name',name,'provider',data->>'provider') FROM configs
	 WHERE kind='connectors' AND data->>'provider' IN ('feishu','lark') AND data->>'user_login_enabled'='true'
	 AND COALESCE(data->>'enabled','true')<>'false' AND COALESCE(data->>'status','') NOT IN ('disabled','revoked') AND secrets<>'' ORDER BY name,id`)
}

func (s *Service) MyIdentities(ctx context.Context, a Actor) (Data, error) {
	bindings, err := many(ctx, s.DB, `SELECT data-'tenant_id'||jsonb_build_object('id',id,'version',version) FROM configs WHERE kind='identity-bindings' AND data->>'principal_id'=$1 ORDER BY name,id`, a.ID)
	if err != nil {
		return nil, err
	}
	apps, err := s.LarkLoginApps(ctx)
	return Data{"items": bindings, "apps": apps, "redirect_uri": s.Config.PublicURL + "/auth/lark/callback"}, err
}

// The same app-scoped mapping is used for login, message context and card actions.
func resolvePlatformIdentity(ctx context.Context, q DB, provider, appID, subject string) (Actor, string, error) {
	if appID == "" || subject == "" || !contains([]string{"feishu", "lark"}, provider) {
		return Actor{}, "", NotFound
	}
	rows, err := many(ctx, q, `SELECT to_jsonb(p)-'password_hash'||jsonb_build_object('binding_id',c.id) FROM configs c JOIN principals p ON p.id::text=c.data->>'principal_id'
	 WHERE c.kind='identity-bindings' AND c.data->>'provider'=$1 AND c.data->>'app_id'=$2 AND c.data->>'subject_id'=$3
	 AND COALESCE(c.data->>'enabled','true')<>'false' AND p.active`, provider, appID, subject)
	if err != nil {
		return Actor{}, "", err
	}
	if len(rows) != 1 {
		return Actor{}, "", NotFound
	}
	return actorFrom(rows[0]), Str(rows[0], "binding_id"), nil
}

func validateIdentityBinding(ctx context.Context, q DB, id string, b Data) error {
	var active, duplicate bool
	if err := q.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM principals WHERE id::text=$1 AND active)", Str(b, "principal_id")).Scan(&active); err != nil {
		return err
	}
	if !active {
		return Fail(422, "invalid_member", "请选择已启用的成员")
	}
	if err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM configs WHERE kind='identity-bindings' AND id::text<>$1
	 AND data->>'provider'=$2 AND data->>'app_id'=$3 AND (data->>'subject_id'=$4 OR data->>'principal_id'=$5))`, id, Str(b, "provider"), Str(b, "app_id"), Str(b, "subject_id"), Str(b, "principal_id")).Scan(&duplicate); err != nil {
		return err
	}
	if duplicate {
		return Fail(409, "identity_already_bound", "此飞书账号或团队成员已绑定，请先解除原绑定")
	}
	return nil
}

func (s *Service) LarkLoginStart(ctx context.Context, connectorID string, a Actor, session string) (string, string, error) {
	if !validID(connectorID) {
		return "", "", NotFound
	}
	_, c, err := s.supportLark(ctx, s.DB, connectorID)
	if err != nil {
		return "", "", err
	}
	if connectorDisabled(c) || !Bool(c, "user_login_enabled") {
		return "", "", Fail(409, "lark_login_disabled", "此应用尚未启用飞书登录")
	}
	if a.ID != "" {
		current, e := s.Authenticate(ctx, session)
		if e != nil || current.ID != a.ID {
			return "", "", Forbidden
		}
	}
	state, verifier := Token(), oauth2.GenerateVerifier()
	sessionHash := ""
	if a.ID != "" {
		sessionHash = Hash([]byte(session))
	}
	_, err = s.DB.Exec(ctx, `INSERT INTO lark_login_transactions(state_hash,verifier,connector_id,connector_version,principal_id,session_hash,expires_at) VALUES($1,$2,$3,$4,$5,$6,now()+interval '10 minutes')`, Hash([]byte(state)), verifier, connectorID, Num(c, "version"), a.ID, sessionHash)
	if err != nil {
		return "", "", err
	}
	authorize := "https://accounts.feishu.cn/open-apis/authen/v1/authorize"
	if Str(c, "provider") == "lark" {
		authorize = "https://accounts.larksuite.com/open-apis/authen/v1/authorize"
	}
	config := oauth2.Config{ClientID: Str(c, "app_id"), RedirectURL: s.Config.PublicURL + "/auth/lark/callback", Endpoint: oauth2.Endpoint{AuthURL: authorize}}
	return config.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier)), state, nil
}

func (s *Service) LarkLoginFinish(ctx context.Context, state, code, session string) (string, bool, error) {
	invalid := Fail(401, "invalid_login", "飞书授权已失效，请重新操作")
	if len(state) < 30 {
		return "", false, invalid
	}
	// Consume once even if exchanging the code or saving the binding fails.
	m, err := one(ctx, s.DB, `DELETE FROM lark_login_transactions WHERE state_hash=$1 AND expires_at>now() RETURNING to_jsonb(lark_login_transactions)`, Hash([]byte(state)))
	if err != nil {
		if errors.Is(err, NotFound) {
			err = invalid
		}
		return "", false, err
	}
	binding := Str(m, "principal_id") != ""
	if binding {
		a, e := s.Authenticate(ctx, session)
		if e != nil || a.ID != Str(m, "principal_id") || Hash([]byte(session)) != Str(m, "session_hash") {
			return "", true, invalid
		}
	}
	cli, c, err := s.supportLark(ctx, s.DB, Str(m, "connector_id"))
	if err != nil {
		return "", binding, err
	}
	if connectorDisabled(c) || !Bool(c, "user_login_enabled") || Num(c, "version") != Num(m, "connector_version") {
		return "", binding, invalid
	}
	identity, err := cli.LoginIdentity(ctx, code, s.Config.PublicURL+"/auth/lark/callback", Str(m, "verifier"))
	if err != nil {
		return "", binding, Fail(401, "lark_login_failed", "授权失败")
	}
	token := ""
	err = s.transaction(ctx, func(tx pgx.Tx) error {
		if e := lockExecutionSettings(ctx, tx); e != nil {
			return e
		}
		current, e := locked(ctx, tx, "configs", Str(c, "id"))
		if e != nil {
			return e
		}
		if Num(current, "version") != Num(m, "connector_version") {
			return invalid
		}
		a, bindingID, resolveErr := resolvePlatformIdentity(ctx, tx, Str(c, "provider"), Str(c, "app_id"), identity.OpenID)
		if resolveErr != nil && !errors.Is(resolveErr, NotFound) {
			return resolveErr
		}
		if binding {
			currentActor, e := authenticate(ctx, tx, session)
			if e != nil || currentActor.ID != Str(m, "principal_id") {
				return invalid
			}
			if resolveErr == nil && a.ID != Str(m, "principal_id") {
				return Fail(409, "identity_already_bound", "此飞书账号已绑定其他成员")
			}
			p, e := one(ctx, tx, "SELECT to_jsonb(p)-'password_hash' FROM principals p WHERE id::text=$1 AND active", Str(m, "principal_id"))
			if e != nil {
				return e
			}
			a = actorFrom(p)
			b := Data{"principal_id": a.ID, "provider": Str(c, "provider"), "app_id": Str(c, "app_id"), "subject_id": identity.OpenID, "display_name": identity.Name, "verified_at": time.Now().UTC().Format(time.RFC3339), "enabled": true}
			if e = validateIdentityBinding(ctx, tx, bindingID, b); e != nil {
				return e
			}
			name := strings.Join([]string{Str(c, "provider"), Str(c, "app_id"), identity.OpenID}, ":")
			if bindingID == "" {
				bindingID = ID()
				_, e = tx.Exec(ctx, "INSERT INTO configs(id,kind,name,data) VALUES($1,'identity-bindings',$2,$3)", bindingID, name, JSON(b))
			} else {
				_, e = tx.Exec(ctx, "UPDATE configs SET data=$2,version=version+1,updated_at=now() WHERE id=$1", bindingID, JSON(b))
			}
			if e != nil {
				return e
			}
			if _, e = tx.Exec(ctx, "INSERT INTO config_versions(config_id,version,data,actor_id) SELECT id,version,data,$2 FROM configs WHERE id=$1", bindingID, a.ID); e != nil {
				return e
			}
			return audit(ctx, tx, a, "bind_identity", "identity-bindings", bindingID, "")
		}
		if resolveErr != nil {
			return Fail(403, "unregistered_identity", "请先用现有账号登录，在个人中心绑定飞书")
		}
		token = Token()
		csrf := Token()
		if _, e = tx.Exec(ctx, "INSERT INTO sessions(id_hash,principal_id,csrf,expires_at,identity_binding_id) VALUES($1,$2,$3,$4,$5)", Hash([]byte(token)), a.ID, csrf, time.Now().Add(s.Config.SessionTTL), bindingID); e != nil {
			return e
		}
		return audit(ctx, tx, a, "login", "identity-bindings", bindingID, "")
	})
	if err != nil {
		token = ""
	}
	return token, binding, err
}

func (s *Service) UnbindIdentity(ctx context.Context, tx pgx.Tx, a Actor, id string, b Data) (Data, error) {
	if err := lockExecutionSettings(ctx, tx); err != nil {
		return nil, err
	}
	m, err := locked(ctx, tx, "configs", id)
	if err != nil {
		return nil, err
	}
	if Str(m, "kind") != "identity-bindings" || Str(Object(m, "data"), "principal_id") != a.ID {
		return nil, NotFound
	}
	if err = checkVersion(m, b); err != nil {
		return nil, err
	}
	var alternative bool
	if err = tx.QueryRow(ctx, `SELECT ($3='local' AND password_hash<>'') OR ($3='oidc' AND COALESCE(subject,'')<>'') OR EXISTS(SELECT 1 FROM configs b WHERE b.kind='identity-bindings' AND b.id::text<>$2 AND b.data->>'principal_id'=$1 AND COALESCE(b.data->>'enabled','true')<>'false' AND EXISTS(SELECT 1 FROM configs c WHERE c.kind='connectors' AND c.data->>'provider'=b.data->>'provider' AND c.data->>'app_id'=b.data->>'app_id' AND c.data->>'user_login_enabled'='true' AND COALESCE(c.data->>'enabled','true')<>'false' AND COALESCE(c.data->>'status','') NOT IN ('disabled','revoked'))) FROM principals WHERE id::text=$1`, a.ID, id, s.Config.AuthMode).Scan(&alternative); err != nil {
		return nil, err
	}
	if !alternative {
		return nil, Fail(409, "last_login_method", "这是你唯一的登录方式，请联系管理员配置其他登录方式后再解绑")
	}
	if _, err = tx.Exec(ctx, "DELETE FROM sessions WHERE identity_binding_id=$1", id); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, "DELETE FROM config_versions WHERE config_id=$1", id); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, "DELETE FROM configs WHERE id=$1", id); err != nil {
		return nil, err
	}
	if err = audit(ctx, tx, a, "unbind_identity", "identity-bindings", id, ""); err != nil {
		return nil, err
	}
	return Data{"status": "unbound"}, nil
}
