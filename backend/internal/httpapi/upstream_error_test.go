package httpapi

import (
	"agentowl/backend/internal/integration"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProblemReturnsSanitizedUpstreamError(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/test", nil)
	err := fmt.Errorf("wrapped: %w", &integration.APIError{Provider: "lark", StatusCode: 400, Code: "230002", Message: "Bot/User can NOT be out of the chat.", RequestID: "log-123"})
	problem(w, r, err)
	if w.Code != 502 {
		t.Fatalf("status=%d", w.Code)
	}
	for _, want := range []string{"230002", "HTTP 400", "Bot/User", "log-123"} {
		if !strings.Contains(w.Body.String(), want) {
			t.Fatalf("missing %s: %s", want, w.Body.String())
		}
	}
	w = httptest.NewRecorder()
	problem(w, r, errors.New("private internal detail"))
	if strings.Contains(w.Body.String(), "private internal detail") {
		t.Fatal("unknown error leaked")
	}
}
