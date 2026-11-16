package integration

import (
	"context"
	"encoding/json"
	"net/http"
)

type LarkIdentity struct {
	OpenID    string `json:"open_id"`
	Name      string `json:"name"`
	TenantKey string `json:"tenant_key"`
}

// LoginIdentity uses the short-lived user token only to identify its owner.
func (l *LarkClient) LoginIdentity(ctx context.Context, code, redirectURI, verifier string) (LarkIdentity, error) {
	invalid := &APIError{Provider: "lark", Operation: "login", Code: "invalid_identity"}
	if code == "" || len(code) > 4096 || redirectURI == "" || verifier == "" {
		return LarkIdentity{}, ErrInvalidInput
	}
	body, _, err := l.client.request(ctx, http.MethodPost, "/open-apis/authen/v2/oauth/token", nil, nil, map[string]string{
		"grant_type": "authorization_code", "client_id": l.config.AppID, "client_secret": l.config.AppSecret,
		"code": code, "redirect_uri": redirectURI, "code_verifier": verifier,
	})
	if err != nil {
		return LarkIdentity{}, invalid
	}
	var token struct {
		Code        int    `json:"code"`
		Error       string `json:"error"`
		AccessToken string `json:"access_token"`
	}
	if json.Unmarshal(body, &token) != nil || token.Code != 0 || token.Error != "" || token.AccessToken == "" {
		return LarkIdentity{}, invalid
	}
	body, _, err = l.client.request(ctx, http.MethodGet, "/open-apis/authen/v1/user_info", nil, http.Header{"Authorization": {"Bearer " + token.AccessToken}}, nil)
	if err != nil {
		return LarkIdentity{}, invalid
	}
	var result struct {
		Code *int         `json:"code"`
		Data LarkIdentity `json:"data"`
	}
	if json.Unmarshal(body, &result) != nil || result.Code == nil || *result.Code != 0 || result.Data.OpenID == "" || len(result.Data.OpenID) > 200 || len(result.Data.Name) > 800 {
		return LarkIdentity{}, invalid
	}
	return result.Data, nil
}
