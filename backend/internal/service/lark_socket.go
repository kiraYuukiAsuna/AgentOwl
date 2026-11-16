package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkcallback "github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
)

// SDK logs may contain endpoint URLs or event bodies; state is reported separately.
type socketLogger struct{}

func (socketLogger) Debug(context.Context, ...interface{}) {}
func (socketLogger) Info(context.Context, ...interface{})  {}
func (socketLogger) Warn(context.Context, ...interface{})  {}
func (socketLogger) Error(context.Context, ...interface{}) {}

func socketError(err error) string {
	var client *larkws.ClientError
	if errors.As(err, &client) {
		return fmt.Sprintf("飞书长连接失败（错误码 %d），请检查应用配置和事件订阅", client.Code)
	}
	var server *larkws.ServerError
	if errors.As(err, &server) {
		return fmt.Sprintf("飞书长连接失败（错误码 %d），请稍后重试", server.Code)
	}
	return "飞书长连接暂不可用，请检查网络和应用配置"
}

func (s *Service) updateSocketStatus(id string, version int64, status, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := s.DB.Exec(ctx, `UPDATE configs SET data=data||$3::jsonb WHERE id=$1 AND kind='connectors' AND version=$2 AND data->>'ws_enabled'='true'`, id, version, JSON(Data{"ws_status": status, "ws_error": reason, "ws_checked_at": time.Now().UTC().Format(time.RFC3339Nano)}))
	if err != nil {
		slog.Warn("长连接状态更新失败")
	}
}

// Each connector is held by one worker using a dedicated PostgreSQL session lock.
func (s *Service) runLarkSockets(ctx context.Context) {
	type running struct {
		cancel context.CancelFunc
		done   chan struct{}
	}
	active := map[string]running{}
	var wg sync.WaitGroup
	defer func() {
		for _, r := range active {
			r.cancel()
		}
		wg.Wait()
	}()
	scan := func() {
		rows, err := many(ctx, s.DB, `SELECT to_jsonb(c) FROM configs c WHERE kind='connectors' AND data->>'provider' IN ('feishu','lark') AND data->>'ws_enabled'='true' AND COALESCE(data->>'status','') NOT IN ('disabled','revoked')`)
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("长连接配置扫描失败")
			}
			return
		}
		wanted := map[string]bool{}
		for _, raw := range rows {
			c := configPublic(raw)
			id := Str(c, "id")
			wanted[id] = true
			if r, ok := active[id]; ok {
				select {
				case <-r.done:
					r.cancel()
					delete(active, id)
				default:
					continue
				}
			}
			child, cancel := context.WithCancel(ctx)
			r := running{cancel: cancel, done: make(chan struct{})}
			active[id] = r
			wg.Add(1)
			go func(id string, r running) { defer wg.Done(); defer close(r.done); s.runLarkSocket(child, id) }(id, r)
		}
		for id, r := range active {
			if !wanted[id] {
				r.cancel()
				delete(active, id)
			}
		}
	}
	scan()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			scan()
		}
	}
}

func (s *Service) runLarkSocket(ctx context.Context, id string) {
	lockCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := pgx.Connect(lockCtx, s.Config.DatabaseURL)
	if err != nil {
		return
	}
	defer conn.Close(context.Background())
	var acquired bool
	if err = conn.QueryRow(lockCtx, `SELECT pg_try_advisory_lock(hashtextextended($1,728690108))`, id).Scan(&acquired); err != nil || !acquired {
		return
	}
	c, secrets, err := s.config(ctx, s.DB, id, "connectors")
	if err != nil || connectorDisabled(c) || !Bool(c, "ws_enabled") {
		return
	}
	version := Num(c, "version")
	cli, _, err := s.supportLark(ctx, s.DB, id)
	if err != nil {
		s.updateSocketStatus(id, version, "error", "长连接配置无效，请检查应用 ID 和密钥")
		return
	}
	handler := dispatcher.NewEventDispatcher("", "").OnP2MessageReceiveV1(func(eventCtx context.Context, ev *larkim.P2MessageReceiveV1) error {
		if ev == nil || ev.EventReq == nil {
			return errors.New("invalid message event")
		}
		incoming, e := cli.ParseLongConnectionEvent(ev.EventReq.Body)
		if e != nil || incoming.Event == nil {
			return errors.New("invalid message event")
		}
		bounded, stop := context.WithTimeout(eventCtx, 2500*time.Millisecond)
		defer stop()
		_, e = s.ingestVerifiedEvent(bounded, Str(c, "provider"), id, c, secrets, incoming.Event, nil)
		if e != nil {
			return errors.New("event persistence failed")
		}
		return nil
	}).OnP2MessageRecalledV1(func(eventCtx context.Context, ev *larkim.P2MessageRecalledV1) error {
		if ev == nil || ev.EventReq == nil {
			return errors.New("invalid recall event")
		}
		incoming, e := cli.ParseLongConnectionEvent(ev.EventReq.Body)
		if e != nil || incoming.Event == nil {
			return errors.New("invalid recall event")
		}
		bounded, stop := context.WithTimeout(eventCtx, 2500*time.Millisecond)
		defer stop()
		_, e = s.ingestVerifiedEvent(bounded, Str(c, "provider"), id, c, secrets, incoming.Event, nil)
		if e != nil {
			return errors.New("recall persistence failed")
		}
		return nil
	})
	handler.OnP2CardActionTrigger(func(eventCtx context.Context, ev *larkcallback.CardActionTriggerEvent) (*larkcallback.CardActionTriggerResponse, error) {
		if ev == nil || ev.EventReq == nil {
			return nil, errors.New("invalid card callback")
		}
		incoming, e := cli.ParseLongConnectionEvent(ev.EventReq.Body)
		if e != nil || incoming.CardAction == nil {
			return nil, errors.New("invalid card callback")
		}
		bounded, stop := context.WithTimeout(eventCtx, 2500*time.Millisecond)
		defer stop()
		result, e := s.ingestVerifiedEvent(bounded, Str(c, "provider"), id, c, secrets, nil, incoming.CardAction)
		if e != nil {
			slog.Warn("飞书卡片操作未完成", "reason", safeError(e))
			message := "操作未完成，请稍后重试或联系管理员"
			var detail *Error
			if errors.As(e, &detail) && detail.Status == http.StatusForbidden {
				message = "无法记录操作，请确认你有权限且卡片仍有效"
			} else if errors.As(e, &detail) && detail.Status == http.StatusConflict {
				message = detail.Message
			}
			return &larkcallback.CardActionTriggerResponse{Toast: &larkcallback.Toast{Type: "error", Content: message}}, nil
		}
		toast := Object(result, "toast")
		card := Object(result, "card")
		response := &larkcallback.CardActionTriggerResponse{Toast: &larkcallback.Toast{Type: Str(toast, "type"), Content: Str(toast, "content")}}
		if Str(card, "type") != "" {
			response.Card = &larkcallback.Card{Type: Str(card, "type"), Data: card["data"]}
		}
		return response, nil
	})
	domain := Str(c, "base_url")
	if domain == "" {
		if Str(c, "provider") == "lark" {
			domain = "https://open.larksuite.com"
		} else {
			domain = "https://open.feishu.cn"
		}
	}
	var mu sync.Mutex
	status, reason := "connecting", ""
	set := func(st, why string) {
		mu.Lock()
		status, reason = st, why
		mu.Unlock()
		s.updateSocketStatus(id, version, st, why)
	}
	set(status, reason)
	client := larkws.NewClient(Str(c, "app_id"), Str(secrets, "app_secret"), larkws.WithDomain(domain), larkws.WithLogger(socketLogger{}), larkws.WithLogLevel(larkcore.LogLevelError), larkws.WithHttpClient(&http.Client{Timeout: 15 * time.Second}), larkws.WithEventHandler(handler), larkws.WithOnReady(func() { set("connected", "") }), larkws.WithOnReconnected(func() { set("connected", "") }), larkws.WithOnReconnecting(func() { set("connecting", "连接中断，正在重连") }), larkws.WithOnError(func(e error) { set("error", socketError(e)) }))
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- client.Start(runCtx) }()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			stop()
			<-done
			return
		case err := <-done:
			if ctx.Err() == nil {
				if err == nil {
					err = errors.New("connection closed")
				}
				set("error", socketError(err))
			}
			return
		case <-ticker.C:
			queryCtx, end := context.WithTimeout(ctx, 3*time.Second)
			var valid bool
			err = conn.QueryRow(queryCtx, `SELECT version=$2 AND data->>'ws_enabled'='true' AND COALESCE(data->>'status','') NOT IN ('disabled','revoked') FROM configs WHERE id=$1 AND kind='connectors'`, id, version).Scan(&valid)
			end()
			if err != nil || !valid {
				stop()
				<-done
				return
			}
			mu.Lock()
			st, why := status, reason
			mu.Unlock()
			s.updateSocketStatus(id, version, st, why)
		}
	}
}
