package httpapi

import (
	"mime"
	"net/http"
	"strconv"

	"agentowl/backend/internal/service"
)

func (s *Server) caseAttachment(w http.ResponseWriter, r *http.Request) {
	index, e := strconv.Atoi(r.PathValue("index"))
	if e != nil || index < 0 {
		problem(w, r, service.Fail(422, "invalid_attachment_index", "附件索引无效"))
		return
	}
	version, e := strconv.ParseInt(r.URL.Query().Get("expected_version"), 10, 64)
	if e != nil || version < 1 {
		problem(w, r, service.Fail(422, "attachment_version_required", "必须提供有效的 expected_version，请刷新问题后重试"))
		return
	}
	content, e := s.S.ReadCaseAttachment(r.Context(), actor(r), r.PathValue("id"), index, version)
	if e != nil {
		problem(w, r, e)
		return
	}
	w.Header().Set("Content-Type", content.MediaType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": content.Filename}))
	w.Header().Set("Content-Length", strconv.Itoa(len(content.Bytes)))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-AgentOwl-Case-Version", strconv.FormatInt(content.Version, 10))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content.Bytes)
}
