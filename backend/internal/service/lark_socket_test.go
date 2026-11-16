package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
)

func TestLarkSocketReceivesAndDeduplicates(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	var schema string
	if err := s.DB.QueryRow(ctx, "SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	dsn, err := url.Parse(os.Getenv("AGENTOWL_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	q := dsn.Query()
	q.Set("search_path", schema+",public")
	dsn.RawQuery = q.Encode()
	s.Config.DatabaseURL = dsn.String()
	connections := make(chan *websocket.Conn, 4)
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	var gateway *httptest.Server
	gateway = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/callback/ws/endpoint" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(larkws.EndpointResp{Code: 0, Data: &larkws.Endpoint{Url: "ws" + strings.TrimPrefix(gateway.URL, "http") + "/socket?service_id=1&device_id=test", ClientConfig: &larkws.ClientConfig{PingInterval: 3600, ReconnectInterval: 1}}})
			return
		}
		if r.URL.Path != "/socket" {
			w.WriteHeader(404)
			return
		}
		conn, e := up.Upgrade(w, r, nil)
		if e == nil {
			connections <- conn
		}
	}))
	defer gateway.Close()
	connector := supportConfig(t, s, "connectors", Data{"provider": "feishu", "app_id": "cli_test", "tenant_id": "tenant_test", "base_url": gateway.URL, "ws_enabled": true, "bot_open_id": "ou_bot"}, Data{"app_secret": "test-secret"})
	channel := supportConfig(t, s, "channels", Data{"connector_id": connector, "external_id": "oc_test", "enabled": true, "visibility": "internal", "reply_mode": "mentions"}, nil)
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); s.runLarkSocket(runCtx, connector) }()
	defer func() { cancel(); <-done }()
	var conn *websocket.Conn
	select {
	case conn = <-connections:
	case <-time.After(10 * time.Second):
		t.Fatal("socket not connected")
	}
	defer conn.Close()
	duplicateDone := make(chan struct{})
	go func() { defer close(duplicateDone); s.runLarkSocket(runCtx, connector) }()
	select {
	case <-duplicateDone:
	case <-time.After(5 * time.Second):
		t.Fatal("second worker failed to release competing lock")
	}
	select {
	case extra := <-connections:
		extra.Close()
		t.Fatal("two workers opened the same connector")
	default:
	}
	msg := supportMessage("om_test", "@_user_1 help", time.Now())
	Object(msg, "message")["mentions"] = []Data{{"id": Data{"open_id": "ou_bot"}}}
	body := JSON(Data{"schema": "2.0", "header": Data{"event_id": "event-socket-1", "event_type": "im.message.receive_v1", "app_id": "cli_test", "tenant_key": "tenant_test", "create_time": fmt.Sprint(time.Now().UnixMilli())}, "event": msg})
	send := func(payload []byte) int {
		t.Helper()
		f := larkws.Frame{SeqID: 1, LogID: 2, Service: 1, Method: int32(larkws.FrameTypeData), Headers: []larkws.Header{{Key: "type", Value: "event"}, {Key: "message_id", Value: "m1"}, {Key: "sum", Value: "1"}}, Payload: payload}
		encoded, e := f.Marshal()
		if e != nil {
			t.Fatal(e)
		}
		if e = conn.WriteMessage(websocket.BinaryMessage, encoded); e != nil {
			t.Fatal(e)
		}
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		var response larkws.Frame
		for {
			_, reply, e := conn.ReadMessage()
			if e != nil {
				t.Fatal(e)
			}
			if e = response.Unmarshal(reply); e != nil {
				t.Fatal(e)
			}
			if response.Method == int32(larkws.FrameTypeData) {
				break
			}
		}
		var result struct {
			Code int `json:"code"`
		}
		if err := json.Unmarshal(response.Payload, &result); err != nil {
			t.Fatal(err)
		}
		return result.Code
	}
	if code := send(body); code != 200 {
		t.Fatalf("first acknowledgment: %d", code)
	}
	if code := send(body); code != 200 {
		t.Fatalf("duplicate acknowledgment: %d", code)
	}
	if n := supportCount(t, s, `SELECT count(*) FROM ingest_events WHERE connector_id=$1`, connector); n != 1 {
		t.Fatalf("events=%d", n)
	}
	if n := supportCount(t, s, `SELECT count(*) FROM jobs WHERE kind='ingest_event'`); n != 1 {
		t.Fatalf("jobs=%d", n)
	}
	record, e := one(ctx, s.DB, `SELECT to_jsonb(e) FROM ingest_events e WHERE connector_id=$1`, connector)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.consumeSupportEvent(ctx, Str(record, "id")); e != nil {
		t.Fatal(e)
	}
	if n := supportCount(t, s, `SELECT count(*) FROM cases WHERE channel_id=$1 AND environment->>'mentioned_bot'='true'`, channel); n != 1 {
		t.Fatalf("mention not recorded: %d", n)
	}
	var bad Data
	if err := json.Unmarshal(body, &bad); err != nil {
		t.Fatal(err)
	}
	Object(bad, "header")["tenant_key"] = "wrong_tenant"
	if code := send(JSON(bad)); code != 500 {
		t.Fatalf("untrusted tenant acknowledged: %d", code)
	}
}

func TestLarkSocketRecallUpdatesCase(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	var schema string
	if err := s.DB.QueryRow(ctx, "SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	dsn, err := url.Parse(os.Getenv("AGENTOWL_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	q := dsn.Query()
	q.Set("search_path", schema+",public")
	dsn.RawQuery = q.Encode()
	s.Config.DatabaseURL = dsn.String()
	connections := make(chan *websocket.Conn, 1)
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	var gateway *httptest.Server
	gateway = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/callback/ws/endpoint" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(larkws.EndpointResp{Code: 0, Data: &larkws.Endpoint{Url: "ws" + strings.TrimPrefix(gateway.URL, "http") + "/socket?service_id=1&device_id=test", ClientConfig: &larkws.ClientConfig{PingInterval: 3600, ReconnectInterval: 1}}})
			return
		}
		if r.URL.Path != "/socket" {
			w.WriteHeader(404)
			return
		}
		conn, e := up.Upgrade(w, r, nil)
		if e == nil {
			connections <- conn
		}
	}))
	defer gateway.Close()
	connector := supportConfig(t, s, "connectors", Data{"provider": "feishu", "app_id": "cli_test", "tenant_id": "tenant_test", "base_url": gateway.URL, "ws_enabled": true, "bot_open_id": "ou_bot"}, Data{"app_secret": "test-secret"})
	supportConfig(t, s, "channels", Data{"connector_id": connector, "external_id": "oc_test", "enabled": true, "visibility": "internal", "reply_mode": "mentions"}, nil)
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); s.runLarkSocket(runCtx, connector) }()
	defer func() { cancel(); <-done }()
	var conn *websocket.Conn
	select {
	case conn = <-connections:
	case <-time.After(10 * time.Second):
		t.Fatal("socket not connected")
	}
	defer conn.Close()
	send := func(payload []byte) int {
		t.Helper()
		frame := larkws.Frame{SeqID: 1, LogID: 2, Service: 1, Method: int32(larkws.FrameTypeData), Headers: []larkws.Header{{Key: "type", Value: "event"}, {Key: "message_id", Value: "m1"}, {Key: "sum", Value: "1"}}, Payload: payload}
		encoded, e := frame.Marshal()
		if e != nil {
			t.Fatal(e)
		}
		if e = conn.WriteMessage(websocket.BinaryMessage, encoded); e != nil {
			t.Fatal(e)
		}
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		for {
			_, reply, e := conn.ReadMessage()
			if e != nil {
				t.Fatal(e)
			}
			var response larkws.Frame
			if e = response.Unmarshal(reply); e != nil {
				t.Fatal(e)
			}
			if response.Method != int32(larkws.FrameTypeData) {
				continue
			}
			var result struct {
				Code int `json:"code"`
			}
			if e = json.Unmarshal(response.Payload, &result); e != nil {
				t.Fatal(e)
			}
			return result.Code
		}
	}
	msg := supportMessage("om_recall", "auth failed", time.Now())
	receive := JSON(Data{"schema": "2.0", "header": Data{"event_id": "socket-msg", "event_type": "im.message.receive_v1", "app_id": "cli_test", "tenant_key": "tenant_test", "create_time": fmt.Sprint(time.Now().UnixMilli())}, "event": msg})
	if code := send(receive); code != 200 {
		t.Fatalf("receive acknowledgment: %d", code)
	}
	if n := supportCount(t, s, "SELECT count(*) FROM ingest_events WHERE connector_id=$1", connector); n != 1 {
		t.Fatal(n)
	}
	record, e := one(ctx, s.DB, "SELECT to_jsonb(e) FROM ingest_events e WHERE connector_id=$1 ORDER BY created_at DESC LIMIT 1", connector)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.consumeSupportEvent(ctx, Str(record, "id")); e != nil {
		t.Fatal(e)
	}
	var caseID string
	if e = s.DB.QueryRow(ctx, "SELECT case_id FROM source_messages WHERE external_id='om_recall'").Scan(&caseID); e != nil {
		t.Fatal(e)
	}
	if _, e = s.DB.Exec(ctx, "INSERT INTO replies(id,case_id,body,status,case_version) SELECT $1,id,'old answer','pending',version FROM cases WHERE id=$2", ID(), caseID); e != nil {
		t.Fatal(e)
	}
	recall := JSON(Data{"schema": "2.0", "header": Data{"event_id": "socket-recall", "event_type": "im.message.recalled_v1", "app_id": "cli_test", "tenant_key": "tenant_test", "create_time": fmt.Sprint(time.Now().UnixMilli())}, "event": Data{"message_id": "om_recall", "chat_id": "oc_test", "recall_time": fmt.Sprint(time.Now().UnixMilli())}})
	if code := send(recall); code != 200 {
		t.Fatalf("recall acknowledgment: %d", code)
	}
	if n := supportCount(t, s, "SELECT count(*) FROM replies WHERE case_id=$1 AND status='stale'", caseID); n != 1 {
		t.Fatal("socket recall did not invalidate reply before worker")
	}
	record, e = one(ctx, s.DB, "SELECT to_jsonb(e) FROM ingest_events e WHERE connector_id=$1 AND kind='im.message.recalled_v1' ORDER BY created_at DESC LIMIT 1", connector)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.consumeSupportEvent(ctx, Str(record, "id")); e != nil {
		t.Fatal(e)
	}
	if n := supportCount(t, s, "SELECT count(*) FROM source_messages WHERE external_id='om_recall' AND deleted AND content='' "); n != 1 {
		t.Fatal(n)
	}
}

func TestLarkSocketFeedbackCard(t *testing.T) {
	f := newFeedbackFixture(t, "feishu")
	op := f.send(t)
	s, ctx := f.s, context.Background()
	var schema string
	if err := s.DB.QueryRow(ctx, "SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	dsn, err := url.Parse(os.Getenv("AGENTOWL_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	q := dsn.Query()
	q.Set("search_path", schema+",public")
	dsn.RawQuery = q.Encode()
	s.Config.DatabaseURL = dsn.String()
	connections := make(chan *websocket.Conn, 1)
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	var gateway *httptest.Server
	gateway = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/callback/ws/endpoint" {
			_ = json.NewEncoder(w).Encode(larkws.EndpointResp{Code: 0, Data: &larkws.Endpoint{Url: "ws" + strings.TrimPrefix(gateway.URL, "http") + "/socket?service_id=1&device_id=test", ClientConfig: &larkws.ClientConfig{PingInterval: 3600, ReconnectInterval: 1}}})
			return
		}
		conn, err := up.Upgrade(w, r, nil)
		if err == nil {
			connections <- conn
		}
	}))
	defer gateway.Close()
	if _, err = s.DB.Exec(ctx, "UPDATE configs SET data=data||$2::jsonb WHERE id=$1", f.connector, JSON(Data{"base_url": gateway.URL, "ws_enabled": true})); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); s.runLarkSocket(runCtx, f.connector) }()
	defer func() { cancel(); <-done }()
	var conn *websocket.Conn
	select {
	case conn = <-connections:
	case <-time.After(10 * time.Second):
		t.Fatal("socket not connected")
	}
	defer conn.Close()
	send := func(body []byte) (int, Data) {
		t.Helper()
		frame := larkws.Frame{SeqID: 1, LogID: 2, Service: 1, Method: int32(larkws.FrameTypeData), Headers: []larkws.Header{{Key: "type", Value: "event"}, {Key: "message_id", Value: "card1"}, {Key: "sum", Value: "1"}}, Payload: body}
		encoded, err := frame.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		if err = conn.WriteMessage(websocket.BinaryMessage, encoded); err != nil {
			t.Fatal(err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		for {
			_, reply, err := conn.ReadMessage()
			if err != nil {
				t.Fatal(err)
			}
			var response larkws.Frame
			if err = response.Unmarshal(reply); err != nil {
				t.Fatal(err)
			}
			if response.Method != int32(larkws.FrameTypeData) {
				continue
			}
			var result struct {
				Code int    `json:"code"`
				Data []byte `json:"data"`
			}
			if err = json.Unmarshal(response.Payload, &result); err != nil {
				t.Fatal(err)
			}
			toast := Data{}
			if len(result.Data) > 0 {
				if err = json.Unmarshal(result.Data, &toast); err != nil {
					t.Fatal(err)
				}
			}
			return result.Code, toast
		}
	}
	card := feedbackCardEvent(op, "resolved")
	body := JSON(Data{"schema": "2.0", "header": Data{"event_id": "socket-feedback", "event_type": "card.action.trigger", "app_id": "cli_test", "tenant_key": "tenant_test"}, "event": card})
	for range 2 {
		code, response := send(body)
		if code != 200 || Str(Object(response, "toast"), "type") != "success" {
			t.Fatalf("callback acknowledgment: %d %#v", code, response)
		}
		card := Object(response, "card")
		if Str(card, "type") != "raw" || !strings.Contains(string(JSON(card["data"])), "按钮已停用") {
			t.Fatalf("card was not replaced: %#v", response)
		}
	}
	if n := supportCount(t, s, "SELECT count(*) FROM case_events WHERE case_id=$1 AND type='feedback'", Str(f.c, "id")); n != 1 {
		t.Fatalf("feedback applied %d times", n)
	}
	if n := supportCount(t, s, "SELECT count(*) FROM cases WHERE id=$1 AND status='resolved'", Str(f.c, "id")); n != 1 {
		t.Fatal("feedback did not resolve case")
	}
	Object(card, "operator")["open_id"] = "ou_unrelated"
	code, response := send(JSON(Data{"schema": "2.0", "header": Data{"event_id": "socket-forged", "event_type": "card.action.trigger", "app_id": "cli_test", "tenant_key": "tenant_test"}, "event": card}))
	if code != 200 || Str(Object(response, "toast"), "type") != "error" {
		t.Fatalf("unrelated participant: %d %#v", code, response)
	}
	if Str(Object(response, "card"), "type") != "" {
		t.Fatalf("error response unexpectedly replaced card: %#v", response)
	}
}
