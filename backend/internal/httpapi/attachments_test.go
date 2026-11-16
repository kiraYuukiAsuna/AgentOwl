package httpapi

import (
	"net/http/httptest"
	"testing"
)

func TestAttachmentSelectionRequiresVersionAndNumericIndex(t *testing.T) {
	// Invalid client selections must fail before touching a provider or database.
	server := &Server{}
	for _, tc := range []struct{ index, query string }{{"0", ""}, {"0", "?expected_version=0"}, {"0", "?expected_version=invalid"}, {"-1", "?expected_version=1"}, {"file_key", "?expected_version=1"}} {
		r := httptest.NewRequest("GET", "/api/v1/cases/test/attachments/"+tc.index+"/content"+tc.query, nil)
		r.SetPathValue("index", tc.index)
		w := httptest.NewRecorder()
		server.caseAttachment(w, r)
		if w.Code != 422 {
			t.Fatalf("index=%s query=%s status=%d", tc.index, tc.query, w.Code)
		}
	}
}
