package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
)

var (
	ErrInvalidWebhook       = errors.New("invalid webhook")
	ErrUnsupportedEvent     = errors.New("unsupported event")
	ErrUnsupportedOperation = errors.New("operation is not supported")
	ErrResponseTooLarge     = errors.New("upstream response exceeds limit")
	ErrInvalidModelOutput   = errors.New("invalid model output")
	ErrInvalidInput         = errors.New("invalid integration input")
)

type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// APIError keeps only bounded diagnostic fields, never the response body or credentials.
type APIError struct {
	Provider       string
	Operation      string
	StatusCode     int
	Code           string
	Message        string
	RequestID      string
	OutcomeUnknown bool
	RetryAfter     time.Duration
}

func (e *APIError) Error() string {
	return e.Diagnostic()
}

// Diagnostic formats the safe, bounded subset suitable for users and job records.
func (e *APIError) Diagnostic() string {
	provider := map[string]string{"lark": "飞书 / Lark", "github": "GitHub", "model": "模型接口"}[e.Provider]
	if provider == "" {
		provider = "上游服务"
	}
	parts := []string{}
	if e.StatusCode > 0 {
		parts = append(parts, fmt.Sprintf("HTTP %d", e.StatusCode))
	}
	if e.Code != "" {
		parts = append(parts, "错误码 "+e.Code)
	}
	if e.Message != "" {
		parts = append(parts, e.Message)
	}
	if e.RequestID != "" {
		parts = append(parts, "请求 ID "+e.RequestID)
	}
	if len(parts) == 0 {
		return provider + "请求失败"
	}
	return provider + "请求失败（" + strings.Join(parts, "；") + "）"
}

var diagnosticID = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)
var diagnosticURL = regexp.MustCompile(`(?i)https?://[^\s]+`)

// Never persist raw upstream bodies. On write requests, a provider may echo
// submitted content; only known fixed messages are safe to expose.
func apiResponseError(provider, operation string, status int, data []byte, headers http.Header, authorization string) *APIError {
	e := &APIError{Provider: provider, Operation: operation, StatusCode: status, Code: "http_error"}
	token := strings.TrimSpace(strings.TrimPrefix(authorization, "Bearer "))
	safeID := func(value string) bool {
		return diagnosticID.MatchString(value) && (token == "" || !strings.Contains(value, token))
	}
	var envelope struct {
		Code    json.RawMessage `json:"code"`
		Msg     string          `json:"msg"`
		Message string          `json:"message"`
		Error   struct {
			Code    json.RawMessage `json:"code"`
			Message string          `json:"message"`
			LogID   string          `json:"log_id"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &envelope) == nil {
		raw := envelope.Code
		if len(raw) == 0 || string(raw) == "null" {
			raw = envelope.Error.Code
		}
		var code string
		if len(raw) > 0 {
			if json.Unmarshal(raw, &code) != nil {
				code = string(raw)
			}
		}
		if safeID(code) && code != "0" {
			e.Code = code
		}
		msg := envelope.Msg
		if msg == "" {
			msg = envelope.Message
		}
		if msg == "" {
			msg = envelope.Error.Message
		}
		fixed := map[string]bool{"Bot/User can NOT be out of the chat.": true, "The bot can not be outside the group.": true, "Bad credentials": true, "Not Found": true, "Resource not accessible by integration": true, "Resource not accessible by personal access token": true}
		if (operation == http.MethodGet && (provider == "lark" || provider == "github")) || fixed[msg] {
			msg = Redact(msg, token, authorization)
			msg = diagnosticURL.ReplaceAllString(msg, "[链接已隐藏]")
			msg = strings.Map(func(r rune) rune {
				if unicode.IsControl(r) {
					return ' '
				}
				return r
			}, msg)
			msg = strings.TrimSpace(msg)
			chars := []rune(msg)
			if len(chars) > 300 {
				msg = string(chars[:300]) + "…"
			}
			e.Message = msg
		}
		if safeID(envelope.Error.LogID) {
			e.RequestID = envelope.Error.LogID
		}
	}
	if provider == "github" && status == http.StatusForbidden {
		switch headers.Get("X-Accepted-GitHub-Permissions") {
		case "contents=write":
			e.Message = strings.TrimSpace(e.Message + "；需要 GitHub 仓库 Contents 写入权限")
		case "pull_requests=write":
			e.Message = strings.TrimSpace(e.Message + "；需要 GitHub 仓库 Pull requests 写入权限")
		}
	}
	if e.RequestID == "" {
		for _, key := range []string{"X-Tt-Logid", "X-Github-Request-Id", "X-Request-Id"} {
			v := headers.Get(key)
			if safeID(v) {
				e.RequestID = v
				break
			}
		}
	}
	return e
}

func (e *APIError) Unwrap() error {
	if e.Code == "response_too_large" {
		return ErrResponseTooLarge
	}
	return nil
}

type client struct {
	base                 *url.URL
	http                 HTTPDoer
	timeout              time.Duration
	maxBytes             int64
	provider             string
	unlimitedRequestBody bool
}

func newClient(provider, rawURL string, doer HTTPDoer, timeout time.Duration, maxBytes int64, allowHTTP bool) (client, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return client{}, fmt.Errorf("%w: invalid base URL", ErrInvalidInput)
	}
	ip := net.ParseIP(u.Hostname())
	loopback := u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback())
	if u.Scheme != "https" && !(u.Scheme == "http" && (allowHTTP || loopback)) {
		return client{}, fmt.Errorf("%w: HTTPS required", ErrInvalidInput)
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if maxBytes <= 0 {
		maxBytes = 4 << 20
	}
	if doer == nil {
		doer = &http.Client{}
	}
	if hc, ok := doer.(*http.Client); ok {
		copyClient := *hc
		copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		doer = &copyClient
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return client{base: u, http: doer, timeout: timeout, maxBytes: maxBytes, provider: provider}, nil
}

func (c client) request(ctx context.Context, method, endpoint string, query url.Values, headers http.Header, body any) ([]byte, http.Header, error) {
	var payload []byte
	var err error
	if body != nil {
		payload, err = json.Marshal(body)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: cannot encode request", ErrInvalidInput)
		}
	}
	if !c.unlimitedRequestBody && int64(len(payload)) > c.maxBytes {
		return nil, nil, fmt.Errorf("%w: request exceeds limit", ErrInvalidInput)
	}
	u := *c.base
	// endpoint is constructed by adapters; RawPath preserves escaped branch names.
	pathURL, err := url.Parse(c.base.EscapedPath() + endpoint)
	if err != nil || pathURL.IsAbs() || pathURL.RawQuery != "" || pathURL.Fragment != "" {
		return nil, nil, ErrInvalidInput
	}
	u.Path, u.RawPath, u.RawQuery = pathURL.Path, pathURL.RawPath, query.Encode()
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(payload))
	if err != nil {
		return nil, nil, ErrInvalidInput
	}
	req.Header = headers.Clone()
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	write := method != http.MethodGet && method != http.MethodHead
	resp, err := c.http.Do(req)
	if err != nil {
		reason := "网络连接失败，请检查服务地址和网络"
		if errors.Is(err, context.DeadlineExceeded) {
			reason = "请求超时"
		} else if errors.Is(err, context.Canceled) {
			reason = "请求已取消"
		} else if errors.Is(err, syscall.ECONNREFUSED) {
			reason = "连接被拒绝，请检查服务地址和监听状态"
		} else if dns := new(net.DNSError); errors.As(err, &dns) {
			reason = "域名解析失败，请检查服务地址和 DNS"
		}
		return nil, nil, &APIError{Provider: c.provider, Operation: method, Code: "transport", Message: reason, OutcomeUnknown: write}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, c.maxBytes+1))
	if err != nil {
		return nil, resp.Header, &APIError{Provider: c.provider, Operation: method, StatusCode: resp.StatusCode, Code: "read_response", OutcomeUnknown: write}
	}
	if int64(len(data)) > c.maxBytes {
		return nil, resp.Header, &APIError{Provider: c.provider, Operation: method, StatusCode: resp.StatusCode, Code: "response_too_large", OutcomeUnknown: write}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		retryAfter := time.Duration(0)
		if secs, parseErr := strconv.Atoi(resp.Header.Get("Retry-After")); parseErr == nil && secs > 0 {
			retryAfter = time.Duration(secs) * time.Second
		}
		apiErr := apiResponseError(c.provider, method, resp.StatusCode, data, resp.Header, req.Header.Get("Authorization"))
		apiErr.OutcomeUnknown = write && (resp.StatusCode >= 500 || resp.StatusCode == 408)
		apiErr.RetryAfter = retryAfter
		return nil, resp.Header, apiErr
	}
	return data, resp.Header, nil
}

func decodeResponse(provider, method string, data []byte, out any) error {
	value, parseErr := decodeUniqueJSON(data)
	if err := json.Unmarshal(data, out); err != nil || parseErr != nil || value == nil {
		return &APIError{Provider: provider, Operation: method, Code: "invalid_response", OutcomeUnknown: method != http.MethodGet}
	}
	return nil
}

func strictDecode(data []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return ErrInvalidModelOutput
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return ErrInvalidModelOutput
	}
	return nil
}
