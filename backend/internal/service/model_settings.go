package service

import (
	"context"
	"encoding/json"
	"math"
	"net/url"
	"strings"

	"agentowl/backend/internal/integration"
	"github.com/jackc/pgx/v5"
)

const defaultDailyTokens int64 = 1000000

var modelSettingFields = []string{"enabled", "base_url", "model_name", "allow_http", "json_mode", "max_tokens_field", "max_daily_tokens"}

func defaultModelSettings() Data {
	return Data{"enabled": false, "base_url": "", "model_name": "", "allow_http": false,
		"json_mode": "json_schema", "max_tokens_field": "max_tokens", "max_daily_tokens": defaultDailyTokens,
		"version": int64(0), "updated_at": nil, "updated_by": nil, "api_key_configured": false}
}

func (s *Service) readModelSettings(ctx context.Context, q DB) (Data, string, error) {
	raw, err := one(ctx, q, "SELECT to_jsonb(t) FROM system_settings t WHERE name='model'")
	result := defaultModelSettings()
	if err == NotFound {
		return result, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	for _, key := range modelSettingFields {
		if value, ok := Object(raw, "data")[key]; ok {
			result[key] = value
		}
	}
	for _, key := range []string{"version", "updated_at", "updated_by"} {
		result[key] = raw[key]
	}
	secret := Str(raw, "secret")
	result["api_key_configured"] = secret != ""
	return result, secret, nil
}

func (s *Service) GetModelSettings(ctx context.Context, a Actor) (Data, error) {
	if a.ID == "" {
		return nil, Forbidden
	}
	settings, _, err := s.readModelSettings(ctx, s.DB)
	return settings, err
}

func (s *Service) ModelSettingsVersion(ctx context.Context) (int64, error) {
	var version int64
	err := s.DB.QueryRow(ctx, "SELECT COALESCE((SELECT version FROM system_settings WHERE name='model'),0)").Scan(&version)
	return version, err
}

func settingInteger(value any) (int64, bool) {
	switch v := value.(type) {
	case int:
		return int64(v), v >= 0 && int64(v) <= 9007199254740991
	case int64:
		return v, v >= 0 && v <= 9007199254740991
	case float64:
		if v >= 0 && v <= 9007199254740991 && math.Trunc(v) == v {
			return int64(v), true
		}
	case json.Number:
		n, err := v.Int64()
		return n, err == nil && n >= 0 && n <= 9007199254740991
	}
	return 0, false
}

func validateModelSettings(data Data) error {
	if Bool(data, "enabled") && (Str(data, "base_url") == "" || Str(data, "model_name") == "") {
		return Fail(422, "validation", "启用模型前，请填写接口地址和模型名称")
	}
	if raw := Str(data, "base_url"); raw != "" {
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
			(u.Scheme != "https" && !(u.Scheme == "http" && Bool(data, "allow_http"))) {
			return Fail(422, "validation", "模型地址必须为无凭据、无查询参数的 HTTPS URL；使用 HTTP 时请明确开启允许 HTTP")
		}
	}
	if len(Str(data, "base_url")) > 2048 || len(Str(data, "model_name")) > 200 || strings.ContainsAny(Str(data, "model_name"), "\r\n\x00") {
		return Fail(422, "validation", "模型地址或名称过长，或包含无效字符")
	}
	if !contains([]string{"json_schema", "json_object"}, Str(data, "json_mode")) ||
		!contains([]string{"max_tokens", "max_completion_tokens"}, Str(data, "max_tokens_field")) {
		return Fail(422, "validation", "请选择支持的 JSON 模式和 Token 参数")
	}
	if n, ok := settingInteger(data["max_daily_tokens"]); !ok || n < 1 || n > 1000000000 {
		return Fail(422, "validation", "每日 Token 预算必须为 1 到 1000000000 之间的整数")
	}
	return nil
}

func (s *Service) SaveModelSettings(ctx context.Context, tx pgx.Tx, a Actor, b Data) (Data, error) {
	if !a.Has("admin") {
		return nil, Forbidden
	}
	expected, ok := settingInteger(b["expected_version"])
	if !ok {
		return nil, Fail(422, "validation", "请提供配置版本后重试")
	}
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended('system_settings:model',0))"); err != nil {
		return nil, err
	}
	old, encrypted, err := s.readModelSettings(ctx, tx)
	if err != nil {
		return nil, err
	}
	if Num(old, "version") != expected {
		return nil, Conflict
	}
	data := Data{}
	for _, key := range modelSettingFields {
		data[key] = old[key]
	}
	for key, value := range b {
		switch key {
		case "expected_version":
		case "enabled", "allow_http", "clear_api_key":
			if _, ok := value.(bool); !ok {
				return nil, Fail(422, "validation", key+" 必须为布尔值")
			}
			if key != "clear_api_key" {
				data[key] = value
			}
		case "base_url", "model_name", "json_mode", "max_tokens_field", "api_key":
			text, ok := value.(string)
			if !ok {
				return nil, Fail(422, "validation", key+" 必须为文本")
			}
			if key != "api_key" {
				data[key] = strings.TrimSpace(text)
			}
		case "max_daily_tokens":
			n, ok := settingInteger(value)
			if !ok {
				return nil, Fail(422, "validation", "每日 Token 预算必须为正整数")
			}
			data[key] = n
		default:
			return nil, Fail(422, "validation", "包含不支持的模型配置字段")
		}
	}
	data["base_url"] = strings.TrimRight(Str(data, "base_url"), "/")
	if err = validateModelSettings(data); err != nil {
		return nil, err
	}
	key := strings.TrimSpace(Str(b, "api_key"))
	clear := Bool(b, "clear_api_key")
	if len(key) > 8192 || strings.ContainsAny(key, "\r\n\x00") || clear && key != "" {
		return nil, Fail(422, "validation", "API Key 无效，或同时选择了更新和清除")
	}
	if Str(data, "base_url") != Str(old, "base_url") && encrypted != "" && key == "" && !clear {
		return nil, Fail(422, "key_destination_changed", "更换模型地址时，请重新填写 API Key 或明确清除旧 Key")
	}
	if clear {
		encrypted = ""
	} else if key != "" {
		encrypted, err = s.Vault.Seal(JSON(Data{"api_key": key}), []byte("system_settings:model"))
		if err != nil {
			return nil, err
		}
	}
	_, err = tx.Exec(ctx, "INSERT INTO system_settings(name,data,secret,updated_by) VALUES('model',$1,$2,$3) ON CONFLICT(name) DO UPDATE SET data=EXCLUDED.data,secret=EXCLUDED.secret,version=system_settings.version+1,updated_at=now(),updated_by=EXCLUDED.updated_by", JSON(data), encrypted, a.ID)
	if err != nil {
		return nil, err
	}
	result, _, err := s.readModelSettings(ctx, tx)
	if err != nil {
		return nil, err
	}
	if err = s.invalidateUnstartedSettingsApprovals(ctx, tx); err != nil {
		return nil, err
	}
	if err = audit(ctx, tx, a, "save", "system_settings", "model", string(JSON(Data{"settings": data, "version": result["version"], "api_key_changed": clear || key != ""}))); err != nil {
		return nil, err
	}
	return result, nil
}

// RunnerModelSettings is only exposed by the authenticated internal Runner API.
func (s *Service) RunnerModelSettings(ctx context.Context) (Data, error) {
	return s.runnerModelSettings(ctx, s.DB)
}

func (s *Service) runnerModelSettings(ctx context.Context, q DB) (Data, error) {
	settings, encrypted, err := s.readModelSettings(ctx, q)
	if err != nil || !Bool(settings, "enabled") {
		return settings, err
	}
	settings["api_key"] = ""
	if encrypted != "" {
		plain, err := s.Vault.Open(encrypted, []byte("system_settings:model"))
		if err != nil {
			return nil, err
		}
		var secret struct {
			APIKey string `json:"api_key"`
		}
		if err = json.Unmarshal(plain, &secret); err != nil {
			return nil, err
		}
		settings["api_key"] = secret.APIKey
	}
	return settings, nil
}

func (s *Service) CurrentModel(ctx context.Context) (*integration.ModelClient, error) {
	return s.currentModel(ctx, s.DB)
}

func (s *Service) currentModel(ctx context.Context, q DB) (*integration.ModelClient, error) {
	// Only tests inject Model; New never reads model configuration from the environment.
	if s.Model != nil {
		return s.Model, nil
	}
	settings, err := s.runnerModelSettings(ctx, q)
	if err != nil || !Bool(settings, "enabled") {
		return nil, err
	}
	if err = validateModelSettings(settings); err != nil {
		return nil, err
	}
	return integration.NewModelClient(integration.ModelConfig{BaseURL: Str(settings, "base_url"), APIKey: Str(settings, "api_key"),
		Model: Str(settings, "model_name"), AllowHTTP: Bool(settings, "allow_http"),
		JSONMode: Str(settings, "json_mode"), MaxTokensField: Str(settings, "max_tokens_field")})
}

func (s *Service) modelDailyTokenLimit(ctx context.Context, q DB) (int64, error) {
	settings, _, err := s.readModelSettings(ctx, q)
	if err != nil {
		return 0, err
	}
	limit, ok := settingInteger(settings["max_daily_tokens"])
	if !ok || limit < 1 || limit > 1000000000 {
		return 0, Fail(503, "invalid_model_settings", "模型预算配置无效，请联系管理员")
	}
	return limit, nil
}
