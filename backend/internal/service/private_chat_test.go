package service

import (
	"context"
	"testing"
	"time"
)

func TestP2PEventUsesVirtualChannelAndCase(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	connector := supportConfig(t, s, "connectors", Data{"provider": "feishu", "app_id": "cli_test", "tenant_id": "tenant_test", "status": "verified", "bot_open_id": "ou_bot"}, Data{"app_secret": "secret", "verification_token": "verify-test", "encrypt_key": "encryption-test"})
	event := supportMessage("om_p2p_one", "登录失败 p2p", time.Now())
	Object(event, "message")["chat_id"] = "oc_p2p_chat"
	Object(event, "message")["chat_type"] = "p2p"
	h, b := supportSigned(t, "p2p-event", "im.message.receive_v1", event)
	result, err := s.IngestWebhook(ctx, "feishu", connector, h, b)
	if err != nil || !Bool(result, "accepted") {
		t.Fatalf("ingest: %v %v", result, err)
	}
	if err = s.consumeSupportEvent(ctx, Str(result, "id")); err != nil {
		t.Fatal(err)
	}
	if n := supportCount(t, s, "SELECT count(*) FROM configs WHERE kind='channels' AND data->>'external_id'=$1", "oc_p2p_chat"); n != 0 {
		t.Fatalf("p2p created %d settings channels", n)
	}
	cases, err := many(ctx, s.DB, "SELECT to_jsonb(c) FROM cases c WHERE environment->>'external_channel_id'=$1 ORDER BY created_at DESC", "oc_p2p_chat")
	if err != nil || len(cases) != 1 {
		t.Fatalf("p2p case count: %v %v", len(cases), err)
	}
	if Str(cases[0], "channel_id") != "" || Str(cases[0], "reply_mode") != "auto" {
		t.Fatalf("p2p case should use virtual channel: %v", cases[0])
	}
	env := Object(cases[0], "environment")
	if !Bool(env, "p2p_chat") || Str(env, "last_message_id") != "om_p2p_one" || Str(env, "external_channel_id") != "oc_p2p_chat" || Str(env, "private_actor_id") != "ou_user" || Str(env, "private_connector_id") != connector {
		t.Fatalf("p2p env mismatch: %v", env)
	}
}

func TestP2PConsecutiveMessagesReuseChatIDOpenCase(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	connector := supportConfig(t, s, "connectors", Data{"provider": "feishu", "app_id": "cli_test", "tenant_id": "tenant_test", "status": "verified", "bot_open_id": "ou_bot"}, Data{"app_secret": "secret", "verification_token": "verify-test", "encrypt_key": "encryption-test"})
	send := func(eventID, messageID, body string) {
		t.Helper()
		event := supportMessage(messageID, body, time.Now())
		Object(event, "message")["chat_id"] = "oc_p2p_chat"
		Object(event, "message")["chat_type"] = "p2p"
		h, b := supportSigned(t, eventID, "im.message.receive_v1", event)
		result, err := s.IngestWebhook(ctx, "feishu", connector, h, b)
		if err != nil || !Bool(result, "accepted") {
			t.Fatalf("ingest: %v %v", result, err)
		}
		if err = s.consumeSupportEvent(ctx, Str(result, "id")); err != nil {
			t.Fatal(err)
		}
	}
	send("p2p-one", "om_p2p_one", "dpop有问题")
	send("p2p-two", "om_p2p_two", "你好")
	if n := supportCount(t, s, "SELECT count(*) FROM configs WHERE kind='channels' AND data->>'external_id'=$1", "oc_p2p_chat"); n != 0 {
		t.Fatalf("p2p created %d settings channels", n)
	}
	caseRow, err := one(ctx, s.DB, "SELECT to_jsonb(c) FROM cases c WHERE environment->>'external_channel_id'=$1", "oc_p2p_chat")
	if err != nil {
		t.Fatal(err)
	}
	if Str(caseRow, "channel_id") != "" || Str(caseRow, "description") != "dpop有问题\n\n你好" {
		t.Fatalf("p2p conversation not aggregated: %#v", caseRow)
	}
	if n := supportCount(t, s, "SELECT count(distinct case_id) FROM source_messages WHERE channel_id LIKE 'private:%'"); n != 1 {
		t.Fatalf("source messages linked to %d cases", n)
	}
}

func TestP2PStartsNewChatIDCaseAfterResolved(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	connector := supportConfig(t, s, "connectors", Data{"provider": "feishu", "app_id": "cli_test", "tenant_id": "tenant_test", "status": "verified", "bot_open_id": "ou_bot"}, Data{"app_secret": "secret", "verification_token": "verify-test", "encrypt_key": "encryption-test"})
	send := func(eventID, messageID, body string) {
		t.Helper()
		event := supportMessage(messageID, body, time.Now())
		Object(event, "message")["chat_id"] = "oc_p2p_chat"
		Object(event, "message")["chat_type"] = "p2p"
		h, b := supportSigned(t, eventID, "im.message.receive_v1", event)
		result, err := s.IngestWebhook(ctx, "feishu", connector, h, b)
		if err != nil || !Bool(result, "accepted") {
			t.Fatalf("ingest: %v %v", result, err)
		}
		if err = s.consumeSupportEvent(ctx, Str(result, "id")); err != nil {
			t.Fatal(err)
		}
	}
	send("p2p-one", "om_p2p_one", "first issue")
	if _, err := s.DB.Exec(ctx, "UPDATE cases SET status='resolved' WHERE id=(SELECT case_id FROM source_messages WHERE external_id='om_p2p_one')"); err != nil {
		t.Fatal(err)
	}
	send("p2p-two", "om_p2p_two", "new issue")
	if n := supportCount(t, s, "SELECT count(*) FROM configs WHERE kind='channels' AND data->>'external_id'=$1", "oc_p2p_chat"); n != 0 {
		t.Fatalf("p2p created %d settings channels", n)
	}
	if n := supportCount(t, s, "SELECT count(*) FROM cases WHERE environment->>'external_channel_id'=$1", "oc_p2p_chat"); n != 2 {
		t.Fatalf("resolved p2p case was reused; cases=%d", n)
	}
}

func TestP2PManualCaseFollowupQueuesOncallNotification(t *testing.T) {
	s, a := testService(t)
	ctx := context.Background()
	connector, notifyChannel := supportLarkFixture(t, s)
	supportConfig(t, s, "owner-routes", Data{"module": "*", "primary_id": a.ID, "channel_id": notifyChannel, "enabled": true}, nil)
	send := func(eventID, messageID, body string) {
		t.Helper()
		event := supportMessage(messageID, body, time.Now())
		Object(event, "message")["chat_id"] = "oc_p2p_manual"
		Object(event, "message")["chat_type"] = "p2p"
		h, b := supportSigned(t, eventID, "im.message.receive_v1", event)
		result, err := s.IngestWebhook(ctx, "feishu", connector, h, b)
		if err != nil || !Bool(result, "accepted") {
			t.Fatalf("ingest: %v %v", result, err)
		}
		if err = s.consumeSupportEvent(ctx, Str(result, "id")); err != nil {
			t.Fatal(err)
		}
	}
	send("p2p-manual-one", "om_p2p_manual_one", "first issue")
	c, err := one(ctx, s.DB, "SELECT to_jsonb(c) FROM cases c WHERE environment->>'external_channel_id'=$1", "oc_p2p_manual")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(ctx, "UPDATE cases SET reply_mode='manual',assignee_id=$2,version=version+1,updated_at=now() WHERE id=$1", Str(c, "id"), a.ID); err != nil {
		t.Fatal(err)
	}
	send("p2p-manual-two", "om_p2p_manual_two", "still broken")
	job, err := one(ctx, s.DB, "SELECT to_jsonb(j) FROM jobs j WHERE kind='notify' AND payload->>'case_id'=$1 AND payload->>'stage'='manual_update'", Str(c, "id"))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ProcessSupportJob(ctx, "notify", Object(job, "payload")); err != nil {
		t.Fatal(err)
	}
	if n := supportCount(t, s, "SELECT count(*) FROM external_operations WHERE kind='notify' AND subject_id=$1 AND request->>'body' LIKE '%人工接管问题收到新消息%' AND request->>'body' LIKE '%still broken%'", Str(c, "id")); n != 1 {
		t.Fatalf("manual followup notification count=%d", n)
	}
	if n := supportCount(t, s, "SELECT count(*) FROM case_events WHERE case_id=$1 AND type='notification_queued' AND metadata->>'stage'='manual_update'", Str(c, "id")); n != 1 {
		t.Fatalf("manual followup event count=%d", n)
	}
}
