package service

import (
	"context"
	"errors"
	"time"

	"agentowl/backend/internal/integration"
)

func sourceIdentity(ctx context.Context, q DB, source Data) (Data, error) {
	connector, err := one(ctx, q, "SELECT data FROM configs WHERE id::text=$1 AND kind='connectors'", Str(source, "connector_id"))
	if err != nil {
		return nil, err
	}
	identity := Data{"provider": Str(connector, "provider"), "app_id": Str(connector, "app_id"), "subject_id": Str(source, "actor_id"), "binding_status": "unbound"}
	a, _, err := resolvePlatformIdentity(ctx, q, Str(connector, "provider"), Str(connector, "app_id"), Str(source, "actor_id"))
	if err != nil && !errors.Is(err, NotFound) {
		return nil, err
	}
	if err == nil {
		identity["principal_id"] = a.ID
		identity["name"] = a.Name
		identity["binding_status"] = "bound"
	}
	return identity, nil
}

func caseRequestFacts(ctx context.Context, q DB, c Data) (Data, error) {
	facts := Data{"case_id": Str(c, "id"), "source": Str(c, "source"), "history_coverage": "locally_received"}
	m, err := one(ctx, q, "SELECT to_jsonb(m) FROM source_messages m WHERE case_id=$1 AND NOT deleted ORDER BY created_at,id LIMIT 1", Str(c, "id"))
	if err != nil && !errors.Is(err, NotFound) {
		return nil, err
	}
	if len(m) > 0 {
		identity, e := sourceIdentity(ctx, q, m)
		if e != nil {
			return nil, e
		}
		facts["requester"] = identity
		facts["conversation"] = Data{"connector_id": Str(m, "connector_id"), "channel_id": Str(m, "channel_id"), "thread_id": Str(m, "thread_id"), "message_id": Str(m, "external_id"), "private": Bool(Object(c, "environment"), "p2p_chat")}
		conversation := Object(facts, "conversation")
		if Bool(conversation, "private") {
			conversation["chat_id"] = Str(Object(c, "environment"), "external_channel_id")
		} else {
			ch, e := one(ctx, q, "SELECT jsonb_build_object('chat_id',data->>'external_id','name',name) FROM configs WHERE id::text=$1 AND kind='channels'", Str(m, "channel_id"))
			if e != nil && !errors.Is(e, NotFound) {
				return nil, e
			}
			conversation["chat_id"] = Str(ch, "chat_id")
			conversation["name"] = Str(ch, "name")
		}
		facts["conversation"] = conversation
		connector, e := one(ctx, q, "SELECT jsonb_build_object('name',name,'open_id',data->>'bot_open_id') FROM configs WHERE id=$1", Str(m, "connector_id"))
		if e != nil {
			return nil, e
		}
		facts["bot"] = connector
	} else {
		p, e := one(ctx, q, "SELECT jsonb_build_object('principal_id',id,'name',name) FROM principals WHERE id::text=$1", Str(c, "created_by"))
		if e != nil && !errors.Is(e, NotFound) {
			return nil, e
		}
		facts["requester"] = p
	}
	return facts, nil
}

// Source content and identity snapshots remain evidence, never execution authority.
func (s *Service) freezeContext(ctx context.Context, q DB, t, input Data) error {
	c, err := one(ctx, q, "SELECT to_jsonb(c) FROM cases c WHERE id=$1", Str(t, "case_id"))
	if err != nil {
		return err
	}
	facts := Data{"task_id": Str(t, "id"), "revision": Num(t, "revision"), "epoch": Num(t, "epoch"), "case_id": Str(c, "id"), "repository_id": Str(t, "repository_id"), "source": Str(c, "source"), "frozen_at": time.Now().UTC().Format(time.RFC3339)}
	sponsorID := Str(t, "created_by")
	if Str(t, "mandate_id") != "" {
		m, e := one(ctx, q, "SELECT to_jsonb(m) FROM task_mandates m WHERE id=$1", Str(t, "mandate_id"))
		if e != nil {
			return e
		}
		sponsorID = Str(m, "sponsor_principal_id")
		facts["authorization"] = Data{"mandate_id": Str(m, "id"), "issuer": Str(m, "issuer"), "scope_digest": Str(m, "scope_digest")}
	}
	p, err := one(ctx, q, "SELECT jsonb_build_object('principal_id',id,'name',name) FROM principals WHERE id::text=$1", sponsorID)
	if err != nil && !errors.Is(err, NotFound) {
		return err
	}
	facts["sponsor"] = p
	request, err := caseRequestFacts(ctx, q, c)
	if err != nil {
		return err
	}
	for key, value := range request {
		facts[key] = value
	}
	rows, err := many(ctx, q, `SELECT to_jsonb(i)||jsonb_build_object('applied',EXISTS(SELECT 1 FROM task_revision_inputs r WHERE r.input_id=i.id AND r.task_id=$1 AND r.revision=$2)) FROM task_inputs i WHERE i.task_id=$1 AND (i.disposition='context' OR EXISTS(SELECT 1 FROM task_revision_inputs r WHERE r.input_id=i.id AND r.task_id=$1 AND r.revision=$2)) ORDER BY i.seq`, Str(t, "id"), Num(t, "revision"))
	if err != nil {
		return err
	}
	index := []Data{}
	references := []Data{}
	sources := []Data{}
	sourceContexts := []Data{}
	for _, row := range rows {
		entry := Data{"id": Str(row, "id"), "kind": Str(row, "kind"), "digest": Str(row, "digest"), "actor": Object(row, "actor_ref"), "source_version": Num(row, "source_version")}
		if Str(row, "source_message_id") != "" {
			source, e := one(ctx, q, "SELECT to_jsonb(m) FROM source_messages m WHERE id=$1", Str(row, "source_message_id"))
			if e != nil {
				return e
			}
			if Bool(source, "deleted") || Num(source, "version") != Num(row, "source_version") {
				return Fail(409, "input_changed", "参考消息已更新，请重新确认任务信息")
			}
			sources = append(sources, Data{"id": Str(source, "id"), "version": Num(source, "version")})
			// Referenced conversations obey the sponsor's current channel access too.
			principal, e := one(ctx, q, "SELECT to_jsonb(p) FROM principals p WHERE id::text=$1 AND active", sponsorID)
			if e != nil {
				return e
			}
			if _, e = s.Get(ctx, q, actorFrom(principal), "cases", Str(source, "case_id")); e != nil {
				return e
			}
			identity, e := sourceIdentity(ctx, q, source)
			if e != nil {
				return e
			}
			entry["actor"] = identity
			entry["source"] = Data{"connector_id": Str(source, "connector_id"), "channel_id": Str(source, "channel_id"), "thread_id": Str(source, "thread_id"), "message_id": Str(source, "external_id"), "url": Str(source, "source_url")}
			contextFacts, contextDocs, e := s.sourceContextSnapshot(ctx, q, source)
			if e != nil {
				return e
			}
			contextFacts["task_input_id"] = Str(row, "id")
			sourceContexts = append(sourceContexts, Data{"input_id": Str(row, "id"), "facts": contextFacts, "documents": contextDocs})
		}
		if Bool(row, "applied") {
			entry["read_tool"] = "read_task_input"
			index = append(index, entry)
		} else {
			entry["body"] = Str(row, "body")
			references = append(references, entry)
		}
	}
	caseContent := Data{"title": Str(c, "title"), "module": Str(c, "module"), "version": Num(c, "version")}
	if len(Object(request, "conversation")) == 0 {
		caseContent["description"] = Str(c, "description")
	}
	documents := Data{
		"case":        string(JSON(caseContent)),
		"task_inputs": string(JSON(index)),
	}
	if len(references) > 0 {
		documents["reference_inputs"] = string(JSON(references))
	}
	if len(sourceContexts) > 0 {
		documents["source_contexts"] = string(JSON(sourceContexts))
	}
	results, err := many(ctx, q, `SELECT jsonb_build_object('stage',stage,'summary',output->>'summary') FROM task_steps WHERE task_id=$1 AND revision=$2 AND status='succeeded' ORDER BY created_at,id`, Str(t, "id"), Num(t, "revision"))
	if err != nil {
		return err
	}
	if len(results) > 0 {
		documents["stage_results"] = string(JSON(results))
	} else if summary := Str(input, "previous_summary"); summary != "" {
		documents["previous_summary"] = summary
	}
	for key := range documents {
		documents[key] = integration.Redact(Str(documents, key))
	}
	input["context_facts"] = facts
	input["context_sources"] = sources
	input["context_documents"] = documents
	input["context_manifest"] = decodeData(s.buildContextManifest(ctx, input))
	// The prompt gets an index; full case/history and previous reports stay behind tools.
	input["context"] = ""
	return nil
}
