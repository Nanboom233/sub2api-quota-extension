// sub2api-package: backend/internal/service
package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

func TestUpstreamQuotaNativeMapping(t *testing.T) {
	now := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name       string
		body       string
		want       map[string]float64
		wantExpiry string
	}{
		{"wallet zero", `{"mode":"unrestricted","unit":"USD","balance":0,"remaining":0}`, map[string]float64{"balance": 0}, ""},
		{"key total", `{"mode":"quota_limited","quota":{"limit":80,"used":20,"remaining":60,"unit":"USD"},"remaining":60,"expires_at":"2026-11-20T00:00:00Z"}`, map[string]float64{"total": 25, "balance": 60}, "2026-11-20T00:00:00Z"},
		{"rate only", `{"mode":"quota_limited","rate_limits":[{"window":"5h","limit":20,"used":0,"reset_at":"2026-10-26T01:00:00Z"},{"window":"1d","limit":100,"used":40},{"window":"7d","limit":200,"used":150}]}`, map[string]float64{"5h": 0, "1d": 40, "7d": 75}, ""},
		{"subscription", `{"mode":"unrestricted","remaining":5,"expires_at":"2026-12-01T00:00:00Z","subscription":{"daily_limit_usd":10,"daily_usage_usd":5,"weekly_limit_usd":50,"weekly_usage_usd":20,"monthly_limit_usd":200,"monthly_usage_usd":30,"weekly_window_start":"2026-10-25T00:00:00Z","expires_at":"2026-11-25T00:00:00Z"}}`, map[string]float64{"1d": 50, "7d": 40, "30d": 15}, "2026-11-25T00:00:00Z"},
		{"earlier key expiry", `{"mode":"unrestricted","expires_at":"2026-11-01T00:00:00Z","subscription":{"expires_at":"2026-11-25T00:00:00Z"}}`, nil, "2026-11-01T00:00:00Z"},
		{"subscription expiry only", `{"mode":"unrestricted","subscription":{"expires_at":"2026-11-25T00:00:00Z"}}`, nil, "2026-11-25T00:00:00Z"},
		{"omitted quota", `{"mode":"quota_limited"}`, nil, ""},
		{"incomplete window", `{"mode":"quota_limited","rate_limits":[{"window":"5h","used":0}]}`, nil, ""},
		{"unlimited subscription", `{"mode":"unrestricted","remaining":-1,"subscription":{"daily_limit_usd":null,"daily_usage_usd":0}}`, nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			usage, err := parseUpstreamQuota([]byte(tc.body), now)
			got := map[string]float64{}
			require.NoError(t, err)
			if tc.wantExpiry == "" {
				require.Nil(t, usage.ExpiresAt)
			} else {
				expiresAt, err := time.Parse(time.RFC3339, tc.wantExpiry)
				require.NoError(t, err)
				require.Equal(t, &expiresAt, usage.ExpiresAt)
			}
			for name, progress := range map[string]*UsageProgress{"5h": usage.FiveHour, "1d": usage.OneDay, "7d": usage.SevenDay, "30d": usage.ThirtyDay, "total": usage.QuotaTotal} {
				if progress != nil {
					got[name] = progress.Utilization
				}
			}
			if usage.Balance != nil {
				got["balance"] = *usage.Balance
			}
			require.Equal(t, len(tc.want), len(got))
			for name, value := range tc.want {
				require.Contains(t, got, name)
				require.InDelta(t, value, got[name], 0.00001)
			}
			if tc.name == "rate only" {
				require.Equal(t, 3600, usage.FiveHour.RemainingSeconds)
				require.Equal(t, now.Add(time.Hour), *usage.FiveHour.ResetsAt)
			}
			if tc.name == "subscription" {
				resetAt := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
				require.Equal(t, &resetAt, usage.SevenDay.ResetsAt)
				require.Equal(t, 6*24*60*60, usage.SevenDay.RemainingSeconds)
				require.Nil(t, usage.ThirtyDay.ResetsAt)
				require.Zero(t, usage.ThirtyDay.RemainingSeconds)
			}
		})
	}
	undated, err := parseUpstreamQuota([]byte(`{"mode":"unrestricted","subscription":{"weekly_limit_usd":50,"weekly_usage_usd":20,"monthly_limit_usd":200,"monthly_usage_usd":30}}`), now)
	require.NoError(t, err)
	require.Nil(t, undated.SevenDay.ResetsAt)
	require.Zero(t, undated.SevenDay.RemainingSeconds)
	require.Nil(t, undated.ThirtyDay.ResetsAt)
	require.Zero(t, undated.ThirtyDay.RemainingSeconds)
	for _, body := range []string{`{}`, `{"mode":"unrestricted","balance":"zero"}`, `{"mode":"quota_limited","quota":{"used":-1,"limit":5}}`} {
		_, err := parseUpstreamQuota([]byte(body), now)
		require.Error(t, err)
	}
}

type quotaHTTPFixture struct {
	HTTPUpstream
	client *http.Client
}

type quotaTimeoutReader struct{}

func (quotaTimeoutReader) Read([]byte) (int, error) { return 0, context.DeadlineExceeded }

func (f *quotaHTTPFixture) DoWithTLS(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	switch req.Header.Get("Authorization") {
	case "Bearer transport":
		return nil, errors.New("fixture transport failure")
	case "Bearer timeout":
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(quotaTimeoutReader{})}, nil
	}
	return f.client.Do(req)
}

type quotaAccountRepoFixture struct {
	*upstreamBillingProbeAccountRepo
	writeErr error
}

func (r *quotaAccountRepoFixture) BulkUpdate(_ context.Context, ids []int64, updates AccountBulkUpdate) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bulkUpdates = append(r.bulkUpdates, updates)
	if r.writeErr != nil {
		return 0, r.writeErr
	}
	for _, id := range ids {
		if updates.ExpiresAt != nil {
			r.accounts[id].ExpiresAt = updates.ExpiresAt
		}
	}
	return int64(len(ids)), nil
}

func TestUpstreamQuotaExistingUsageEntryPoints(t *testing.T) {
	var calls atomic.Int64
	var fixtureBody atomic.Value
	fixtureBody.Store(`{"mode":"quota_limited","remaining":25,"unit":"USD","expires_at":"2026-12-01T00:00:00Z","subscription":{"expires_at":"2026-11-25T00:00:00Z"}}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		require.Equal(t, "/v1/usage", r.URL.Path)
		require.Equal(t, http.MethodGet, r.Method)
		w.Header().Set("Content-Type", "application/json")
		switch r.Header.Get("Authorization") {
		case "Bearer fixture":
			_, _ = io.WriteString(w, fixtureBody.Load().(string))
		case "Bearer unauthorized":
			w.WriteHeader(http.StatusUnauthorized)
		case "Bearer forbidden":
			w.WriteHeader(http.StatusForbidden)
		case "Bearer unsupported":
			w.WriteHeader(http.StatusNotFound)
		case "Bearer server":
			w.WriteHeader(http.StatusBadGateway)
		case "Bearer malformed":
			_, _ = w.Write([]byte(`{"balance":999}`))
		default:
			t.Errorf("unexpected authentication: %q", r.Header.Get("Authorization"))
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer server.Close()
	repo := &quotaAccountRepoFixture{upstreamBillingProbeAccountRepo: &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{}}}
	cases := []string{"fixture", "unauthorized", "forbidden", "unsupported", "server", "malformed", "transport", "timeout"}
	ids := make([]int64, len(cases))
	for i, key := range cases {
		id := int64(i + 1)
		ids[i] = id
		repo.accounts[id] = &Account{ID: id, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
			Credentials: map[string]any{"base_url": server.URL + "/v1", "api_key": key},
			Extra:       map[string]any{UpstreamBillingProbeEnabledExtraKey: false}}
	}
	savedExpiry := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	repo.accounts[1].ExpiresAt = &savedExpiry
	repo.accounts[1].AutoPauseOnExpired = true
	repo.accounts[1].Status = StatusActive
	repo.accounts[1].Schedulable = true
	wantAccount := *repo.accounts[1]
	cfg := &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true}}}
	svc := &AccountUsageService{accountRepo: repo, upstreamHTTP: &quotaHTTPFixture{client: server.Client()}, upstreamConfig: cfg}
	usage, err := svc.GetUsage(context.Background(), 1)
	require.NoError(t, err)
	require.NotNil(t, usage.Balance)
	require.Equal(t, 25.0, *usage.Balance)
	wantExpiry := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	wantAccount.ExpiresAt = &wantExpiry
	require.Equal(t, wantAccount, *repo.accounts[1])
	require.Equal(t, []AccountBulkUpdate{{ExpiresAt: &wantExpiry}}, repo.bulkUpdates)

	// An equal reported expiry needs no write. Missing expiry must preserve the saved value.
	_, err = svc.GetUsage(context.Background(), 1)
	require.NoError(t, err)
	require.Len(t, repo.bulkUpdates, 1)
	fixtureBody.Store(`{"mode":"unrestricted","balance":25,"unit":"USD"}`)
	batch, failures, err := svc.GetUsageBatch(context.Background(), ids, true)
	require.NoError(t, err)
	require.Empty(t, failures)
	require.Len(t, batch, len(cases))
	for i, key := range cases {
		result := batch[int64(i+1)]
		require.Empty(t, result.Error, key)
		require.Empty(t, result.ErrorCode, key)
		if key == "fixture" {
			require.Equal(t, 25.0, *result.Balance)
		} else {
			require.Equal(t, &UsageInfo{}, result, key)
		}
	}
	require.Equal(t, wantAccount, *repo.accounts[1])
	require.Len(t, repo.bulkUpdates, 1)

	fixtureBody.Store(`{"mode":"unrestricted","balance":25,"expires_at":"2020-01-01T00:00:00Z"}`)
	_, err = svc.GetUsage(context.Background(), 1)
	require.NoError(t, err)
	pastExpiry := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	wantAccount.ExpiresAt = &pastExpiry
	require.Equal(t, wantAccount, *repo.accounts[1])
	require.Equal(t, AccountBulkUpdate{ExpiresAt: &pastExpiry}, repo.bulkUpdates[1])

	repo.writeErr = errors.New("fixture expiry write failure")
	fixtureBody.Store(`{"mode":"unrestricted","balance":25,"expires_at":"2026-12-01T00:00:00Z"}`)
	usage, err = svc.GetUsage(context.Background(), 1)
	require.NoError(t, err)
	require.Empty(t, usage.Error)
	require.Empty(t, usage.ErrorCode)
	require.Equal(t, 25.0, *usage.Balance)
	require.Equal(t, wantAccount, *repo.accounts[1])
	native := &UsageInfo{FiveHour: &UsageProgress{Utilization: 35}, Error: "Native provider error", ErrorCode: "native_provider_error"}
	wantNative := *native
	svc.fillUpstreamAPIKeyUsage(context.Background(), repo.accounts[6], server.URL, native)
	require.Equal(t, &wantNative, native)
	before := calls.Load()
	invalid := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"base_url": "not-a-url", "api_key": "fixture"}}
	usage, err = svc.GetUsageForAccount(context.Background(), invalid)
	require.NoError(t, err)
	require.Equal(t, &UsageInfo{}, usage)
	official := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"base_url": "https://api.openai.com/v1", "api_key": "fixture"}}
	usage, err = svc.GetUsageForAccount(context.Background(), official)
	require.NoError(t, err)
	require.Empty(t, usage.ErrorCode)
	require.Equal(t, before, calls.Load())
}
