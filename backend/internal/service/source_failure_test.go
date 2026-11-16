package service

import (
	"context"
	"testing"
)

func TestSourceSyncFailurePersistsAndReturnsError(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	id := supportConfig(t, s, "knowledge-sources", Data{"kind": "invalid", "enabled": true}, nil)
	err := s.syncSupportSource(ctx, Data{"source_id": id, "version": 1})
	if err == nil {
		t.Fatal("source failure reported success")
	}
	raw, err := locked(ctx, s.DB, "configs", id)
	if err != nil {
		t.Fatal(err)
	}
	data := Object(raw, "data")
	if Str(data, "status") != "failed" || Str(data, "last_error") == "" {
		t.Fatal("source error was rolled back")
	}
}
