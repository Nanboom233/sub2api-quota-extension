// sub2api-package: backend/internal/service
package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestQuotaScriptHTTPAndResult(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/balance", r.URL.Path)
		require.Equal(t, "Bearer synthetic-key", r.Header.Get("Authorization"))
		require.Equal(t, "fixture", r.Header.Get("X-Quota-Fixture"))
		require.Equal(t, http.MethodPost, r.Method)
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.Equal(t, "request", string(body))
		_, _ = w.Write([]byte(`{"remaining":0}`))
	}))
	defer server.Close()
	account := &Account{Type: AccountTypeAPIKey, Platform: PlatformOpenAI,
		Credentials: map[string]any{"api_key": "synthetic-key", "base_url": server.URL,
			"header_override_enabled": true, "header_overrides": map[string]any{"X-Quota-Fixture": "fixture"}}}
	svc := &AccountUsageService{upstreamHTTP: &quotaHTTPFixture{client: server.Client()},
		upstreamConfig: &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true}}}}
	script := `function queryQuota(ctx) {
  const response = http.request({url: ctx.baseUrl + '/balance', method: 'POST',
    headers: {Authorization: 'Bearer ' + ctx.apiKey}, body: 'request'});
  if (response.status !== 200) throw new Error('HTTP ' + response.status);
  const data = JSON.parse(response.body);
  return {session: {used: 0, limit: 10, reset_at: '2099-01-01T00:00:00Z'},
    day: {used: 2, limit: 10}, week: {used: 3, limit: 10},
    month: {used: 4, limit: 10}, total: {used: 12, limit: 10},
    balance: {amount: data.remaining, unit: ' credits '}, unit: 'unused'};
}`
	usage, err := svc.ExecuteQuotaScript(context.Background(), account, script)
	require.NoError(t, err)
	require.Equal(t, 0.0, usage.FiveHour.Utilization)
	require.NotNil(t, usage.FiveHour.ResetsAt)
	require.Positive(t, usage.FiveHour.RemainingSeconds)
	require.Equal(t, 20.0, usage.OneDay.Utilization)
	require.Equal(t, 30.0, usage.SevenDay.Utilization)
	require.Equal(t, 40.0, usage.ThirtyDay.Utilization)
	require.Equal(t, 120.0, usage.QuotaTotal.Utilization)
	require.Equal(t, 0.0, *usage.Balance)
	require.Equal(t, "credits", usage.BalanceUnit)
	require.NotNil(t, usage.UpdatedAt)

	for _, tc := range []struct {
		result, unit string
		balance      *float64
	}{
		{`{balance: -5, unit: 'EUR'}`, "EUR", quotaScriptFloat(-5)},
		{`{balance: {amount: 5}, unit: 'tokens'}`, "tokens", quotaScriptFloat(5)},
		{`{balance: 0}`, "", quotaScriptFloat(0)},
		{`{unit: null}`, "", nil},
	} {
		result, err := svc.ExecuteQuotaScript(context.Background(), account, "function queryQuota(){return "+tc.result+";}")
		require.NoError(t, err)
		require.Equal(t, tc.balance, result.Balance)
		require.Equal(t, tc.unit, result.BalanceUnit)
		require.Nil(t, result.FiveHour)
	}
}

func quotaScriptFloat(value float64) *float64 { return &value }

func TestQuotaScriptValidationAndDiagnostics(t *testing.T) {
	account := &Account{Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "synthetic-secret"},
		Proxy: &Proxy{Username: "synthetic-proxy-user", Password: "synthetic-proxy-password"}}
	svc := &AccountUsageService{}
	for _, result := range []string{
		`[]`, `null`, `Promise.resolve({})`, `{balance: NaN}`, `{balance: '1'}`,
		`{balance: {amount: 1, unit: ''}}`, `{balance: 1, unit: null}`,
		`{balance: 1, day: {used: -1, limit: 5}}`, `{day: {used: 1, limit: 0}}`,
		`{day: {used: 1, limit: Infinity}}`, `{day: {used: 1, limit: 5, reset_at: 'tomorrow'}}`,
		`{get balance(){throw new Error('getter failed')}}`,
	} {
		usage, err := svc.ExecuteQuotaScript(context.Background(), account, "function queryQuota(){return "+result+";}")
		require.Error(t, err, result)
		require.Nil(t, usage, result)
	}
	for _, script := range []string{"function queryQuota( {", `function queryQuota(ctx){throw new Error('failed: '+ctx.apiKey+' synthetic-proxy-user synthetic-proxy-password')}`} {
		usage, err := svc.ExecuteQuotaScript(context.Background(), account, script)
		require.Error(t, err)
		require.NotEmpty(t, err.Error())
		require.NotContains(t, err.Error(), "synthetic-secret")
		require.NotContains(t, err.Error(), "synthetic-proxy-user")
		require.NotContains(t, err.Error(), "synthetic-proxy-password")
		require.Nil(t, usage)
	}
}

func TestQuotaScriptCancellation(t *testing.T) {
	canceled := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		canceled <- struct{}{}
	}))
	defer server.Close()
	svc := &AccountUsageService{upstreamHTTP: &quotaHTTPFixture{client: server.Client()},
		upstreamConfig: &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true}}}}
	account := &Account{Type: AccountTypeAPIKey, Credentials: map[string]any{"base_url": server.URL}}
	for _, script := range []string{
		`function queryQuota(){while(true){}}`,
		`function queryQuota(){return {get balance(){while(true){}}}}`,
		`function queryQuota(){throw {toString(){while(true){}}}}`,
		`function queryQuota(ctx){http.request({url:ctx.baseUrl});return {}}`,
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		started := time.Now()
		usage, err := svc.ExecuteQuotaScript(ctx, account, script)
		cancel()
		require.Error(t, err)
		require.Nil(t, usage)
		require.Less(t, time.Since(started), 2*time.Second)
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("HTTP request did not stop")
	}
}

func TestQuotaScriptStorageAndDispatch(t *testing.T) {
	provided := map[string]any{"unrelated": true, QuotaScriptExtraKey: " \n "}
	normalized, err := normalizeQuotaScriptExtra(provided)
	require.NoError(t, err)
	require.Equal(t, map[string]any{"unrelated": true}, normalized)
	require.Contains(t, provided, QuotaScriptExtraKey)
	_, err = normalizeQuotaScriptExtra(map[string]any{QuotaScriptExtraKey: false})
	require.Error(t, err)
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = fmt.Fprint(w, `{"mode":"unrestricted","balance":9}`)
	}))
	defer server.Close()
	account := &Account{ID: 1, Type: AccountTypeAPIKey, Platform: PlatformOpenAI,
		Credentials: map[string]any{"api_key": "fixture", "base_url": server.URL},
		Extra:       map[string]any{QuotaScriptExtraKey: `function queryQuota(){return {balance:7}}`}}
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{1: account}}
	svc := &AccountUsageService{accountRepo: repo, upstreamHTTP: &quotaHTTPFixture{client: server.Client()},
		upstreamConfig: &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true}}}}
	usage, err := svc.GetUsage(context.Background(), 1)
	require.NoError(t, err)
	require.Equal(t, 7.0, *usage.Balance)
	account.Extra[QuotaScriptExtraKey] = `function queryQuota(){throw new Error('custom failure')}`
	batch, failures, err := svc.GetUsageBatch(context.Background(), []int64{1}, true)
	require.NoError(t, err)
	require.Empty(t, failures)
	require.Contains(t, batch[1].Error, "custom failure")
	require.Nil(t, batch[1].Balance)
	require.Zero(t, calls.Load())
	delete(account.Extra, QuotaScriptExtraKey)
	usage, err = svc.GetUsage(context.Background(), 1)
	require.NoError(t, err)
	require.Empty(t, usage.Error)
	require.Equal(t, 9.0, *usage.Balance)
	require.EqualValues(t, 1, calls.Load())
	account.Extra[QuotaScriptExtraKey] = strings.Repeat(" ", 2)
	require.Empty(t, AccountQuotaScript(account))
}
