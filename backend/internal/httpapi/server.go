package httpapi

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"agentowl/backend/internal/integration"
	"agentowl/backend/internal/service"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type Server struct {
	S      *service.Service
	mu     sync.Mutex
	limits map[string]bucket
}
type bucket struct {
	Count int
	Until time.Time
}
type actorKey struct{}

func New(s *service.Service) *Server { return &Server{S: s, limits: map[string]bucket{}} }

func mutableSettingsResource(resource string) bool {
	switch resource {
	case "connectors", "channels", "repositories", "runtime-machines", "policies", "owner-routes", "principals", "identity-bindings", "knowledge-sources":
		return true
	default:
		return false
	}
}
func (s *Server) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /auth/config", func(w http.ResponseWriter, r *http.Request) {
		apps, err := s.S.LarkLoginApps(r.Context())
		if err != nil {
			problem(w, r, err)
			return
		}
		write(w, 200, service.Data{"mode": s.S.Config.AuthMode, "lark_apps": apps})
	})
	m.HandleFunc("GET /health/live", func(w http.ResponseWriter, r *http.Request) { write(w, 200, service.Data{"status": "ok"}) })
	m.HandleFunc("GET /health/ready", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if s.S.DB.Ping(ctx) != nil {
			problem(w, r, service.Fail(503, "database_unavailable", "数据库不可用"))
			return
		}
		write(w, 200, service.Data{"status": "ready"})
	})
	m.HandleFunc("POST /auth/login", s.login)
	m.HandleFunc("GET /auth/login", s.oidcStart)
	m.HandleFunc("GET /auth/callback", s.oidcCallback)
	m.HandleFunc("GET /auth/lark/login", s.larkLogin)
	m.HandleFunc("GET /auth/lark/callback", s.larkCallback)
	m.Handle("POST /auth/logout", s.auth(http.HandlerFunc(s.logout)))
	api := http.NewServeMux()
	s.settingsRoutes(api)
	s.taskRuntimeRoutes(api)
	s.identityRoutes(api)
	api.HandleFunc("GET /api/v1/cases/{id}/attachments/{index}/content", s.caseAttachment)
	api.HandleFunc("GET /api/v1/me", func(w http.ResponseWriter, r *http.Request) {
		s.respond(w, r, func() (service.Data, error) { return s.S.Me(r.Context(), actor(r)) })
	})
	api.HandleFunc("PATCH /api/v1/me", func(w http.ResponseWriter, r *http.Request) {
		s.change(w, r, func(tx pgx.Tx, a service.Actor, b service.Data) (service.Data, int, error) {
			m, e := s.S.SaveProfile(r.Context(), tx, a, b)
			return m, 200, e
		})
	})
	api.HandleFunc("GET /api/v1/dashboard", func(w http.ResponseWriter, r *http.Request) {
		s.respond(w, r, func() (service.Data, error) { return s.S.Dashboard(r.Context(), actor(r)) })
	})
	for _, name := range []string{"cases", "knowledge", "knowledge-sources", "connectors", "channels", "repositories", "runtime-machines", "policies", "owner-routes", "principals", "identity-bindings", "admission-rules", "approvals", "development-tasks", "audit-events", "external-operations", "jobs"} {
		resource := name
		api.HandleFunc("GET /api/v1/"+resource, func(w http.ResponseWriter, r *http.Request) {
			s.respond(w, r, func() (service.Data, error) { return s.S.List(r.Context(), actor(r), resource, r.URL.Query()) })
		})
		api.HandleFunc("GET /api/v1/"+resource+"/{id}", func(w http.ResponseWriter, r *http.Request) {
			s.respond(w, r, func() (service.Data, error) { return s.S.Detail(r.Context(), actor(r), resource, r.PathValue("id")) })
		})
		if resource == "approvals" || resource == "development-tasks" || resource == "audit-events" || resource == "external-operations" || resource == "jobs" {
			continue
		}
		save := func(w http.ResponseWriter, r *http.Request) {
			s.change(w, r, func(tx pgx.Tx, a service.Actor, b service.Data) (service.Data, int, error) {
				id := r.PathValue("id")
				var v service.Data
				var e error
				switch resource {
				case "cases":
					v, e = s.S.SaveCase(r.Context(), tx, a, id, b)
				case "knowledge":
					v, e = s.S.SaveKnowledge(r.Context(), tx, a, id, b)
				case "principals":
					v, e = s.S.SaveUser(r.Context(), tx, a, id, b)
				default:
					v, e = s.S.SaveConfig(r.Context(), tx, a, resource, id, b)
				}
				status := 200
				if id == "" {
					status = 201
				}
				return v, status, e
			})
		}
		api.HandleFunc("POST /api/v1/"+resource, save)
		api.HandleFunc("PATCH /api/v1/"+resource+"/{id}", save)
		if mutableSettingsResource(resource) {
			remove := func(w http.ResponseWriter, r *http.Request) {
				s.change(w, r, func(tx pgx.Tx, a service.Actor, b service.Data) (service.Data, int, error) {
					id := r.PathValue("id")
					var v service.Data
					var e error
					if resource == "principals" {
						v, e = s.S.DeleteUser(r.Context(), tx, a, id, b)
					} else {
						v, e = s.S.DeleteConfig(r.Context(), tx, a, resource, id, b)
					}
					return v, 200, e
				})
			}
			api.HandleFunc("DELETE /api/v1/"+resource+"/{id}", remove)
		}
	}
	for _, resource := range []string{"cases", "run-attempts"} {
		res := resource
		api.HandleFunc("GET /api/v1/"+res+"/{id}/events", func(w http.ResponseWriter, r *http.Request) {
			s.respond(w, r, func() (service.Data, error) {
				return s.S.Events(r.Context(), actor(r), res, r.PathValue("id"), r.URL.Query().Get("cursor"))
			})
		})
	}
	s.action(api, "cases", "actions", 200, func(ctx context.Context, tx pgx.Tx, a service.Actor, id string, b service.Data) (service.Data, error) {
		return s.S.CaseAction(ctx, tx, a, id, b)
	})
	s.action(api, "cases", "feedback", 200, s.S.Feedback)
	s.action(api, "cases", "relations", 200, s.S.Relate)
	s.action(api, "cases", "reply-drafts", 202, s.S.CreateReply)
	s.action(api, "replies", "submit", 200, s.S.SubmitReply)
	s.action(api, "cases", "development-confirmations", 201, s.S.ConfirmDevelopment)
	s.action(api, "development-tasks", "revisions", 200, s.S.ReviseTask)
	s.action(api, "development-tasks", "actions", 200, s.S.TaskAction)
	s.action(api, "approvals", "decisions", 200, s.S.Decide)
	s.action(api, "knowledge", "versions", 200, s.S.SaveKnowledge)
	for _, act := range []string{"submit", "withdraw"} {
		a := act
		s.action(api, "knowledge", act, 200, func(ctx context.Context, tx pgx.Tx, u service.Actor, id string, b service.Data) (service.Data, error) {
			return s.S.KnowledgeAction(ctx, tx, u, id, a, b)
		})
	}
	api.HandleFunc("GET /api/v1/knowledge/{id}/versions", func(w http.ResponseWriter, r *http.Request) {
		s.respond(w, r, func() (service.Data, error) { return s.S.KnowledgeVersions(r.Context(), actor(r), r.PathValue("id")) })
	})
	for _, res := range []string{"knowledge-sources", "channels", "connectors", "external-operations"} {
		resource := res
		action := "sync"
		if res == "connectors" {
			action = "verify"
		}
		if res == "external-operations" {
			action = "reconcile"
		}
		s.action(api, res, action, 202, func(ctx context.Context, tx pgx.Tx, u service.Actor, id string, b service.Data) (service.Data, error) {
			return s.S.ScheduleResource(ctx, tx, u, resource, id, b)
		})
	}
	s.action(api, "jobs", "retry", 200, s.S.RetryJob)
	s.action(api, "jobs", "ignore", 200, s.S.IgnoreJob)
	api.HandleFunc("GET /api/v1/artifacts/{id}/content", s.download)
	m.Handle("/api/v1/", s.auth(api))
	m.HandleFunc("POST /hooks/{provider}/{connector_id}/events", s.webhook)
	m.HandleFunc("POST /hooks/{provider}/{connector_id}/actions", s.cardAction)
	return s.middleware(m)
}

type actionFunc func(context.Context, pgx.Tx, service.Actor, string, service.Data) (service.Data, error)

func (s *Server) action(m *http.ServeMux, resource, action string, status int, fn actionFunc) {
	m.HandleFunc("POST /api/v1/"+resource+"/{id}/"+action, func(w http.ResponseWriter, r *http.Request) {
		s.change(w, r, func(tx pgx.Tx, a service.Actor, b service.Data) (service.Data, int, error) {
			m, e := fn(r.Context(), tx, a, r.PathValue("id"), b)
			return m, status, e
		})
	})
}
func (s *Server) respond(w http.ResponseWriter, r *http.Request, fn func() (service.Data, error)) {
	v, e := fn()
	if e != nil {
		problem(w, r, e)
		return
	}
	write(w, 200, v)
}
func (s *Server) change(w http.ResponseWriter, r *http.Request, fn func(pgx.Tx, service.Actor, service.Data) (service.Data, int, error)) {
	raw, e := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if e != nil {
		problem(w, r, service.Fail(413, "body_too_large", "请求体过大"))
		return
	}
	b := service.Data{}
	if len(raw) == 0 {
		raw = []byte("{}")
	} else if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		problem(w, r, service.Fail(415, "content_type", "需要 JSON 请求"))
		return
	}
	if json.Unmarshal(raw, &b) != nil {
		problem(w, r, service.Fail(422, "invalid_json", "JSON 内容无效"))
		return
	}
	a := actor(r)
	v, status, e := s.S.Mutate(r.Context(), a, r.Header.Get("Idempotency-Key"), service.Hash(append([]byte(r.Method+":"+r.URL.Path+":"), raw...)), func(tx pgx.Tx) (service.Data, int, error) { return fn(tx, a, b) })
	if e != nil {
		problem(w, r, e)
		return
	}
	write(w, status, v)
}
func actor(r *http.Request) service.Actor {
	a, _ := r.Context().Value(actorKey{}).(service.Actor)
	return a
}
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, e := r.Cookie("agentowl_session")
		if e != nil {
			problem(w, r, service.Fail(401, "unauthorized", "请先登录"))
			return
		}
		a, e := s.S.Authenticate(r.Context(), c.Value)
		if e != nil {
			problem(w, r, e)
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" {
			if !s.validOrigin(r) || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(a.CSRF)) != 1 {
				problem(w, r, service.Fail(403, "csrf_failed", "请求来源校验失败"))
				return
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), actorKey{}, a)))
	})
}
func (s *Server) validOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	return o == "" || o == strings.TrimSuffix(s.S.Config.PublicURL, "/")
}
func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-ID", service.ID())
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Frame-Options", "DENY")
		defer func() {
			if v := recover(); v != nil {
				slog.Error("request panic", "request_id", w.Header().Get("X-Request-ID"))
				problem(w, r, errors.New("internal panic"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}
func write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func problem(w http.ResponseWriter, r *http.Request, e error) {
	status, code, msg := 500, "internal_error", "服务暂时不可用"
	var se *service.Error
	if errors.As(e, &se) {
		status, code, msg = se.Status, se.Code, se.Message
	} else {
		var upstream *integration.APIError
		if errors.As(e, &upstream) {
			status, code, msg = 502, "upstream_error", upstream.Diagnostic()
		}
		var pe *pgconn.PgError
		if errors.As(e, &pe) {
			if pe.Code == "23505" {
				status, code, msg = 409, "conflict", "已有相同记录或操作"
			} else if pe.Code == "23503" || pe.Code == "22P02" {
				status, code, msg = 422, "invalid_reference", "关联对象或输入无效"
			}
		}
		slog.Error("request failed", "request_id", w.Header().Get("X-Request-ID"), "code", code)
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(service.Data{"type": "about:blank", "title": http.StatusText(status), "status": status, "detail": msg, "code": code, "request_id": w.Header().Get("X-Request-ID")})
}
func (s *Server) allowLogin(r *http.Request) bool {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	key := "login:" + host
	if s.S.Redis != nil {
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		n, e := s.S.Redis.Incr(ctx, key).Result()
		if e == nil {
			if n == 1 {
				s.S.Redis.Expire(ctx, key, time.Minute)
			}
			return n <= 10
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if len(s.limits) > 10000 {
		for k, v := range s.limits {
			if !v.Until.After(now) {
				delete(s.limits, k)
			}
		}
		if len(s.limits) > 10000 {
			return false
		}
	}
	b := s.limits[key]
	if !b.Until.After(now) {
		b = bucket{Until: now.Add(time.Minute)}
	}
	b.Count++
	s.limits[key] = b
	return b.Count <= 10
}
func (s *Server) setSession(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{Name: "agentowl_session", Value: token, Path: "/", Secure: strings.HasPrefix(s.S.Config.PublicURL, "https://"), HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: int(s.S.Config.SessionTTL.Seconds())})
}
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if !s.validOrigin(r) {
		problem(w, r, service.Forbidden)
		return
	}
	if !s.allowLogin(r) {
		problem(w, r, service.Fail(429, "rate_limited", "登录尝试过于频繁"))
		return
	}
	var b struct{ Email, Password string }
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") || json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&b) != nil {
		problem(w, r, service.Fail(422, "invalid_json", "登录信息无效"))
		return
	}
	m, t, e := s.S.LocalLogin(r.Context(), b.Email, b.Password)
	if e != nil {
		problem(w, r, e)
		return
	}
	s.setSession(w, t)
	write(w, 200, m)
}
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	c, _ := r.Cookie("agentowl_session")
	if c != nil {
		if e := s.S.Logout(r.Context(), c.Value); e != nil {
			problem(w, r, e)
			return
		}
	}
	http.SetCookie(w, &http.Cookie{Name: "agentowl_session", Path: "/", MaxAge: -1, HttpOnly: true, Secure: strings.HasPrefix(s.S.Config.PublicURL, "https://"), SameSite: http.SameSiteLaxMode})
	write(w, 200, service.Data{"status": "logged_out"})
}
func (s *Server) oidcStart(w http.ResponseWriter, r *http.Request) {
	location, state, e := s.S.OIDCStart(r.Context())
	if e != nil {
		problem(w, r, e)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "agentowl_login", Value: state, Path: "/auth", MaxAge: 600, HttpOnly: true, Secure: strings.HasPrefix(s.S.Config.PublicURL, "https://"), SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, location, http.StatusFound)
}
func (s *Server) oidcCallback(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	c, e := r.Cookie("agentowl_login")
	if e != nil || len(state) < 30 || subtle.ConstantTimeCompare([]byte(c.Value), []byte(state)) != 1 {
		problem(w, r, service.Fail(401, "invalid_login", "登录请求不匹配"))
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "agentowl_login", Path: "/auth", MaxAge: -1, HttpOnly: true})
	_, token, e := s.S.OIDCFinish(r.Context(), state, r.URL.Query().Get("code"))
	if e != nil {
		problem(w, r, e)
		return
	}
	s.setSession(w, token)
	http.Redirect(w, r, s.S.Config.PublicURL+"/", http.StatusSeeOther)
}
func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	a, e := s.S.Get(r.Context(), s.S.DB, actor(r), "artifacts", r.PathValue("id"))
	if e != nil {
		problem(w, r, e)
		return
	}
	body, e := s.S.ReadArtifactContent(r.Context(), service.Str(a, "id"))
	if e != nil {
		problem(w, r, e)
		return
	}
	w.Header().Set("Content-Type", service.Str(a, "media_type"))
	w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(service.Str(a, "name")))
	http.ServeContent(w, r, service.Str(a, "name"), time.Time{}, bytes.NewReader(body))
}

func (s *Server) webhook(w http.ResponseWriter, r *http.Request) {
	body, e := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20))
	if e != nil {
		problem(w, r, service.Fail(413, "body_too_large", "事件过大"))
		return
	}
	v, e := s.S.IngestWebhook(r.Context(), r.PathValue("provider"), r.PathValue("connector_id"), r.Header, body)
	if e != nil {
		problem(w, r, e)
		return
	}
	write(w, 200, v)
}

func (s *Server) cardAction(w http.ResponseWriter, r *http.Request) {
	body, e := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20))
	if e != nil {
		problem(w, r, service.Fail(413, "body_too_large", "事件过大"))
		return
	}
	v, e := s.S.IngestCardAction(r.Context(), r.PathValue("provider"), r.PathValue("connector_id"), r.Header, body)
	if e != nil {
		problem(w, r, e)
		return
	}
	write(w, 200, v)
}

func (s *Server) internalRunnerAuthorized(r *http.Request) bool {
	provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	machineID := strings.TrimSpace(r.URL.Query().Get("machine_id"))
	if machineID == "" {
		return false
	}
	_, secret, err := s.S.RuntimeMachineSecret(r.Context(), machineID)
	if err != nil {
		return false
	}
	token := strings.TrimSpace(service.Str(secret, "callback_token"))
	return token != "" && subtle.ConstantTimeCompare([]byte(provided), []byte(token)) == 1
}
func (s *Server) InternalHandler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /internal/runner/settings", func(w http.ResponseWriter, r *http.Request) {
		if !s.internalRunnerAuthorized(r) {
			problem(w, r, service.Forbidden)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		s.respond(w, r, func() (service.Data, error) { return s.S.RunnerSettings(r.Context()) })
	})
	m.HandleFunc("POST /internal/runner/authorize", func(w http.ResponseWriter, r *http.Request) {
		if !s.internalRunnerAuthorized(r) {
			problem(w, r, service.Forbidden)
			return
		}
		var b service.Data
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&b) != nil {
			problem(w, r, service.Fail(422, "invalid_json", "无效许可请求"))
			return
		}
		v, e := s.S.AuthorizeRun(r.Context(), b)
		if e != nil {
			problem(w, r, e)
			return
		}
		write(w, 200, v)
	})
	m.HandleFunc("GET /internal/runner/context/{run_id}/manifest", func(w http.ResponseWriter, r *http.Request) {
		if !s.internalRunnerAuthorized(r) || s.S.CheckRunMachine(r.Context(), r.PathValue("run_id"), r.URL.Query().Get("machine_id")) != nil {
			problem(w, r, service.Forbidden)
			return
		}
		s.respond(w, r, func() (service.Data, error) { return s.S.RunnerContextManifest(r.Context(), r.PathValue("run_id")) })
	})
	m.HandleFunc("GET /internal/runner/context/{run_id}/refs/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !s.internalRunnerAuthorized(r) || s.S.CheckRunMachine(r.Context(), r.PathValue("run_id"), r.URL.Query().Get("machine_id")) != nil {
			problem(w, r, service.Forbidden)
			return
		}
		if protocol := r.URL.Query().Get("protocol"); protocol != "" && protocol != "1" {
			if protocol == "3" {
				s.respond(w, r, func() (service.Data, error) {
					return s.S.RunnerContextPage(r.Context(), r.PathValue("run_id"), r.PathValue("id"), r.URL.Query().Get("cursor"))
				})
				return
			}
			if protocol != "2" {
				problem(w, r, service.Fail(422, "protocol_version", "不支持的引用协议"))
				return
			}
			s.respond(w, r, func() (service.Data, error) {
				return s.S.TaskInputPage(r.Context(), r.PathValue("run_id"), r.PathValue("id"), r.URL.Query().Get("cursor"))
			})
			return
		}
		body, e := s.S.RunnerContextRef(r.Context(), r.PathValue("run_id"), r.PathValue("id"))
		if e != nil {
			problem(w, r, e)
			return
		}
		write(w, 200, service.Data{"body": body})
	})
	return s.middleware(m)
}
