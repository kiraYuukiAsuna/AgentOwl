package httpapi

import (
	"net/http"

	"agentowl/backend/internal/service"
	"github.com/jackc/pgx/v5"
)

func (s *Server) settingsRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/v1/settings/model", func(w http.ResponseWriter, r *http.Request) {
		s.respond(w, r, func() (service.Data, error) { return s.S.GetModelSettings(r.Context(), actor(r)) })
	})
	m.HandleFunc("PUT /api/v1/settings/model", func(w http.ResponseWriter, r *http.Request) {
		s.change(w, r, func(tx pgx.Tx, a service.Actor, b service.Data) (service.Data, int, error) {
			v, err := s.S.SaveModelSettings(r.Context(), tx, a, b)
			return v, 200, err
		})
	})
	m.HandleFunc("GET /api/v1/settings/execution", func(w http.ResponseWriter, r *http.Request) {
		s.respond(w, r, func() (service.Data, error) { return s.S.GetExecutionSettings(r.Context(), actor(r)) })
	})
	m.HandleFunc("PUT /api/v1/settings/execution", func(w http.ResponseWriter, r *http.Request) {
		s.change(w, r, func(tx pgx.Tx, a service.Actor, b service.Data) (service.Data, int, error) {
			v, err := s.S.SaveExecutionSettings(r.Context(), tx, a, b)
			return v, 200, err
		})
	})
}
