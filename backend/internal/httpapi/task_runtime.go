package httpapi

import (
	"agentowl/backend/internal/service"
	"github.com/jackc/pgx/v5"
	"net/http"
)

func (s *Server) taskRuntimeRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/v1/source-threads", func(w http.ResponseWriter, r *http.Request) {
		s.respond(w, r, func() (service.Data, error) { return s.S.SourceThreads(r.Context(), actor(r), r.URL.Query()) })
	})
	for _, part := range []string{"runtime", "inputs", "threads", "authorization"} {
		part := part
		m.HandleFunc("GET /api/v1/development-tasks/{id}/"+part, func(w http.ResponseWriter, r *http.Request) {
			s.respond(w, r, func() (service.Data, error) {
				data, e := s.S.TaskRuntimeDetail(r.Context(), actor(r), r.PathValue("id"), r.URL.Query().Get("cursor"))
				if e != nil {
					return nil, e
				}
				if part == "runtime" {
					return data, nil
				}
				if part == "authorization" {
					return service.Object(data, "authorization"), nil
				}
				return service.Data{"items": data[part], "version": data["version"], "next_cursor": data["next_cursor"]}, nil
			})
		})
	}
	m.HandleFunc("POST /api/v1/development-tasks/{id}/inputs", func(w http.ResponseWriter, r *http.Request) {
		s.change(w, r, func(tx pgx.Tx, a service.Actor, b service.Data) (service.Data, int, error) {
			v, e := s.S.AddTaskInput(r.Context(), tx, a, r.PathValue("id"), b)
			return v, 201, e
		})
	})
	m.HandleFunc("POST /api/v1/development-tasks/{id}/inputs/{input_id}/actions", func(w http.ResponseWriter, r *http.Request) {
		s.change(w, r, func(tx pgx.Tx, a service.Actor, b service.Data) (service.Data, int, error) {
			v, e := s.S.TaskInputAction(r.Context(), tx, a, r.PathValue("id"), r.PathValue("input_id"), b)
			return v, 200, e
		})
	})
	m.HandleFunc("POST /api/v1/development-tasks/{id}/threads", func(w http.ResponseWriter, r *http.Request) {
		s.change(w, r, func(tx pgx.Tx, a service.Actor, b service.Data) (service.Data, int, error) {
			v, e := s.S.AttachTaskThread(r.Context(), tx, a, r.PathValue("id"), b)
			return v, 200, e
		})
	})
	m.HandleFunc("DELETE /api/v1/development-tasks/{id}/threads/{thread_id}", func(w http.ResponseWriter, r *http.Request) {
		s.change(w, r, func(tx pgx.Tx, a service.Actor, b service.Data) (service.Data, int, error) {
			b["thread_id"] = r.PathValue("thread_id")
			b["remove"] = true
			v, e := s.S.AttachTaskThread(r.Context(), tx, a, r.PathValue("id"), b)
			return v, 200, e
		})
	})
	m.HandleFunc("GET /api/v1/settings/admission", func(w http.ResponseWriter, r *http.Request) {
		s.respond(w, r, func() (service.Data, error) { return s.S.GetAdmissionSettings(r.Context(), actor(r)) })
	})
	m.HandleFunc("PUT /api/v1/settings/admission", func(w http.ResponseWriter, r *http.Request) {
		s.change(w, r, func(tx pgx.Tx, a service.Actor, b service.Data) (service.Data, int, error) {
			v, e := s.S.SaveAdmissionSettings(r.Context(), tx, a, b)
			return v, 200, e
		})
	})
	m.HandleFunc("GET /api/v1/admission-decisions", func(w http.ResponseWriter, r *http.Request) {
		s.respond(w, r, func() (service.Data, error) { return s.S.AdmissionDecisions(r.Context(), actor(r), r.URL.Query()) })
	})
	m.HandleFunc("GET /api/v1/admission-decisions/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.respond(w, r, func() (service.Data, error) {
			f := r.URL.Query()
			f.Set("id", r.PathValue("id"))
			return s.S.AdmissionDecisions(r.Context(), actor(r), f)
		})
	})
	m.HandleFunc("POST /api/v1/admission-decisions/{id}/actions", func(w http.ResponseWriter, r *http.Request) {
		s.change(w, r, func(tx pgx.Tx, a service.Actor, b service.Data) (service.Data, int, error) {
			v, e := s.S.AdmissionAction(r.Context(), tx, a, r.PathValue("id"), b)
			return v, 200, e
		})
	})
}
