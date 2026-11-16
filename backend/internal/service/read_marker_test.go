package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestIngestWebhookQueuesReadMarkerForLiveLarkMessages(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	connector, channel := supportLarkFixture(t, s)
	h, b := supportSigned(t, "event-one", "im.message.receive_v1", supportMessage("om_one", "auth failed", time.Now()))
	result, err := s.IngestWebhook(ctx, "feishu", connector, h, b)
	if err != nil || !Bool(result, "accepted") {
		t.Fatalf("ingest: %v %v", result, err)
	}
	var n int
	if err = s.DB.QueryRow(ctx, "SELECT count(*) FROM external_operations WHERE kind='read_mark'").Scan(&n); err != nil || n != 1 {
		t.Fatalf("read mark op count=%d err=%v", n, err)
	}
	if err = s.consumeSupportEvent(ctx, Str(result, "id")); err != nil {
		t.Fatal(err)
	}
	if n = supportCount(t, s, "SELECT count(*) FROM source_messages WHERE channel_id=$1", channel); n != 1 {
		t.Fatal(n)
	}
}

func TestIngestWebhookQueuesReadMarkerForPrivateLarkMessages(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	connector := supportConfig(t, s, "connectors", Data{"provider": "feishu", "app_id": "cli_test", "tenant_id": "tenant_test", "status": "verified", "bot_open_id": "ou_bot"}, Data{"app_secret": "secret", "verification_token": "verify-test", "encrypt_key": "encryption-test"})
	var tokenCalls, reactionCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			tokenCalls++
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "tenant_access_token": "token", "expire": 7200})
		case "/open-apis/im/v1/messages/om_private/reactions":
			reactionCalls++
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"reaction_id": "reaction-1"}})
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	if _, err := s.DB.Exec(ctx, "UPDATE configs SET data=data||$2::jsonb WHERE id=$1", connector, JSON(Data{"base_url": server.URL})); err != nil {
		t.Fatal(err)
	}
	event := supportMessage("om_private", "auth failed", time.Now())
	Object(event, "message")["chat_id"] = "oc_p2p_chat"
	Object(event, "message")["chat_type"] = "p2p"
	h, b := supportSigned(t, "private-readmark", "im.message.receive_v1", event)
	result, err := s.IngestWebhook(ctx, "feishu", connector, h, b)
	if err != nil || !Bool(result, "accepted") {
		t.Fatalf("ingest: %v %v", result, err)
	}
	op, err := one(ctx, s.DB, "SELECT to_jsonb(o) FROM external_operations o WHERE kind='read_mark'")
	if err != nil {
		t.Fatal(err)
	}
	target := Object(op, "target")
	if !Bool(target, "private_channel") || Str(target, "channel_id") != "private:"+connector+":ou_user" || Str(target, "external_id") != "oc_p2p_chat" || Str(target, "private_actor_id") != "ou_user" {
		t.Fatalf("private read mark target mismatch: %#v", target)
	}
	dispatched, err := s.DispatchMessage(ctx, op)
	if err != nil || Str(dispatched, "remote_id") != "reaction-1" || reactionCalls != 1 || tokenCalls != 1 {
		t.Fatalf("dispatch private read mark: %v %v token=%d reaction=%d", dispatched, err, tokenCalls, reactionCalls)
	}
	if n := supportCount(t, s, "SELECT count(*) FROM configs WHERE kind='channels' AND data->>'external_id'=$1", "oc_p2p_chat"); n != 0 {
		t.Fatalf("private read mark created %d settings channels", n)
	}
	if err = s.consumeSupportEvent(ctx, Str(result, "id")); err != nil {
		t.Fatal(err)
	}
	if n := supportCount(t, s, "SELECT count(*) FROM source_messages WHERE channel_id=$1", Str(target, "channel_id")); n != 1 {
		t.Fatal(n)
	}
}

func TestDispatchMessageReadMarkCallsReactionAPI(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	connector, channel := supportLarkFixture(t, s)
	var tokenCalls, reactionCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			tokenCalls++
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "tenant_access_token": "token", "expire": 7200})
		case "/open-apis/im/v1/messages/om_mark/reactions":
			reactionCalls++
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"reaction_id": "reaction-1"}})
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	if _, err := s.DB.Exec(ctx, "UPDATE configs SET data=data||$2::jsonb WHERE id=$1", connector, JSON(Data{"base_url": server.URL})); err != nil {
		t.Fatal(err)
	}
	op := Data{"id": ID(), "kind": "read_mark", "target": Data{"channel_id": channel, "channel_version": 1, "connector_id": connector, "connector_version": 1}, "request": Data{"message_id": "om_mark", "emoji_type": "Typing", "connector_id": connector, "connector_version": 1}}
	result, err := s.DispatchMessage(ctx, op)
	if err != nil || Str(result, "remote_id") != "reaction-1" || reactionCalls != 1 || tokenCalls != 1 {
		t.Fatalf("dispatch read mark: %v %v token=%d reaction=%d", result, err, tokenCalls, reactionCalls)
	}
}

func TestDispatchMessageReadMarkIgnoresEmptyBodyValidation(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	connector, channel := supportLarkFixture(t, s)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "tenant_access_token": "token", "expire": 7200})
		case "/open-apis/im/v1/messages/om_mark/reactions":
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"reaction_id": "reaction-1"}})
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	if _, err := s.DB.Exec(ctx, "UPDATE configs SET data=data||$2::jsonb WHERE id=$1", connector, JSON(Data{"base_url": server.URL})); err != nil {
		t.Fatal(err)
	}
	op := Data{"id": ID(), "kind": "read_mark", "target": Data{"channel_id": channel, "channel_version": 1, "connector_id": connector, "connector_version": 1}, "request": Data{"message_id": "om_mark", "emoji_type": "Typing", "connector_id": connector, "connector_version": 1}}
	if _, err := s.DispatchMessage(ctx, op); err != nil {
		t.Fatalf("empty-body read mark rejected: %v", err)
	}
}
