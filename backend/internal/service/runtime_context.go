package service

import (
	"agentowl/backend/internal/runner"
	"context"
)

type RuntimeContextProvider struct{ Service *Service }

func (p RuntimeContextProvider) input(ctx context.Context, runID string) (Data, error) {
	if p.Service == nil || !validID(runID) {
		return nil, NotFound
	}
	r, err := one(ctx, p.Service.DB, "SELECT to_jsonb(r) FROM run_attempts r WHERE id=$1", runID)
	if err != nil {
		return nil, err
	}
	permit, err := p.Service.AuthorizeRun(ctx, Data{"run_id": runID, "attempt_id": runID, "task_id": Str(r, "task_id"), "step_id": Str(r, "step_id"), "epoch": Num(r, "epoch"), "action_digest": Str(r, "action_digest"), "spec_digest": Str(r, "spec_digest"), "phase": "renew"})
	if err != nil {
		return nil, err
	}
	if !Bool(permit, "allowed") {
		return nil, Forbidden
	}
	step, err := one(ctx, p.Service.DB, "SELECT to_jsonb(st) FROM task_steps st WHERE id=$1", Str(r, "step_id"))
	if err != nil {
		return nil, err
	}
	input := Object(step, "input")
	if sources, ok := input["context_sources"]; ok {
		var invalid bool
		err = p.Service.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM jsonb_to_recordset($1::jsonb) AS ref(id text,version bigint) LEFT JOIN source_messages m ON m.id::text=ref.id WHERE m.id IS NULL OR m.deleted OR m.version<>ref.version OR EXISTS(SELECT 1 FROM ingest_events e WHERE e.connector_id=m.connector_id AND e.status='pending' AND e.payload->'event'->>'message_id'=m.external_id AND e.payload->>'channel_id'=m.channel_id AND (COALESCE((e.payload->'event'->>'deleted')::boolean,false) OR COALESCE(e.payload->'event'->>'source_revision','')<>m.source_revision)))`, JSON(sources)).Scan(&invalid)
		if err != nil {
			return nil, err
		}
		if invalid {
			return nil, Fail(409, "input_changed", "上下文引用的消息已更新")
		}
	}
	return input, nil
}

func (p RuntimeContextProvider) ContextManifest(ctx context.Context, runID string) (runner.ContextManifest, error) {
	input, err := p.input(ctx, runID)
	if err != nil {
		return runner.ContextManifest{}, err
	}
	manifest := p.Service.buildContextManifest(ctx, input)
	for i := range manifest.Refs {
		manifest.Refs[i].Inline = ""
	}
	manifest.TokenBudget, err = runner.EstimateJSONTokens(manifest)
	return manifest, err
}

func (p RuntimeContextProvider) ContextRef(ctx context.Context, runID, refID string) (string, error) {
	input, err := p.input(ctx, runID)
	if err != nil {
		return "", err
	}
	for _, ref := range p.Service.buildContextManifest(ctx, input).Refs {
		if ref.ID != refID {
			continue
		}
		if body := Str(Object(input, "context_documents"), refID); body != "" {
			return body, nil
		}
		if ref.Inline != "" {
			return ref.Inline, nil
		}
		if ref.ArtifactID != "" {
			body, e := p.Service.readArtifact(ctx, ref.ArtifactID)
			return string(body), e
		}
	}
	return "", NotFound
}

func (p RuntimeContextProvider) ContextRefPage(ctx context.Context, runID, refID, cursor string) (runner.ContextPage, error) {
	body, err := p.ContextRef(ctx, runID, refID)
	if err != nil {
		return runner.ContextPage{}, err
	}
	page, err := runner.PageContext(refID, body, cursor)
	if err != nil {
		return page, Fail(422, "invalid_cursor", "引用已变更或分页位置无效，请从第一页重新读取")
	}
	return page, nil
}
