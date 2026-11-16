package service

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/oauth2"
)

func (s *Service) Bootstrap(ctx context.Context) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// Serialize first-start initialization across API replicas.
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(192837465)"); err != nil {
		return err
	}
	var n int
	if e := tx.QueryRow(ctx, "SELECT count(*) FROM principals WHERE active AND (role='admin' OR 'admin'=ANY(roles))").Scan(&n); e != nil {
		return e
	}
	if n > 0 {
		return nil
	}
	if s.Config.BootstrapEmail == "" {
		return errors.New("BOOTSTRAP_EMAIL is required for the first administrator")
	}
	password := ""
	var subject any
	if s.Config.AuthMode == "local" {
		if len(s.Config.BootstrapPassword) < 12 {
			return errors.New("BOOTSTRAP_PASSWORD must contain at least 12 characters")
		}
		b, e := bcrypt.GenerateFromPassword([]byte(s.Config.BootstrapPassword), bcrypt.DefaultCost)
		if e != nil {
			return e
		}
		password = string(b)
	} else {
		subject = s.Config.BootstrapSubject
	}
	_, e := tx.Exec(ctx, "INSERT INTO principals(id,name,email,role,roles,password_hash,subject) VALUES($1,$2,$3,'admin',ARRAY['admin'],$4,$5) ON CONFLICT(email) DO NOTHING", ID(), s.Config.BootstrapName, strings.ToLower(s.Config.BootstrapEmail), password, subject)
	if e != nil {
		return e
	}
	if e = s.bootstrapRuntimeMachine(ctx, tx); e != nil {
		return e
	}
	return tx.Commit(ctx)
}
func actorFrom(m Data) Actor {
	return Actor{ID: Str(m, "id"), Name: Str(m, "name"), Email: Str(m, "email"), Role: Str(m, "role"), Roles: Strings(m, "roles"), ChannelIDs: Strings(m, "channel_ids"), RepositoryIDs: Strings(m, "repository_ids")}
}
func publicUser(m Data) Data { delete(m, "password_hash"); return m }
func (s *Service) Authenticate(ctx context.Context, cookie string) (Actor, error) {
	return authenticate(ctx, s.DB, cookie)
}
func authenticate(ctx context.Context, q DB, cookie string) (Actor, error) {
	if len(cookie) < 30 {
		return Actor{}, Fail(401, "unauthorized", "请先登录")
	}
	m, e := one(ctx, q, `SELECT to_jsonb(p)||jsonb_build_object('csrf_token',s.csrf) FROM sessions s JOIN principals p ON p.id=s.principal_id WHERE s.id_hash=$1 AND s.expires_at>now() AND p.active
	 AND (s.identity_binding_id='' OR EXISTS(SELECT 1 FROM configs b WHERE b.id::text=s.identity_binding_id AND b.kind='identity-bindings' AND b.data->>'principal_id'=p.id::text AND COALESCE(b.data->>'enabled','true')<>'false'
	 AND EXISTS(SELECT 1 FROM configs c WHERE c.kind='connectors' AND c.data->>'provider'=b.data->>'provider' AND c.data->>'app_id'=b.data->>'app_id' AND c.data->>'user_login_enabled'='true' AND COALESCE(c.data->>'enabled','true')<>'false' AND COALESCE(c.data->>'status','') NOT IN ('disabled','revoked'))))`, Hash([]byte(cookie)))
	if e != nil {
		return Actor{}, Fail(401, "unauthorized", "登录已失效，请重新登录")
	}
	a := actorFrom(m)
	a.CSRF = Str(m, "csrf_token")
	return a, nil
}
func (s *Service) Me(ctx context.Context, a Actor) (Data, error) {
	m, e := one(ctx, s.DB, "SELECT to_jsonb(p)-'password_hash' FROM principals p WHERE id=$1", a.ID)
	if e == nil {
		m["csrf_token"] = a.CSRF
	}
	return m, e
}

func (s *Service) SaveProfile(ctx context.Context, tx pgx.Tx, a Actor, b Data) (Data, error) {
	m, e := locked(ctx, tx, "principals", a.ID)
	if e != nil {
		return nil, e
	}
	if e = checkVersion(m, b); e != nil {
		return nil, e
	}
	name := strings.TrimSpace(Str(b, "name"))
	if name == "" || len(name) > 200 {
		return nil, Fail(422, "validation", "昵称不能为空且不能超过 200 字符")
	}
	hash := Str(m, "password_hash")
	current, next := Str(b, "current_password"), Str(b, "new_password")
	if current != "" || next != "" {
		if s.Config.AuthMode != "local" {
			return nil, Fail(409, "oidc_required", "团队账号密码不能在这里修改")
		}
		if current == "" || next == "" {
			return nil, Fail(422, "validation", "修改密码需要填写当前密码和新密码")
		}
		if len(next) < 12 || len(next) > 72 {
			return nil, Fail(422, "validation", "新密码长度必须为 12～72 字节")
		}
		if hash == "" || bcrypt.CompareHashAndPassword([]byte(hash), []byte(current)) != nil {
			return nil, Fail(401, "invalid_credentials", "当前密码错误")
		}
		h, err := bcrypt.GenerateFromPassword([]byte(next), bcrypt.DefaultCost)
		if err != nil {
			return nil, err
		}
		hash = string(h)
	}
	if _, e = tx.Exec(ctx, "UPDATE principals SET name=$2,password_hash=$3,version=version+1,updated_at=now() WHERE id=$1", a.ID, name, hash); e != nil {
		return nil, e
	}
	if next != "" {
		if _, e = tx.Exec(ctx, "DELETE FROM sessions WHERE principal_id=$1 AND csrf<>$2", a.ID, a.CSRF); e != nil {
			return nil, e
		}
	}
	if e = audit(ctx, tx, a, "update_profile", "principals", a.ID, ""); e != nil {
		return nil, e
	}
	m, e = one(ctx, tx, "SELECT to_jsonb(p)-'password_hash' FROM principals p WHERE id=$1", a.ID)
	if e == nil {
		m["csrf_token"] = a.CSRF
	}
	return m, e
}
func (s *Service) newSession(ctx context.Context, id string) (Data, string, error) {
	token, csrf := Token(), Token()
	_, e := s.DB.Exec(ctx, "INSERT INTO sessions(id_hash,principal_id,csrf,expires_at) VALUES($1,$2,$3,$4)", Hash([]byte(token)), id, csrf, time.Now().Add(s.Config.SessionTTL))
	if e != nil {
		return nil, "", e
	}
	m, e := one(ctx, s.DB, "SELECT to_jsonb(p)-'password_hash' FROM principals p WHERE id=$1", id)
	if e != nil {
		return nil, "", e
	}
	m["csrf_token"] = csrf
	return m, token, nil
}
func (s *Service) LocalLogin(ctx context.Context, email, password string) (Data, string, error) {
	if s.Config.AuthMode != "local" {
		return nil, "", Fail(409, "oidc_required", "请使用团队账号登录")
	}
	m, e := one(ctx, s.DB, "SELECT to_jsonb(p) FROM principals p WHERE email=$1 AND active", strings.ToLower(strings.TrimSpace(email)))
	if e != nil || Str(m, "password_hash") == "" || bcrypt.CompareHashAndPassword([]byte(Str(m, "password_hash")), []byte(password)) != nil {
		return nil, "", Fail(401, "invalid_credentials", "邮箱或密码错误")
	}
	return s.newSession(ctx, Str(m, "id"))
}
func (s *Service) Logout(ctx context.Context, token string) error {
	_, e := s.DB.Exec(ctx, "DELETE FROM sessions WHERE id_hash=$1", Hash([]byte(token)))
	return e
}
func (s *Service) OIDCStart(ctx context.Context) (string, string, error) {
	if s.Config.AuthMode != "oidc" {
		return "", "", Fail(409, "local_login", "当前使用本地账号登录")
	}
	ctx = oidc.ClientContext(ctx, &http.Client{Timeout: 15 * time.Second})
	p, e := oidc.NewProvider(ctx, s.Config.OIDCIssuer)
	if e != nil {
		return "", "", Fail(503, "identity_unavailable", "团队登录服务暂时不可用，请稍后重试")
	}
	state, nonce, verifier := Token(), Token(), oauth2.GenerateVerifier()
	_, e = s.DB.Exec(ctx, "INSERT INTO login_transactions(state_hash,verifier,nonce,expires_at) VALUES($1,$2,$3,now()+interval '10 minutes')", Hash([]byte(state)), verifier, nonce)
	if e != nil {
		return "", "", e
	}
	c := oauth2.Config{ClientID: s.Config.OIDCClientID, ClientSecret: s.Config.OIDCClientSecret, Endpoint: p.Endpoint(), RedirectURL: s.Config.PublicURL + "/auth/callback", Scopes: []string{oidc.ScopeOpenID, "profile", "email"}}
	return c.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier), oidc.Nonce(nonce)), state, nil
}
func (s *Service) OIDCFinish(ctx context.Context, state, code string) (Data, string, error) {
	if s.Config.AuthMode != "oidc" {
		return nil, "", Forbidden
	}
	var verifier, nonce string
	e := s.DB.QueryRow(ctx, "DELETE FROM login_transactions WHERE state_hash=$1 AND expires_at>now() RETURNING verifier,nonce", Hash([]byte(state))).Scan(&verifier, &nonce)
	if e != nil {
		return nil, "", Fail(401, "invalid_login", "登录请求已失效，请重新登录")
	}
	ctx = oidc.ClientContext(ctx, &http.Client{Timeout: 15 * time.Second})
	p, e := oidc.NewProvider(ctx, s.Config.OIDCIssuer)
	if e != nil {
		return nil, "", Fail(503, "identity_unavailable", "团队登录服务暂时不可用，请稍后重试")
	}
	c := oauth2.Config{ClientID: s.Config.OIDCClientID, ClientSecret: s.Config.OIDCClientSecret, Endpoint: p.Endpoint(), RedirectURL: s.Config.PublicURL + "/auth/callback"}
	tok, e := c.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if e != nil {
		return nil, "", Fail(401, "invalid_login", "团队账号验证失败，请重新登录")
	}
	raw, ok := tok.Extra("id_token").(string)
	if !ok {
		return nil, "", Fail(401, "invalid_login", "登录服务未返回有效凭证，请联系管理员")
	}
	id, e := p.Verifier(&oidc.Config{ClientID: s.Config.OIDCClientID}).Verify(ctx, raw)
	if e != nil || subtle.ConstantTimeCompare([]byte(idNonce(id)), []byte(nonce)) != 1 {
		return nil, "", Fail(401, "invalid_login", "登录凭证无效，请重新登录")
	}
	var uid string
	e = s.DB.QueryRow(ctx, "SELECT id FROM principals WHERE subject=$1 AND active", id.Subject).Scan(&uid)
	if errors.Is(e, pgx.ErrNoRows) {
		return nil, "", Fail(403, "unregistered_identity", "你的账号尚未开通访问权限，请联系管理员")
	}
	if e != nil {
		return nil, "", e
	}
	return s.newSession(ctx, uid)
}
func idNonce(t *oidc.IDToken) string {
	if t == nil {
		return ""
	}
	return t.Nonce
}
