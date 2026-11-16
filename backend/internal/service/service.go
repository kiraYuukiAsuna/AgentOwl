package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"agentowl/backend/internal/integration"
	"agentowl/backend/internal/platform"
	"agentowl/backend/internal/runner"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

type Data map[string]any
type DB interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}
type Error struct {
	Status        int
	Code, Message string
}

func (e *Error) Error() string                { return e.Message }
func Fail(status int, code, msg string) error { return &Error{status, code, msg} }

var NotFound = Fail(404, "not_found", "找不到这项内容，或你没有查看权限")
var Conflict = Fail(409, "version_conflict", "内容已更新，请刷新后再操作")
var Forbidden = Fail(403, "forbidden", "你没有权限执行此操作，请联系管理员")

type Actor struct {
	ID, Name, Email, Role, CSRF      string
	Roles, ChannelIDs, RepositoryIDs []string
}

func (a Actor) Has(roles ...string) bool {
	if a.Role == "admin" || contains(a.Roles, "admin") {
		return true
	}
	for _, r := range roles {
		if a.Role == r || contains(a.Roles, r) {
			return true
		}
	}
	return false
}
func (a Actor) Channel(id string) bool {
	return a.Has("admin") || contains(a.ChannelIDs, "*") || contains(a.ChannelIDs, id)
}
func (a Actor) Repository(id string) bool {
	return a.Has("admin") || contains(a.RepositoryIDs, "*") || contains(a.RepositoryIDs, id)
}

type RunnerAPI interface {
	Capabilities(context.Context) (runner.Capabilities, error)
	Start(context.Context, runner.RunSpec) (runner.Run, error)
	Inspect(context.Context, string) (runner.Run, error)
	Stop(context.Context, string) (runner.Run, error)
	Collect(context.Context, string) (io.ReadCloser, error)
	Acknowledge(context.Context, string) error
	ContextManifest(context.Context, string) (runner.ContextManifest, error)
	ContextRef(context.Context, string, string) (string, error)
}

type Service struct {
	DB     *pgxpool.Pool
	Config platform.Config
	Vault  *integration.Vault
	Model  *integration.ModelClient // Test injection only; production reads system_settings.
	Redis  *redis.Client
}

func New(db *pgxpool.Pool, c platform.Config) (*Service, error) {
	v, e := integration.NewVault(c.EncryptionKey)
	if e != nil {
		return nil, e
	}
	s := &Service{DB: db, Config: c, Vault: v}
	if c.RedisURL != "" {
		o, e := redis.ParseURL(c.RedisURL)
		if e != nil {
			return nil, e
		}
		s.Redis = redis.NewClient(o)
	}
	return s, nil
}
func ID() string {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
func Token() string {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func Hash(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
func JSON(v any) []byte {
	b, e := json.Marshal(v)
	if e != nil {
		panic(e)
	}
	return b
}
func Str(m Data, k string) string { v, _ := m[k].(string); return v }
func Num(m Data, k string) int64 {
	switch v := m[k].(type) {
	case float64:
		return int64(v)
	case int:
		return int64(v)
	case int64:
		return v
	case json.Number:
		n, _ := v.Int64()
		return n
	case string:
		n, _ := strconv.ParseInt(v, 10, 64)
		return n
	}
	return 0
}
func Bool(m Data, k string) bool { b, _ := m[k].(bool); return b }
func Strings(m Data, k string) []string {
	r := []string{}
	switch a := m[k].(type) {
	case []string:
		return a
	case []any:
		for _, x := range a {
			if s, ok := x.(string); ok {
				r = append(r, s)
			}
		}
	}
	return r
}
func Object(m Data, k string) Data {
	switch v := m[k].(type) {
	case Data:
		return v
	case map[string]any:
		return Data(v)
	}
	return Data{}
}
func contains(a []string, s string) bool {
	for _, v := range a {
		if v == s {
			return true
		}
	}
	return false
}
func fallback(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

func nullIfBlank(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}
func require(m Data, keys ...string) error {
	for _, k := range keys {
		if strings.TrimSpace(Str(m, k)) == "" {
			return Fail(422, "validation", k+" 不能为空")
		}
	}
	return nil
}
func validID(id string) bool {
	if len(id) != 36 {
		return false
	}
	_, e := hex.DecodeString(strings.ReplaceAll(id, "-", ""))
	return e == nil
}
func one(ctx context.Context, q DB, sql string, args ...any) (Data, error) {
	var b []byte
	e := q.QueryRow(ctx, sql, args...).Scan(&b)
	if errors.Is(e, pgx.ErrNoRows) {
		return nil, NotFound
	}
	if e != nil {
		return nil, e
	}
	m := Data{}
	e = json.Unmarshal(b, &m)
	return m, e
}
func many(ctx context.Context, q DB, sql string, args ...any) ([]Data, error) {
	rows, e := q.Query(ctx, sql, args...)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Data{}
	for rows.Next() {
		var b []byte
		if e = rows.Scan(&b); e != nil {
			return nil, e
		}
		m := Data{}
		if e = json.Unmarshal(b, &m); e != nil {
			return nil, e
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
func locked(ctx context.Context, q DB, table, id string) (Data, error) {
	if !validID(id) {
		return nil, NotFound
	}
	return one(ctx, q, "SELECT to_jsonb(t) FROM "+table+" t WHERE id=$1 FOR UPDATE", id)
}
func checkVersion(d, b Data) error {
	if Num(b, "expected_version") != Num(d, "version") {
		return Conflict
	}
	return nil
}
func audit(ctx context.Context, q DB, a Actor, action, kind, id string, detail string) error {
	_, e := q.Exec(ctx, "INSERT INTO audit_events(id,actor_id,action,resource_type,resource_id,detail) VALUES($1,$2,$3,$4,$5,$6)", ID(), a.ID, action, kind, id, integration.Redact(detail))
	return e
}
func caseEvent(ctx context.Context, q DB, id, kind, actor, body string, meta Data) error {
	_, e := q.Exec(ctx, "INSERT INTO case_events(id,case_id,type,actor_id,body,metadata) VALUES($1,$2,$3,$4,$5,$6)", ID(), id, kind, actor, integration.Redact(body), JSON(meta))
	return e
}
func enqueue(ctx context.Context, q DB, kind, key string, payload Data, delay time.Duration) error {
	_, e := q.Exec(ctx, "INSERT INTO jobs(id,kind,dedupe_key,payload,available_at) VALUES($1,$2,$3,$4,$5) ON CONFLICT(dedupe_key) DO NOTHING", ID(), kind, key, JSON(payload), time.Now().Add(delay))
	return e
}

func (s *Service) Mutate(ctx context.Context, a Actor, key, hash string, fn func(pgx.Tx) (Data, int, error)) (Data, int, error) {
	if len(key) < 8 || len(key) > 200 {
		return nil, 0, Fail(422, "idempotency_required", "需要有效的 Idempotency-Key")
	}
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return nil, 0, e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", a.ID+":"+key); e != nil {
		return nil, 0, e
	}
	var old string
	var body []byte
	var status int
	e = tx.QueryRow(ctx, "SELECT request_hash,response,status_code FROM idempotency_keys WHERE actor_id=$1 AND key=$2", a.ID, key).Scan(&old, &body, &status)
	if e == nil {
		if old != hash {
			return nil, 0, Fail(409, "idempotency_conflict", "此请求标识已用于其他内容")
		}
		m := Data{}
		if e = json.Unmarshal(body, &m); e != nil {
			return nil, 0, e
		}
		return m, status, tx.Commit(ctx)
	}
	if !errors.Is(e, pgx.ErrNoRows) {
		return nil, 0, e
	}
	m, status, e := fn(tx)
	if e != nil {
		return nil, 0, e
	}
	if _, e = tx.Exec(ctx, "INSERT INTO idempotency_keys(actor_id,key,request_hash,response,status_code) VALUES($1,$2,$3,$4,$5)", a.ID, key, hash, JSON(m), status); e != nil {
		return nil, 0, e
	}
	if e = tx.Commit(ctx); e != nil {
		return nil, 0, e
	}
	return m, status, nil
}

func (s *Service) canCase(a Actor, c Data) bool {
	return a.Has("admin") || a.ID == Str(c, "created_by") || a.ID == Str(c, "assignee_id") || Str(c, "channel_id") != "" && a.Channel(Str(c, "channel_id"))
}
func (s *Service) canKnowledge(a Actor, k Data) bool {
	return a.Has("admin") || Str(k, "visibility") == "public" || a.ID == Str(k, "author_id") || Str(k, "visibility") == "internal" && a.ID != "" || intersects(a.ChannelIDs, Strings(k, "channel_ids"))
}
func intersects(a, b []string) bool {
	for _, x := range a {
		if contains(b, x) || x == "*" {
			return true
		}
	}
	return false
}
func (s *Service) config(ctx context.Context, q DB, id, kind string) (Data, Data, error) {
	if !validID(id) {
		return nil, nil, NotFound
	}
	m, e := one(ctx, q, "SELECT to_jsonb(c) FROM configs c WHERE id=$1 AND kind=$2", id, kind)
	if e != nil {
		return nil, nil, e
	}
	secret := Data{}
	if enc := Str(m, "secrets"); enc != "" {
		b, e := s.Vault.Open(enc, []byte(kind+":"+id))
		if e != nil {
			return nil, nil, e
		}
		if e = json.Unmarshal(b, &secret); e != nil {
			return nil, nil, e
		}
	}
	return configPublic(m), secret, nil
}
func configPublic(m Data) Data {
	d := Object(m, "data")
	for _, k := range []string{"id", "name", "version", "created_at", "updated_at"} {
		d[k] = m[k]
	}
	d["secret_configured"] = Str(m, "secrets") != ""
	return d
}
