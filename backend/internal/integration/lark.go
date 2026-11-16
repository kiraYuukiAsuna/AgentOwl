package integration

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

type LarkConfig struct {
	BaseURL           string
	AppID             string
	TenantKey         string
	AppSecret         string
	VerificationToken string
	EncryptKey        string
	HTTPClient        HTTPDoer
	Timeout           time.Duration
	MaxResponseBytes  int64
	AllowHTTP         bool
	Clock             func() time.Time
	WebhookWindow     time.Duration
}

type LarkClient struct {
	client      client
	config      LarkConfig
	tokenMu     sync.Mutex
	token       string
	tokenUntil  time.Time
	tokenFlight chan struct{}
	tokenErr    error
}

func NewLarkClient(cfg LarkConfig) (*LarkClient, error) {
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://open.feishu.cn"
	}
	if cfg.AppID == "" {
		return nil, fmt.Errorf("%w: Lark app ID required", ErrInvalidInput)
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if cfg.WebhookWindow <= 0 {
		cfg.WebhookWindow = 5 * time.Minute
	}
	c, err := newClient("lark", cfg.BaseURL, cfg.HTTPClient, cfg.Timeout, cfg.MaxResponseBytes, cfg.AllowHTTP)
	if err != nil {
		return nil, err
	}
	return &LarkClient{client: c, config: cfg}, nil
}

type CardAction struct {
	ID            string          `json:"id"`
	AppID         string          `json:"app_id"`
	ActorID       string          `json:"actor_id"`
	ActorTenantID string          `json:"actor_tenant_id"`
	MessageID     string          `json:"message_id"`
	ChannelID     string          `json:"channel_id"`
	Name          string          `json:"name"`
	Value         json.RawMessage `json:"value"`
	FormValue     json.RawMessage `json:"form_value,omitempty"`
}

type LarkInbound struct {
	Challenge  string      `json:"challenge,omitempty"`
	Event      *Event      `json:"event,omitempty"`
	CardAction *CardAction `json:"card_action,omitempty"`
	Ignored    bool        `json:"ignored,omitempty"`
}

type larkEnvelope struct {
	Encrypt   string `json:"encrypt"`
	Type      string `json:"type"`
	Token     string `json:"token"`
	Challenge string `json:"challenge"`
	Schema    string `json:"schema"`
	Header    struct {
		EventID    string `json:"event_id"`
		EventType  string `json:"event_type"`
		AppID      string `json:"app_id"`
		TenantKey  string `json:"tenant_key"`
		Token      string `json:"token"`
		CreateTime string `json:"create_time"`
	} `json:"header"`
	Event json.RawMessage `json:"event"`
}

type cardActionCallback struct {
	OpenID        string `json:"open_id"`
	OpenMessageID string `json:"open_message_id"`
	OpenChatID    string `json:"open_chat_id"`
	TenantKey     string `json:"tenant_key"`
	Token         string `json:"token"`
	Challenge     string `json:"challenge"`
	Type          string `json:"type"`
	Action        struct {
		Name      string          `json:"name"`
		Value     json.RawMessage `json:"value"`
		FormValue json.RawMessage `json:"form_value"`
	} `json:"action"`
}

// ParseWebhook accepts v2 event callbacks. Business callbacks require EncryptKey,
// a fresh signed request, VerificationToken, and the configured app/tenant identity.
// Replay deduplication remains a durable caller responsibility, not an in-memory cache.
func (l *LarkClient) ParseWebhook(headers http.Header, body []byte) (LarkInbound, error) {
	if int64(len(body)) > l.client.maxBytes || len(body) == 0 {
		return LarkInbound{}, ErrInvalidWebhook
	}
	if _, err := decodeUniqueJSON(body); err != nil {
		return LarkInbound{}, ErrInvalidWebhook
	}
	rawBody := body
	var env larkEnvelope
	if json.Unmarshal(body, &env) != nil {
		return LarkInbound{}, ErrInvalidWebhook
	}
	if env.Encrypt != "" {
		plain, err := decryptLark(env.Encrypt, l.config.EncryptKey)
		if err != nil {
			return LarkInbound{}, ErrInvalidWebhook
		}
		if _, err = decodeUniqueJSON(plain); err != nil {
			return LarkInbound{}, ErrInvalidWebhook
		}
		body = plain
		env = larkEnvelope{}
		if json.Unmarshal(plain, &env) != nil {
			return LarkInbound{}, ErrInvalidWebhook
		}
	}
	if env.Type == "url_verification" {
		if !secureEqual(env.Token, l.config.VerificationToken) || env.Challenge == "" {
			return LarkInbound{}, ErrInvalidWebhook
		}
		return LarkInbound{Challenge: env.Challenge}, nil
	}
	if env.Schema == "2.0" || env.Header.EventType != "" {
		if err := l.verifySignature(headers, rawBody); err != nil {
			return LarkInbound{}, err
		}
		if env.Schema != "2.0" || !secureEqual(env.Header.Token, l.config.VerificationToken) || env.Header.AppID != l.config.AppID || env.Header.TenantKey != l.config.TenantKey {
			return LarkInbound{}, ErrInvalidWebhook
		}
		return l.parseEventEnvelope(env)
	}
	if err := l.verifyCardActionSignature(headers, rawBody); err != nil {
		return LarkInbound{}, err
	}
	var card cardActionCallback
	if json.Unmarshal(body, &card) != nil {
		return LarkInbound{}, ErrInvalidWebhook
	}
	return l.parseCardActionCallback(card, body)
}

func (l *LarkClient) ParseCardAction(headers http.Header, body []byte) (LarkInbound, error) {
	if int64(len(body)) > l.client.maxBytes || len(body) == 0 {
		return LarkInbound{}, ErrInvalidWebhook
	}
	if _, err := decodeUniqueJSON(body); err != nil {
		return LarkInbound{}, ErrInvalidWebhook
	}
	rawBody := body
	var encrypted struct {
		Encrypt string `json:"encrypt"`
	}
	if json.Unmarshal(body, &encrypted) != nil {
		return LarkInbound{}, ErrInvalidWebhook
	}
	if encrypted.Encrypt != "" {
		plain, err := decryptLark(encrypted.Encrypt, l.config.EncryptKey)
		if err != nil {
			return LarkInbound{}, ErrInvalidWebhook
		}
		if _, err = decodeUniqueJSON(plain); err != nil {
			return LarkInbound{}, ErrInvalidWebhook
		}
		body = plain
	}
	var env larkEnvelope
	if json.Unmarshal(body, &env) != nil {
		return LarkInbound{}, ErrInvalidWebhook
	}
	if env.Schema == "2.0" || env.Header.EventType != "" {
		if err := l.verifySignature(headers, rawBody); err != nil {
			return LarkInbound{}, err
		}
		if env.Schema != "2.0" || env.Header.EventType != "card.action.trigger" || !secureEqual(env.Header.Token, l.config.VerificationToken) || env.Header.AppID != l.config.AppID || env.Header.TenantKey != l.config.TenantKey {
			return LarkInbound{}, ErrInvalidWebhook
		}
		return l.parseEventEnvelope(env)
	}
	var card cardActionCallback
	if json.Unmarshal(body, &card) != nil {
		return LarkInbound{}, ErrInvalidWebhook
	}
	if card.Type == "url_verification" {
		if !secureEqual(card.Token, l.config.VerificationToken) || card.Challenge == "" {
			return LarkInbound{}, ErrInvalidWebhook
		}
		return LarkInbound{Challenge: card.Challenge}, nil
	}
	if err := l.verifyCardActionSignature(headers, rawBody); err != nil {
		return LarkInbound{}, err
	}
	return l.parseCardActionCallback(card, body)
}

// ParseLongConnectionEvent is only for events delivered by the authenticated SDK connection.
func (l *LarkClient) ParseLongConnectionEvent(body []byte) (LarkInbound, error) {
	if len(body) == 0 || int64(len(body)) > l.client.maxBytes {
		return LarkInbound{}, ErrInvalidWebhook
	}
	if _, err := decodeUniqueJSON(body); err != nil {
		return LarkInbound{}, ErrInvalidWebhook
	}
	var env larkEnvelope
	if json.Unmarshal(body, &env) != nil || env.Schema != "2.0" || env.Header.AppID != l.config.AppID || env.Header.TenantKey != l.config.TenantKey ||
		(env.Header.EventType != "im.message.receive_v1" && env.Header.EventType != "im.message.recalled_v1" && env.Header.EventType != "card.action.trigger") {
		return LarkInbound{}, ErrInvalidWebhook
	}
	return l.parseEventEnvelope(env)
}

func (l *LarkClient) parseEventEnvelope(env larkEnvelope) (LarkInbound, error) {
	if env.Header.EventType == "card.action.trigger" {
		var card struct {
			Operator struct {
				OpenID    string `json:"open_id"`
				TenantKey string `json:"tenant_key"`
			} `json:"operator"`
			Context struct {
				MessageID string `json:"open_message_id"`
				ChatID    string `json:"open_chat_id"`
			} `json:"context"`
			Action struct {
				Name      string          `json:"name"`
				Value     json.RawMessage `json:"value"`
				FormValue json.RawMessage `json:"form_value"`
			} `json:"action"`
		}
		if json.Unmarshal(env.Event, &card) != nil || card.Operator.OpenID == "" || card.Context.MessageID == "" || len(card.Action.Value) == 0 {
			return LarkInbound{}, ErrInvalidWebhook
		}
		value, err := decodeUniqueJSON(card.Action.Value)
		if err != nil {
			return LarkInbound{}, ErrInvalidWebhook
		}
		if _, ok := value.(map[string]any); !ok {
			return LarkInbound{}, ErrInvalidWebhook
		}
		id := env.Header.EventID
		if id == "" {
			digest := sha256.Sum256(env.Event)
			id = "card:" + hex.EncodeToString(digest[:])
		}
		return LarkInbound{CardAction: &CardAction{ID: id, AppID: env.Header.AppID, ActorID: card.Operator.OpenID, ActorTenantID: card.Operator.TenantKey, MessageID: card.Context.MessageID, ChannelID: card.Context.ChatID, Name: card.Action.Name, Value: card.Action.Value, FormValue: card.Action.FormValue}}, nil
	}
	if env.Header.EventID == "" {
		return LarkInbound{}, ErrInvalidWebhook
	}
	if env.Header.EventType == "im.message.receive_v1" {
		var payload struct {
			Sender struct {
				SenderID struct {
					OpenID string `json:"open_id"`
				} `json:"sender_id"`
				SenderType string `json:"sender_type"`
				TenantKey  string `json:"tenant_key"`
			} `json:"sender"`
			Message struct {
				MessageID   string `json:"message_id"`
				RootID      string `json:"root_id"`
				ParentID    string `json:"parent_id"`
				ThreadID    string `json:"thread_id"`
				CreateTime  string `json:"create_time"`
				UpdateTime  string `json:"update_time"`
				ChatID      string `json:"chat_id"`
				ChatType    string `json:"chat_type"`
				MessageType string `json:"message_type"`
				Content     string `json:"content"`
				Mentions    []struct {
					ID struct {
						OpenID string `json:"open_id"`
					} `json:"id"`
				} `json:"mentions"`
			} `json:"message"`
		}
		if json.Unmarshal(env.Event, &payload) != nil || payload.Message.MessageID == "" || payload.Message.ChatID == "" {
			return LarkInbound{}, ErrInvalidWebhook
		}
		msg := payload.Message
		if platformTime(msg.CreateTime).IsZero() || (payload.Sender.SenderType == "user" && payload.Sender.SenderID.OpenID == "") {
			return LarkInbound{}, ErrInvalidWebhook
		}
		text, attachments, err := larkContent(msg.MessageType, msg.Content)
		if err != nil {
			return LarkInbound{}, ErrInvalidWebhook
		}
		revision := msg.UpdateTime
		if revision == "" {
			revision = msg.CreateTime
		}
		mentions := []string{}
		for _, v := range msg.Mentions {
			if v.ID.OpenID != "" {
				mentions = append(mentions, v.ID.OpenID)
			}
		}
		return LarkInbound{Event: &Event{ID: env.Header.EventID, Kind: env.Header.EventType, AppID: env.Header.AppID, TenantID: env.Header.TenantKey, ChannelID: msg.ChatID, ChatType: msg.ChatType, MessageID: msg.MessageID, RootID: msg.RootID, ParentID: msg.ParentID, ThreadID: msg.ThreadID, ActorID: payload.Sender.SenderID.OpenID, ActorTenantID: payload.Sender.TenantKey, SourceRevision: revision, OccurredAt: platformTime(msg.CreateTime), MessageType: msg.MessageType, Text: Redact(text), Bot: payload.Sender.SenderType != "user", Attachments: attachments, MentionedUserIDs: mentions}}, nil
	}
	if env.Header.EventType == "im.message.recalled_v1" {
		var payload struct {
			MessageID  string `json:"message_id"`
			ChatID     string `json:"chat_id"`
			RecallTime string `json:"recall_time"`
		}
		if json.Unmarshal(env.Event, &payload) != nil || payload.MessageID == "" {
			return LarkInbound{}, ErrInvalidWebhook
		}
		if payload.RecallTime == "" {
			payload.RecallTime = env.Header.CreateTime
		}
		if platformTime(payload.RecallTime).IsZero() {
			return LarkInbound{}, ErrInvalidWebhook
		}
		return LarkInbound{Event: &Event{ID: env.Header.EventID, Kind: env.Header.EventType, AppID: env.Header.AppID, TenantID: env.Header.TenantKey, ChannelID: payload.ChatID, MessageID: payload.MessageID, SourceRevision: payload.RecallTime, OccurredAt: platformTime(payload.RecallTime), Deleted: true}}, nil
	}
	return LarkInbound{Ignored: true}, nil
}

func (l *LarkClient) verifySignature(h http.Header, body []byte) error {
	if l.config.EncryptKey == "" {
		return ErrInvalidWebhook
	}
	ts, nonce := h.Get("X-Lark-Request-Timestamp"), h.Get("X-Lark-Request-Nonce")
	seconds, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || nonce == "" || len(nonce) > 256 {
		return ErrInvalidWebhook
	}
	delta := l.config.Clock().Sub(time.Unix(seconds, 0))
	if delta > l.config.WebhookWindow || delta < -l.config.WebhookWindow {
		return ErrInvalidWebhook
	}
	expected := sha256.New()
	expected.Write([]byte(ts))
	expected.Write([]byte(nonce))
	expected.Write([]byte(l.config.EncryptKey))
	expected.Write(body)
	sig, err := hex.DecodeString(h.Get("X-Lark-Signature"))
	if err != nil || len(sig) != sha256.Size || subtle.ConstantTimeCompare(sig, expected.Sum(nil)) != 1 {
		return ErrInvalidWebhook
	}
	return nil
}

func (l *LarkClient) verifyCardActionSignature(h http.Header, body []byte) error {
	if l.config.VerificationToken == "" {
		return ErrInvalidWebhook
	}
	ts, nonce := h.Get("X-Lark-Request-Timestamp"), h.Get("X-Lark-Request-Nonce")
	seconds, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || nonce == "" || len(nonce) > 256 {
		return ErrInvalidWebhook
	}
	delta := l.config.Clock().Sub(time.Unix(seconds, 0))
	if delta > l.config.WebhookWindow || delta < -l.config.WebhookWindow {
		return ErrInvalidWebhook
	}
	expected := sha1.New()
	expected.Write([]byte(ts))
	expected.Write([]byte(nonce))
	expected.Write([]byte(l.config.VerificationToken))
	expected.Write(body)
	sig, err := hex.DecodeString(h.Get("X-Lark-Signature"))
	if err != nil || len(sig) != sha1.Size || subtle.ConstantTimeCompare(sig, expected.Sum(nil)) != 1 {
		return ErrInvalidWebhook
	}
	return nil
}

func (l *LarkClient) parseCardActionCallback(card cardActionCallback, body []byte) (LarkInbound, error) {
	if card.OpenID == "" || card.OpenMessageID == "" || len(card.Action.Value) == 0 {
		return LarkInbound{}, ErrInvalidWebhook
	}
	value, err := decodeUniqueJSON(card.Action.Value)
	if err != nil {
		return LarkInbound{}, ErrInvalidWebhook
	}
	if _, ok := value.(map[string]any); !ok {
		return LarkInbound{}, ErrInvalidWebhook
	}
	idDigest := sha256.Sum256(body)
	id := "card:" + hex.EncodeToString(idDigest[:])
	return LarkInbound{CardAction: &CardAction{ID: id, AppID: l.config.AppID, ActorID: card.OpenID, ActorTenantID: card.TenantKey, MessageID: card.OpenMessageID, ChannelID: card.OpenChatID, Name: card.Action.Name, Value: card.Action.Value, FormValue: card.Action.FormValue}}, nil
}

func secureEqual(a, b string) bool {
	return a != "" && b != "" && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func decryptLark(encoded, key string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(raw) < aes.BlockSize*2 || (len(raw)-aes.BlockSize)%aes.BlockSize != 0 {
		return nil, ErrInvalidWebhook
	}
	digest := sha256.Sum256([]byte(key))
	block, err := aes.NewCipher(digest[:])
	if err != nil {
		return nil, ErrInvalidWebhook
	}
	plain := make([]byte, len(raw)-aes.BlockSize)
	cipher.NewCBCDecrypter(block, raw[:aes.BlockSize]).CryptBlocks(plain, raw[aes.BlockSize:])
	pad := int(plain[len(plain)-1])
	if pad < 1 || pad > aes.BlockSize || pad > len(plain) {
		return nil, ErrInvalidWebhook
	}
	for _, b := range plain[len(plain)-pad:] {
		if int(b) != pad {
			return nil, ErrInvalidWebhook
		}
	}
	return plain[:len(plain)-pad], nil
}

func platformTime(s string) time.Time {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}
	}
	switch {
	case n >= 1e17:
		return time.Unix(0, n).UTC()
	case n >= 1e14:
		return time.UnixMicro(n).UTC()
	case n >= 1e11:
		return time.UnixMilli(n).UTC()
	default:
		return time.Unix(n, 0).UTC()
	}
}

func larkContent(kind, content string) (string, []Attachment, error) {
	if content == "" {
		return "", nil, nil
	}
	v, err := decodeUniqueJSON([]byte(content))
	if err != nil {
		return "", nil, err
	}
	var text []string
	var attachments []Attachment
	var walk func(any)
	walk = func(v any) {
		switch object := v.(type) {
		case map[string]any:
			if title, ok := object["title"].(string); ok {
				text = append(text, title)
			}
			if value, ok := object["text"].(string); ok {
				text = append(text, value)
			}
			if key, ok := object["image_key"].(string); ok {
				attachments = append(attachments, Attachment{Key: key, Type: "image"})
			}
			if key, ok := object["file_key"].(string); ok {
				name, _ := object["file_name"].(string)
				attachments = append(attachments, Attachment{Key: key, Type: "file", Name: name})
			}
			if child, ok := object["content"]; ok {
				walk(child)
			}
			// Post messages may wrap their content in a language key.
			for _, lang := range []string{"zh_cn", "en_us", "ja_jp"} {
				if child, ok := object[lang]; ok {
					walk(child)
					break
				}
			}
		case []any:
			for _, child := range object {
				walk(child)
			}
		}
	}
	walk(v)
	if len(text) == 0 && len(attachments) == 0 {
		return "[Unsupported message content: " + kind + "]", nil, nil
	}
	return strings.Join(text, "\n"), attachments, nil
}

func (l *LarkClient) tenantToken(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, l.client.timeout)
	defer cancel()
	l.tokenMu.Lock()
	if l.token != "" && l.config.Clock().Before(l.tokenUntil) {
		token := l.token
		l.tokenMu.Unlock()
		return token, nil
	}
	if flight := l.tokenFlight; flight != nil {
		l.tokenMu.Unlock()
		select {
		case <-ctx.Done():
			return "", &APIError{Provider: "lark", Operation: "token", Code: "timeout"}
		case <-flight:
			l.tokenMu.Lock()
			token, err := l.token, l.tokenErr
			l.tokenMu.Unlock()
			return token, err
		}
	}
	l.tokenFlight = make(chan struct{})
	l.tokenMu.Unlock()
	token, until, err := l.fetchTenantToken(ctx)
	l.tokenMu.Lock()
	l.token, l.tokenUntil, l.tokenErr = token, until, err
	close(l.tokenFlight)
	l.tokenFlight = nil
	l.tokenMu.Unlock()
	return token, err
}

func (l *LarkClient) fetchTenantToken(ctx context.Context) (string, time.Time, error) {
	if l.config.AppSecret == "" {
		return "", time.Time{}, fmt.Errorf("%w: Lark app secret required", ErrInvalidInput)
	}
	data, headers, err := l.client.request(ctx, http.MethodPost, "/open-apis/auth/v3/tenant_access_token/internal", nil, nil, map[string]string{"app_id": l.config.AppID, "app_secret": l.config.AppSecret})
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) {
			copyError := *apiErr
			copyError.Operation = "token"
			copyError.OutcomeUnknown = false
			err = &copyError
		}
		return "", time.Time{}, err
	}
	var response struct {
		Code   *int   `json:"code"`
		Token  string `json:"tenant_access_token"`
		Expire int64  `json:"expire"`
	}
	if json.Unmarshal(data, &response) != nil || response.Code == nil {
		return "", time.Time{}, &APIError{Provider: "lark", Operation: "token", Code: "invalid_response"}
	}
	if *response.Code != 0 {
		err := apiResponseError("lark", http.MethodPost, http.StatusOK, data, headers, "")
		err.Operation = "token"
		return "", time.Time{}, err
	}
	if response.Token == "" || response.Expire <= 0 || response.Expire > 86400 {
		return "", time.Time{}, &APIError{Provider: "lark", Operation: "token", Code: "invalid_response"}
	}
	margin := time.Duration(response.Expire) * time.Second / 10
	if margin > time.Minute {
		margin = time.Minute
	}
	return response.Token, l.config.Clock().Add(time.Duration(response.Expire)*time.Second - margin), nil
}

func (l *LarkClient) api(ctx context.Context, method, path string, q url.Values, body, out any) error {
	token, err := l.tenantToken(ctx)
	if err != nil {
		return err
	}
	data, headers, err := l.client.request(ctx, method, path, q, http.Header{"Authorization": {"Bearer " + token}}, body)
	if err != nil {
		return err
	}
	var envelope struct {
		Code *int            `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(data, &envelope) != nil || envelope.Code == nil {
		return &APIError{Provider: "lark", Operation: method, Code: "invalid_response", OutcomeUnknown: method != http.MethodGet}
	}
	if *envelope.Code != 0 {
		if *envelope.Code == 99991663 || *envelope.Code == 99991665 || *envelope.Code == 99991677 {
			l.tokenMu.Lock()
			if l.token == token {
				l.token = ""
				l.tokenErr = &APIError{Provider: "lark", Operation: "token", Code: "invalidated"}
			}
			l.tokenMu.Unlock()
		}
		upstream := apiResponseError("lark", method, http.StatusOK, data, headers, "Bearer "+token)
		upstream.OutcomeUnknown = method != http.MethodGet && *envelope.Code == 18121
		return upstream
	}
	if out != nil {
		return decodeResponse("lark", method, envelope.Data, out)
	}
	return nil
}

type Message struct {
	MessageID   string `json:"message_id"`
	ChatID      string `json:"chat_id"`
	RootID      string `json:"root_id"`
	ParentID    string `json:"parent_id"`
	ThreadID    string `json:"thread_id"`
	MessageType string `json:"msg_type"`
	CreateTime  string `json:"create_time"`
	UpdateTime  string `json:"update_time"`
	Deleted     bool   `json:"deleted"`
	Body        struct {
		Content string `json:"content"`
	} `json:"body"`
	Sender struct {
		ID         string `json:"id"`
		IDType     string `json:"id_type"`
		SenderType string `json:"sender_type"`
	} `json:"sender"`
}

func (m Message) Event() (Event, error) {
	text, attachments, err := larkContent(m.MessageType, m.Body.Content)
	if err != nil {
		return Event{}, err
	}
	revision := m.UpdateTime
	if revision == "" {
		revision = m.CreateTime
	}
	return Event{ID: "history:" + m.MessageID + ":" + revision, Kind: "im.message.history", ChannelID: m.ChatID, MessageID: m.MessageID, RootID: m.RootID, ParentID: m.ParentID, ThreadID: m.ThreadID, ActorID: m.Sender.ID, MessageType: m.MessageType, SourceRevision: revision, OccurredAt: platformTime(m.CreateTime), Text: Redact(text), Bot: m.Sender.SenderType != "user", Deleted: m.Deleted, Attachments: attachments}, nil
}

type HistoryRequest struct {
	ContainerType, ContainerID, PageToken string
	StartTime, EndTime                    time.Time
	PageSize                              int
}
type MessagePage struct {
	Items     []Message `json:"items"`
	HasMore   bool      `json:"has_more"`
	PageToken string    `json:"page_token"`
}

func (l *LarkClient) History(ctx context.Context, req HistoryRequest) (MessagePage, error) {
	if req.ContainerType == "" {
		req.ContainerType = "chat"
	}
	if req.ContainerID == "" || (req.ContainerType != "chat" && req.ContainerType != "thread") {
		return MessagePage{}, ErrInvalidInput
	}
	if req.ContainerType == "thread" && (!req.StartTime.IsZero() || !req.EndTime.IsZero()) {
		return MessagePage{}, fmt.Errorf("%w: thread history has no time range filter", ErrInvalidInput)
	}
	if req.PageSize == 0 {
		req.PageSize = 50
	}
	if req.PageSize < 1 || req.PageSize > 50 {
		return MessagePage{}, ErrInvalidInput
	}
	q := url.Values{"container_id_type": {req.ContainerType}, "container_id": {req.ContainerID}, "page_size": {strconv.Itoa(req.PageSize)}, "sort_type": {"ByCreateTimeAsc"}}
	if req.PageToken != "" {
		q.Set("page_token", req.PageToken)
	}
	if !req.StartTime.IsZero() {
		q.Set("start_time", strconv.FormatInt(req.StartTime.Unix(), 10))
	}
	if !req.EndTime.IsZero() {
		q.Set("end_time", strconv.FormatInt(req.EndTime.Unix(), 10))
	}
	var out MessagePage
	err := l.api(ctx, http.MethodGet, "/open-apis/im/v1/messages", q, nil, &out)
	if err == nil && out.HasMore && out.PageToken == "" {
		err = &APIError{Provider: "lark", Operation: "history", Code: "missing_cursor"}
	}
	return out, err
}

type SendMessageRequest struct {
	ReceiveIDType, ReceiveID, MessageType string
	Content                               json.RawMessage
	UUID, ReplyTo                         string
	ReplyInThread                         bool
}
type SentMessage struct {
	MessageID  string `json:"message_id"`
	ChatID     string `json:"chat_id"`
	RootID     string `json:"root_id"`
	ParentID   string `json:"parent_id"`
	ThreadID   string `json:"thread_id"`
	CreateTime string `json:"create_time"`
}

// SendMessage never retries. Caller persists UUID before calling and reconciles
// ambiguous outcomes; platform UUID deduplication is time bounded, not a permanent ledger.
func (l *LarkClient) SendMessage(ctx context.Context, req SendMessageRequest) (SentMessage, error) {
	if req.UUID == "" || len(req.UUID) > 50 || !json.Valid(req.Content) || req.MessageType == "" {
		return SentMessage{}, ErrInvalidInput
	}
	if req.ReceiveIDType == "" {
		req.ReceiveIDType = "chat_id"
	}
	if req.ReceiveIDType != "chat_id" && req.ReceiveIDType != "open_id" && req.ReceiveIDType != "user_id" && req.ReceiveIDType != "union_id" && req.ReceiveIDType != "email" {
		return SentMessage{}, ErrInvalidInput
	}
	payload := map[string]any{"msg_type": req.MessageType, "content": string(req.Content), "uuid": req.UUID}
	path := "/open-apis/im/v1/messages"
	q := url.Values{}
	if req.ReplyTo != "" {
		path += "/" + url.PathEscape(req.ReplyTo) + "/reply"
		payload["reply_in_thread"] = req.ReplyInThread
	} else {
		if req.ReceiveID == "" {
			return SentMessage{}, ErrInvalidInput
		}
		payload["receive_id"] = req.ReceiveID
		q.Set("receive_id_type", req.ReceiveIDType)
	}
	var out SentMessage
	err := l.api(ctx, http.MethodPost, path, q, payload, &out)
	if err == nil && out.MessageID == "" {
		err = &APIError{Provider: "lark", Operation: "send", Code: "missing_message_id", OutcomeUnknown: true}
	}
	return out, err
}

type AddReactionRequest struct {
	MessageID string
	EmojiType string
}

type AddedReaction struct {
	ReactionID string `json:"reaction_id"`
}

func (l *LarkClient) AddReaction(ctx context.Context, req AddReactionRequest) (AddedReaction, error) {
	if req.MessageID == "" || req.EmojiType == "" {
		return AddedReaction{}, ErrInvalidInput
	}
	payload := map[string]any{"reaction_type": map[string]any{"emoji_type": req.EmojiType}}
	var out AddedReaction
	err := l.api(ctx, http.MethodPost, "/open-apis/im/v1/messages/"+url.PathEscape(req.MessageID)+"/reactions", nil, payload, &out)
	if err == nil && out.ReactionID == "" {
		err = &APIError{Provider: "lark", Operation: "reaction", Code: "missing_reaction_id", OutcomeUnknown: true}
	}
	return out, err
}

func (l *LarkClient) GetMessage(ctx context.Context, id string) ([]Message, error) {
	if id == "" {
		return nil, ErrInvalidInput
	}
	var out struct {
		Items []Message `json:"items"`
	}
	err := l.api(ctx, http.MethodGet, "/open-apis/im/v1/messages/"+url.PathEscape(id), nil, nil, &out)
	return out.Items, err
}

// A UUID-only send cannot be reliably looked up by the supported message APIs.
// Keep an unknown operation pending rather than inferring failure from absence.
func (l *LarkClient) LookupMessage(ctx context.Context, messageID string) ([]Message, error) {
	if messageID == "" {
		return nil, ErrUnsupportedOperation
	}
	return l.GetMessage(ctx, messageID)
}

func (l *LarkClient) FetchResource(ctx context.Context, messageID, fileKey, kind string) ([]byte, error) {
	if messageID == "" || fileKey == "" || (kind != "image" && kind != "file") {
		return nil, ErrInvalidInput
	}
	token, err := l.tenantToken(ctx)
	if err != nil {
		return nil, err
	}
	data, headers, err := l.client.request(ctx, http.MethodGet, "/open-apis/im/v1/messages/"+url.PathEscape(messageID)+"/resources/"+url.PathEscape(fileKey), url.Values{"type": {kind}}, http.Header{"Authorization": {"Bearer " + token}}, nil)
	if err == nil && strings.Contains(headers.Get("Content-Type"), "application/json") && headers.Get("Content-Disposition") == "" {
		var envelope struct {
			Code *int   `json:"code"`
			Msg  string `json:"msg"`
		}
		if json.Unmarshal(data, &envelope) == nil && envelope.Code != nil && *envelope.Code != 0 && envelope.Msg != "" {
			upstream := apiResponseError("lark", http.MethodGet, http.StatusOK, data, headers, "Bearer "+token)
			upstream.Operation = "resource"
			return nil, upstream
		}
	}
	return data, err
}
