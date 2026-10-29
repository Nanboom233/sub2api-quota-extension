// sub2api-package: backend/internal/handler/admin
package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type quotaScriptAdminFixture struct {
	service.AdminService
	account *service.Account
	proxies []int64
	writes  int
}

func (f *quotaScriptAdminFixture) GetAccount(context.Context, int64) (*service.Account, error) {
	return f.account, nil
}

func (f *quotaScriptAdminFixture) GetProxy(_ context.Context, id int64) (*service.Proxy, error) {
	f.proxies = append(f.proxies, id)
	return &service.Proxy{ID: id}, nil
}

func (f *quotaScriptAdminFixture) CreateAccount(context.Context, *service.CreateAccountInput) (*service.Account, error) {
	f.writes++
	return f.account, nil
}

func (f *quotaScriptAdminFixture) UpdateAccount(context.Context, int64, *service.UpdateAccountInput) (*service.Account, error) {
	f.writes++
	return f.account, nil
}

func TestQuotaScriptPreviewDrafts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	proxyID := int64(9)
	fixture := &quotaScriptAdminFixture{account: &service.Account{
		ID: 1, Type: service.AccountTypeAPIKey, Platform: "kimi", ProxyID: &proxyID,
		Credentials: map[string]any{"api_key": "saved-key", "base_url": "https://saved.example",
			"account_mode": "coding", "api_protocol": "adaptive",
			"api_base_urls": map[string]any{"chat_completions": "https://saved.example"}},
		Extra: map[string]any{"unrelated": true, "enable_tls_fingerprint": true, "quota_script": "saved script"},
	}}
	handler := &AccountHandler{adminService: fixture, accountUsageService: &service.AccountUsageService{}}
	router := gin.New()
	router.POST("/quota-script/test", handler.TestQuotaScript)
	request := func(body map[string]any) *httptest.ResponseRecorder {
		data, err := json.Marshal(body)
		require.NoError(t, err)
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/quota-script/test", bytes.NewReader(data))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(recorder, req)
		return recorder
	}
	for _, tc := range []struct {
		name      string
		body      map[string]any
		key, base string
		proxies   []int64
	}{
		{"create", map[string]any{"api_key": "draft-key", "base_url": "https://draft.example", "proxy_id": 17}, "draft-key", "https://draft.example", []int64{17}},
		{"saved baseline", map[string]any{"account_id": 1, "api_key": " ", "base_url": ""}, "saved-key", "https://saved.example", []int64{9}},
		{"edit draft and clear proxy", map[string]any{"account_id": 1, "api_key": "draft-key", "base_url": "https://draft.example", "proxy_id": nil,
			"extra": map[string]any{"unrelated": nil, "enable_tls_fingerprint": false}}, "draft-key", "https://draft.example", nil},
		{"select proxy", map[string]any{"account_id": 1, "proxy_id": 20}, "saved-key", "https://saved.example", []int64{20}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture.proxies = nil
			tc.body["script"] = "function queryQuota(ctx){return {balance: ctx.apiKey === '" + tc.key + "' && ctx.baseUrl === '" + tc.base + "' ? 1 : 0};}"
			recorder := request(tc.body)
			require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
			var envelope struct {
				Code int               `json:"code"`
				Data service.UsageInfo `json:"data"`
			}
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
			require.Zero(t, envelope.Code)
			require.NotNil(t, envelope.Data.Balance)
			require.Equal(t, 1.0, *envelope.Data.Balance)
			require.Equal(t, tc.proxies, fixture.proxies)
			require.Zero(t, fixture.writes)
			require.Equal(t, "saved-key", fixture.account.Credentials["api_key"])
			require.Equal(t, "https://saved.example", fixture.account.GetCNProtocolBaseURL(service.APIProtocolChatCompletions))
			require.Equal(t, map[string]any{"unrelated": true, "enable_tls_fingerprint": true, "quota_script": "saved script"}, fixture.account.Extra)
			require.Equal(t, int64(9), *fixture.account.ProxyID)
		})
	}
	failed := request(map[string]any{"account_id": 1, "script": "function queryQuota(ctx){throw new Error('failed '+ctx.apiKey)}"})
	require.Equal(t, http.StatusBadGateway, failed.Code)
	require.Contains(t, failed.Body.String(), "failed")
	require.NotContains(t, failed.Body.String(), "saved-key")
	require.Equal(t, http.StatusBadRequest, request(map[string]any{"script": " "}).Code)
}
