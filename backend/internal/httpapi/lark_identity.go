package httpapi

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"agentowl/backend/internal/service"
	"github.com/jackc/pgx/v5"
)

func (s *Server) larkCookie(w http.ResponseWriter, state string) {
	age := 600
	if state == "" {
		age = -1
	}
	http.SetCookie(w, &http.Cookie{Name: "agentowl_lark_login", Value: state, Path: "/auth/lark", MaxAge: age, HttpOnly: true, Secure: strings.HasPrefix(s.S.Config.PublicURL, "https://"), SameSite: http.SameSiteLaxMode})
}

func (s *Server) larkLogin(w http.ResponseWriter, r *http.Request) {
	if !s.allowLogin(r) {
		problem(w, r, service.Fail(429, "rate_limited", "登录尝试过于频繁"))
		return
	}
	location, state, err := s.S.LarkLoginStart(r.Context(), r.URL.Query().Get("connector_id"), service.Actor{}, "")
	if err != nil {
		problem(w, r, err)
		return
	}
	s.larkCookie(w, state)
	http.Redirect(w, r, location, http.StatusFound)
}

func (s *Server) identityRoutes(api *http.ServeMux) {
	api.HandleFunc("GET /api/v1/me/identities", func(w http.ResponseWriter, r *http.Request) {
		s.respond(w, r, func() (service.Data, error) { return s.S.MyIdentities(r.Context(), actor(r)) })
	})
	api.HandleFunc("POST /api/v1/me/identities/bind", func(w http.ResponseWriter, r *http.Request) {
		if !s.allowLogin(r) {
			problem(w, r, service.Fail(429, "rate_limited", "操作过于频繁，请稍后重试"))
			return
		}
		var body struct {
			ConnectorID string `json:"connector_id"`
		}
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") || json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body) != nil {
			problem(w, r, service.Fail(422, "invalid_json", "请选择飞书应用"))
			return
		}
		cookie, err := r.Cookie("agentowl_session")
		if err != nil {
			problem(w, r, service.Forbidden)
			return
		}
		location, state, err := s.S.LarkLoginStart(r.Context(), body.ConnectorID, actor(r), cookie.Value)
		if err != nil {
			problem(w, r, err)
			return
		}
		s.larkCookie(w, state)
		write(w, 200, service.Data{"url": location})
	})
	api.HandleFunc("DELETE /api/v1/me/identities/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.change(w, r, func(tx pgx.Tx, a service.Actor, b service.Data) (service.Data, int, error) {
			m, err := s.S.UnbindIdentity(r.Context(), tx, a, r.PathValue("id"), b)
			return m, 200, err
		})
	})
}

func (s *Server) larkCallback(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	cookie, err := r.Cookie("agentowl_lark_login")
	s.larkCookie(w, "")
	if err != nil || len(state) < 30 || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(state)) != 1 {
		http.Redirect(w, r, "/login?login_error="+url.QueryEscape("飞书授权已失效，请重新操作"), http.StatusSeeOther)
		return
	}
	session := ""
	if c, e := r.Cookie("agentowl_session"); e == nil {
		session = c.Value
	}
	token, binding, err := s.S.LarkLoginFinish(r.Context(), state, r.URL.Query().Get("code"), session)
	path := "/"
	if binding {
		path = "/profile?identity_bound=1"
	}
	if err != nil {
		path = "/login"
		if binding {
			path = "/profile"
		}
		message := "授权失败"
		var failure *service.Error
		if errors.As(err, &failure) {
			message = failure.Message
		}
		path += "?login_error=" + url.QueryEscape(message)
	} else if !binding {
		s.setSession(w, token)
	}
	http.Redirect(w, r, path, http.StatusSeeOther)
}
