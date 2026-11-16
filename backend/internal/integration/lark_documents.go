package integration

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

type BotInfo struct {
	OpenID         string `json:"open_id"`
	AppName        string `json:"app_name"`
	ActivateStatus int    `json:"activate_status"`
}

func (l *LarkClient) BotInfo(ctx context.Context) (BotInfo, error) {
	var out struct {
		Code *int    `json:"code"`
		Bot  BotInfo `json:"bot"`
	}
	tok, e := l.tenantToken(ctx)
	if e != nil {
		return out.Bot, e
	}
	b, headers, e := l.client.request(ctx, http.MethodGet, "/open-apis/bot/v3/info", nil, http.Header{"Authorization": {"Bearer " + tok}}, nil)
	if e != nil {
		return out.Bot, e
	}
	if e = decodeResponse("lark", "bot_info", b, &out); e != nil {
		return out.Bot, e
	}
	if out.Code == nil {
		return out.Bot, &APIError{Provider: "lark", Operation: "bot_info", Code: "invalid_response"}
	}
	if *out.Code != 0 {
		upstream := apiResponseError("lark", http.MethodGet, http.StatusOK, b, headers, "Bearer "+tok)
		upstream.Operation = "bot_info"
		return out.Bot, upstream
	}
	if out.Bot.OpenID == "" || out.Bot.ActivateStatus != 2 {
		return out.Bot, &APIError{Provider: "lark", Operation: "bot_info", Code: "bot_inactive"}
	}
	return out.Bot, nil
}

type Document struct {
	ID         string `json:"document_id"`
	RevisionID int64  `json:"revision_id"`
	Title      string `json:"title"`
	Content    string `json:"content"`
}

// ReadDocument reads current docx plain text. Checking metadata twice prevents
// attaching an old revision number to content changed during the read.
func (l *LarkClient) ReadDocument(ctx context.Context, id string) (Document, error) {
	var before, after struct {
		Document Document `json:"document"`
	}
	var body struct {
		Content string `json:"content"`
	}
	if id == "" {
		return Document{}, ErrInvalidInput
	}
	p := "/open-apis/docx/v1/documents/" + url.PathEscape(id)
	if e := l.api(ctx, http.MethodGet, p, nil, nil, &before); e != nil {
		return Document{}, e
	}
	if before.Document.ID != id {
		return Document{}, &APIError{Provider: "lark", Operation: "document", Code: "invalid_identity"}
	}
	if e := l.api(ctx, http.MethodGet, p+"/raw_content", nil, nil, &body); e != nil {
		return Document{}, e
	}
	if e := l.api(ctx, http.MethodGet, p, nil, nil, &after); e != nil {
		return Document{}, e
	}
	if before.Document.RevisionID != after.Document.RevisionID || after.Document.ID != id {
		return Document{}, fmt.Errorf("%w: document changed while reading", ErrInvalidInput)
	}
	before.Document.Content = Redact(body.Content)
	return before.Document, nil
}

var documentToken = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// ResolveDocumentID follows a Wiki node using the configured app identity.
// The input URL is parsed only; requests always use the configured Lark API.
func (l *LarkClient) ResolveDocumentID(ctx context.Context, id, link string) (string, error) {
	if id != "" {
		if !documentToken.MatchString(id) {
			return "", &APIError{Provider: "lark", Code: "invalid_document_id", Message: "文档 ID 格式无效"}
		}
		return id, nil
	}
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return "", &APIError{Provider: "lark", Code: "invalid_document_link", Message: "请填写完整的 docx 或 Wiki 文档链接"}
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 2 || !documentToken.MatchString(parts[1]) {
		return "", &APIError{Provider: "lark", Code: "invalid_document_link", Message: "请填写完整的 docx 或 Wiki 文档链接"}
	}
	if parts[0] == "docx" {
		return parts[1], nil
	}
	if parts[0] != "wiki" {
		return "", &APIError{Provider: "lark", Code: "invalid_document_link", Message: "仅支持 docx 或 Wiki 文档链接"}
	}
	var out struct {
		Node struct {
			NodeToken   string `json:"node_token"`
			ObjectToken string `json:"obj_token"`
			ObjectType  string `json:"obj_type"`
		} `json:"node"`
	}
	if err = l.api(ctx, http.MethodGet, "/open-apis/wiki/v2/spaces/get_node", url.Values{"token": {parts[1]}, "obj_type": {"wiki"}}, nil, &out); err != nil {
		return "", err
	}
	if out.Node.NodeToken != parts[1] || !documentToken.MatchString(out.Node.ObjectToken) {
		return "", &APIError{Provider: "lark", Operation: "wiki_node", Code: "invalid_identity"}
	}
	if out.Node.ObjectType != "docx" {
		return "", &APIError{Provider: "lark", Code: "unsupported_document_type", Message: "该 Wiki 节点不是新版 docx 文档"}
	}
	return out.Node.ObjectToken, nil
}
