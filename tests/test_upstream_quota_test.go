// sub2api-package: backend/internal/service
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
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
	writeErr       error
	proxies        map[int64]*Proxy
	onBeforeUpdate func()
}

func cloneFixtureAccount(account *Account) *Account {
	if account == nil {
		return nil
	}
	clone := *account
	clone.Credentials = mergeMap(nil, account.Credentials)
	clone.Extra = mergeMap(nil, account.Extra)
	if account.ProxyID != nil {
		proxyID := *account.ProxyID
		clone.ProxyID = &proxyID
	}
	if account.Proxy != nil {
		proxyCopy := *account.Proxy
		clone.Proxy = &proxyCopy
	}
	if account.ExpiresAt != nil {
		expCopy := *account.ExpiresAt
		clone.ExpiresAt = &expCopy
	}
	if account.TempUnschedulableUntil != nil {
		untilCopy := *account.TempUnschedulableUntil
		clone.TempUnschedulableUntil = &untilCopy
	}
	return &clone
}

func fixtureCredentialsEqual(a, b map[string]any) bool {
	if a == nil {
		a = map[string]any{}
	}
	if b == nil {
		b = map[string]any{}
	}
	ja, errA := json.Marshal(a)
	jb, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(ja) == string(jb)
}

func fixtureProxyIDEqual(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func fixtureProxyIdentityMatches(stored, expected *Account, proxies map[int64]*Proxy) bool {
	if expected.ProxyID == nil {
		return true
	}
	var current *Proxy
	if proxies != nil {
		current = proxies[*expected.ProxyID]
	} else {
		current = stored.Proxy
	}
	if current == nil {
		return expected.Proxy == nil
	}
	if expected.Proxy == nil || expected.Proxy.ID != *expected.ProxyID {
		return false
	}
	return current.Protocol == expected.Proxy.Protocol &&
		current.Host == expected.Proxy.Host &&
		current.Port == expected.Proxy.Port &&
		current.Username == expected.Proxy.Username &&
		current.Password == expected.Proxy.Password &&
		current.Status == expected.Proxy.Status
}

func (r *quotaAccountRepoFixture) GetByID(_ context.Context, id int64) (*Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	account := r.accounts[id]
	if account == nil {
		return nil, ErrAccountNotFound
	}
	return cloneFixtureAccount(account), nil
}

func (r *quotaAccountRepoFixture) GetByIDs(_ context.Context, ids []int64) ([]*Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]*Account, 0, len(ids))
	for _, id := range ids {
		if account := r.accounts[id]; account != nil {
			result = append(result, cloneFixtureAccount(account))
		}
	}
	return result, nil
}

func (r *quotaAccountRepoFixture) ListShadowsByParent(_ context.Context, _ int64) ([]*Account, error) {
	return nil, nil
}

func (r *quotaAccountRepoFixture) Update(_ context.Context, account *Account) error {
	if r.onBeforeUpdate != nil {
		r.onBeforeUpdate()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.saveAccountLocked(account)
}

func (r *quotaAccountRepoFixture) UpdateWithAccountBillingSettings(_ context.Context, account *Account, settings AccountBillingSettingsUpdate) error {
	if r.onBeforeUpdate != nil {
		r.onBeforeUpdate()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.writeErr != nil {
		return r.writeErr
	}
	current := r.accounts[account.ID]
	if current == nil {
		return ErrAccountNotFound
	}
	identityUnchanged := current.Platform == account.Platform && current.Type == account.Type &&
		fixtureCredentialsEqual(current.Credentials, account.Credentials) && fixtureProxyIDEqual(current.ProxyID, account.ProxyID)
	schedulingUnchanged := identityUnchanged && AccountQuotaScript(current) == AccountQuotaScript(account)
	if settings.MergeAPIKeyQuotaState {
		for _, key := range APIKeyQuotaManagedExtraKeys {
			delete(account.Extra, key)
			if schedulingUnchanged || (identityUnchanged && key == APIKeyUsageUnsupportedIdentityExtraKey) {
				if account.Extra == nil {
					account.Extra = map[string]any{}
				}
				if value, ok := current.Extra[key]; ok {
					account.Extra[key] = value
				}
			}
		}
		previous := settings.PreviousExpiresAt
		if (account.ExpiresAt == nil && previous == nil) ||
			(account.ExpiresAt != nil && previous != nil && account.ExpiresAt.Unix() == previous.Unix()) {
			account.ExpiresAt = current.ExpiresAt
		}
		if !schedulingUnchanged && strings.HasPrefix(current.TempUnschedulableReason, "apikey_quota_") {
			current.TempUnschedulableUntil = nil
			current.TempUnschedulableReason = ""
		}
	}
	return r.saveAccountLocked(account)
}

func (r *quotaAccountRepoFixture) saveAccountLocked(account *Account) error {
	if r.writeErr != nil {
		return r.writeErr
	}
	existing := r.accounts[account.ID]
	if existing == nil {
		return ErrAccountNotFound
	}
	saved := cloneFixtureAccount(account)
	// Match accountRepository.updateLockedAccount, which never overwrites temp_unschedulable_*.
	if existing.TempUnschedulableUntil != nil {
		untilCopy := *existing.TempUnschedulableUntil
		saved.TempUnschedulableUntil = &untilCopy
	} else {
		saved.TempUnschedulableUntil = nil
	}
	saved.TempUnschedulableReason = existing.TempUnschedulableReason
	r.accounts[account.ID] = saved
	return nil
}

func (r *quotaAccountRepoFixture) BulkUpdate(_ context.Context, ids []int64, updates AccountBulkUpdate) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bulkUpdates = append(r.bulkUpdates, updates)
	if r.writeErr != nil {
		return 0, r.writeErr
	}
	removableOnNil := map[string]struct{}{
		UpstreamBillingProbeExtraKey: {},
		QuotaScriptExtraKey:          {},
	}
	for _, key := range APIKeyQuotaManagedExtraKeys {
		removableOnNil[key] = struct{}{}
	}
	clearQuotaPause := false
	if val, ok := updates.Extra[APIKeyQuotaExhaustedExtraKey]; ok && val == nil {
		clearQuotaPause = true
	}
	for _, id := range ids {
		account := r.accounts[id]
		if account == nil {
			continue
		}
		if len(updates.Credentials) > 0 {
			if account.Credentials == nil {
				account.Credentials = make(map[string]any, len(updates.Credentials))
			}
			for k, v := range updates.Credentials {
				account.Credentials[k] = v
			}
		}
		if updates.ProxyID != nil {
			if *updates.ProxyID == 0 {
				account.ProxyID = nil
				account.Proxy = nil
			} else {
				proxyID := *updates.ProxyID
				account.ProxyID = &proxyID
			}
		}
		if len(updates.Extra) > 0 {
			if account.Extra == nil {
				account.Extra = make(map[string]any, len(updates.Extra))
			}
			for k, v := range updates.Extra {
				if v == nil {
					if _, removable := removableOnNil[k]; removable {
						delete(account.Extra, k)
						continue
					}
				}
				account.Extra[k] = v
			}
		}
		if clearQuotaPause && strings.HasPrefix(account.TempUnschedulableReason, "apikey_quota_") {
			account.TempUnschedulableUntil = nil
			account.TempUnschedulableReason = ""
		}
	}
	return int64(len(ids)), nil
}

func (r *quotaAccountRepoFixture) UpdateAPIKeyQuotaState(
	_ context.Context,
	expected *Account,
	usage *UsageInfo,
	expiresAt *time.Time,
	unsupported *string,
) (bool, error) {
	if expected == nil {
		return false, ErrAccountNilInput
	}
	if expected.ID <= 0 || (usage == nil && expiresAt == nil && unsupported == nil) {
		return false, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	stored := r.accounts[expected.ID]
	if stored == nil {
		return false, nil
	}
	if !fixtureProxyIdentityMatches(stored, expected, r.proxies) ||
		stored.Platform != expected.Platform ||
		stored.Type != expected.Type ||
		!fixtureCredentialsEqual(stored.Credentials, expected.Credentials) ||
		!fixtureProxyIDEqual(stored.ProxyID, expected.ProxyID) ||
		AccountQuotaScript(stored) != AccountQuotaScript(expected) {
		return false, nil
	}
	changed := false
	extra, until := APIKeyQuotaSchedulingUpdates(stored.Extra, usage, time.Now().UTC())
	schedulingChanged := len(extra) > 0
	if unsupported != nil {
		if extra == nil {
			extra = make(map[string]any)
		}
		extra[APIKeyUsageUnsupportedIdentityExtraKey] = *unsupported
	}
	for key, value := range extra {
		if !fixtureCredentialsEqual(map[string]any{"v": stored.Extra[key]}, map[string]any{"v": value}) {
			changed = true
		}
	}
	if expiresAt != nil && (stored.ExpiresAt == nil || !stored.ExpiresAt.Equal(*expiresAt)) {
		changed = true
	}
	if schedulingChanged && until == nil && strings.HasPrefix(stored.TempUnschedulableReason, "apikey_quota_") {
		changed = true
	}
	if schedulingChanged && until != nil && stored.IsActive() && stored.Schedulable &&
		(stored.TempUnschedulableUntil == nil || !time.Now().Before(*stored.TempUnschedulableUntil) || strings.HasPrefix(stored.TempUnschedulableReason, "apikey_quota_")) &&
		(stored.TempUnschedulableUntil == nil || !stored.TempUnschedulableUntil.Equal(*until) || stored.TempUnschedulableReason != apiKeyQuotaTempUnschedulableReasonPrefix) {
		changed = true
	}
	if !changed {
		return false, nil
	}
	if len(extra) > 0 {
		if r.updates == nil {
			r.updates = make(map[int64][]map[string]any)
		}
		r.updates[expected.ID] = append(r.updates[expected.ID], maps.Clone(extra))
	}
	if r.writeErr != nil {
		return false, r.writeErr
	}
	if len(extra) > 0 {
		if stored.Extra == nil {
			stored.Extra = make(map[string]any, len(extra))
		}
		for k, v := range extra {
			stored.Extra[k] = v
		}
	}
	if expiresAt != nil {
		expCopy := *expiresAt
		stored.ExpiresAt = &expCopy
	}
	if schedulingChanged {
		if until != nil {
			now := time.Now().UTC()
			if stored.IsActive() && stored.Schedulable && (stored.TempUnschedulableUntil == nil ||
				!now.Before(*stored.TempUnschedulableUntil) ||
				strings.HasPrefix(stored.TempUnschedulableReason, "apikey_quota_")) {
				untilCopy := *until
				stored.TempUnschedulableUntil = &untilCopy
				stored.TempUnschedulableReason = apiKeyQuotaTempUnschedulableReasonPrefix
			}
		} else {
			if strings.HasPrefix(stored.TempUnschedulableReason, "apikey_quota_") {
				stored.TempUnschedulableUntil = nil
				stored.TempUnschedulableReason = ""
			}
		}
	}
	*expected = *cloneFixtureAccount(stored)
	return true, nil
}

func (r *quotaAccountRepoFixture) SetTempUnschedulable(_ context.Context, id int64, until time.Time, reason string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	account := r.accounts[id]
	if account == nil {
		return ErrAccountNotFound
	}
	if account.TempUnschedulableUntil == nil || account.TempUnschedulableUntil.Before(until) {
		untilCopy := until
		account.TempUnschedulableUntil = &untilCopy
		account.TempUnschedulableReason = reason
	}
	return nil
}

func (r *quotaAccountRepoFixture) ClearTempUnschedulable(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	account := r.accounts[id]
	if account == nil {
		return ErrAccountNotFound
	}
	account.TempUnschedulableUntil = nil
	account.TempUnschedulableReason = ""
	return nil
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
	require.Empty(t, repo.bulkUpdates)

	// An equal reported expiry needs no write. Missing expiry must preserve the saved value.
	_, err = svc.GetUsage(context.Background(), 1)
	require.NoError(t, err)
	require.Empty(t, repo.bulkUpdates)
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
	require.Empty(t, repo.bulkUpdates)

	fixtureBody.Store(`{"mode":"unrestricted","balance":25,"expires_at":"2020-01-01T00:00:00Z"}`)
	_, err = svc.GetUsage(context.Background(), 1)
	require.NoError(t, err)
	pastExpiry := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	wantAccount.ExpiresAt = &pastExpiry
	require.Equal(t, wantAccount, *repo.accounts[1])
	require.Empty(t, repo.bulkUpdates)

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
	svc.fillUpstreamAPIKeyUsage(context.Background(), repo.accounts[6], server.URL, native, false)
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

func TestUpstreamQuotaProbeMemoization(t *testing.T) {
	hits := map[string]*atomic.Int64{}
	for _, key := range []string{
		"notfound", "method", "notimpl", "malformed",
		"unauth", "forbid", "ratelimit", "badgw",
		"supported", "newkey",
		"bill-404", "bill-405", "bill-501", "bill-malformed", "bill-recover",
	} {
		hits[key] = &atomic.Int64{}
	}
	var recoverSupported atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Authorization")[len("Bearer "):]
		if counter, ok := hits[key]; ok {
			counter.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		switch key {
		case "notfound", "bill-404":
			w.WriteHeader(http.StatusNotFound)
		case "method", "bill-405":
			w.WriteHeader(http.StatusMethodNotAllowed)
		case "notimpl", "bill-501":
			w.WriteHeader(http.StatusNotImplemented)
		case "malformed", "bill-malformed":
			_, _ = w.Write([]byte(`{"balance":999}`))
		case "bill-recover":
			if !recoverSupported.Load() {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if r.URL.Path == "/v1/sub2api/billing" {
				_, _ = w.Write([]byte(`{"object":"sub2api.key_billing","schema_version":1,"billing_scope":"token","group_rate_multiplier":1,"resolved_rate_multiplier":1,"peak_rate_enabled":false,"effective_rate_multiplier":1,"observed_at":"2026-10-26T00:00:00Z"}`))
				return
			}
			_, _ = w.Write([]byte(`{"mode":"unrestricted","balance":42,"unit":"USD"}`))
		case "unauth":
			w.WriteHeader(http.StatusUnauthorized)
		case "forbid":
			w.WriteHeader(http.StatusForbidden)
		case "ratelimit":
			w.WriteHeader(http.StatusTooManyRequests)
		case "badgw":
			w.WriteHeader(http.StatusBadGateway)
		case "supported", "newkey":
			_, _ = w.Write([]byte(`{"mode":"unrestricted","balance":18,"unit":"USD"}`))
		}
	}))
	defer server.Close()

	repo := &quotaAccountRepoFixture{upstreamBillingProbeAccountRepo: &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{}}}
	cfg := &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true}}}
	httpFixture := &quotaHTTPFixture{client: server.Client()}
	svc := &AccountUsageService{accountRepo: repo, upstreamHTTP: httpFixture, upstreamConfig: cfg}
	settingsRepo := &upstreamBillingProbeSettingRepo{values: map[string]string{
		SettingKeyUpstreamBillingProbeSettings: `{"enabled":true,"interval_minutes":30}`,
	}}
	billingSvc := newUpstreamBillingProbeTestService(repo, httpFixture, settingsRepo)
	now := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	billingSvc.now = func() time.Time { return now }

	memoizedKeys := []string{"notfound", "method", "notimpl", "malformed"}
	for i, key := range memoizedKeys {
		id := int64(i + 1)
		repo.accounts[id] = &Account{
			ID: id, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive,
			Credentials: map[string]any{"base_url": server.URL + "/v1", "api_key": key},
			Extra:       map[string]any{UpstreamBillingProbeEnabledExtraKey: true},
		}
		_, err := svc.GetUsage(context.Background(), id)
		require.NoError(t, err, key)
		require.EqualValues(t, 1, hits[key].Load(), key)
		require.Equal(t, upstreamAPIKeyUsageProbeIdentity(repo.accounts[id]), repo.accounts[id].Extra[APIKeyUsageUnsupportedIdentityExtraKey], key)
		require.Len(t, repo.updates[id], 1, key)

		// Automatic single, batch, and scheduled billing probes must skip memoized accounts.
		_, err = svc.GetUsage(context.Background(), id)
		require.NoError(t, err, key)
		_, failures, err := svc.GetUsageBatch(context.Background(), []int64{id}, false)
		require.NoError(t, err, key)
		require.Empty(t, failures, key)
		require.NoError(t, billingSvc.RunDue(context.Background()), key)
		require.EqualValues(t, 1, hits[key].Load(), key)
		require.Len(t, repo.updates[id], 1, key)

		// Manual forced single and batch usage refreshes must never be blocked by memoized state.
		_, err = svc.GetUsage(context.Background(), id, true)
		require.NoError(t, err, key)
		_, failures, err = svc.GetUsageBatch(context.Background(), []int64{id}, true)
		require.NoError(t, err, key)
		require.Empty(t, failures, key)
		require.EqualValues(t, 3, hits[key].Load(), key)
		require.Len(t, repo.updates[id], 1, key)
	}

	transientKeys := []string{"unauth", "forbid", "ratelimit", "badgw", "transport", "timeout"}
	for i, key := range transientKeys {
		id := int64(10 + i)
		repo.accounts[id] = &Account{
			ID: id, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
			Credentials: map[string]any{"base_url": server.URL + "/v1", "api_key": key},
			Extra:       map[string]any{},
		}
		_, err := svc.GetUsage(context.Background(), id)
		require.NoError(t, err, key)
		require.NotContains(t, repo.accounts[id].Extra, APIKeyUsageUnsupportedIdentityExtraKey, key)

		_, err = svc.GetUsage(context.Background(), id)
		require.NoError(t, err, key)
		if counter, ok := hits[key]; ok {
			require.EqualValues(t, 2, counter.Load(), key)
		}
	}

	// Cross-endpoint memoization from /v1/sub2api/billing (404, 405, 501, malformed 2xx, and official domain).
	billingUnsupportedKeys := []string{"bill-404", "bill-405", "bill-501", "bill-malformed"}
	for i, key := range billingUnsupportedKeys {
		id := int64(20 + i)
		repo.accounts[id] = &Account{
			ID: id, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Concurrency: 1,
			Credentials: map[string]any{"base_url": server.URL + "/v1", "api_key": key},
			Extra:       map[string]any{UpstreamBillingProbeEnabledExtraKey: true},
		}
		snapshot, err := billingSvc.ProbeAccount(context.Background(), id)
		require.NoError(t, err, key)
		require.Equal(t, UpstreamBillingProbeStatusUnsupported, snapshot.Status, key)
		require.EqualValues(t, 1, hits[key].Load(), key)
		require.Equal(t, upstreamAPIKeyUsageProbeIdentity(repo.accounts[id]), repo.accounts[id].Extra[APIKeyUsageUnsupportedIdentityExtraKey], key)

		// Automatic /v1/usage and scheduled RunDue (even after 25h > 24h max backoff) must skip memoized accounts.
		_, err = svc.GetUsage(context.Background(), id)
		require.NoError(t, err, key)
		now = now.Add(25 * time.Hour)
		require.NoError(t, billingSvc.RunDue(context.Background()), key)
		require.EqualValues(t, 1, hits[key].Load(), key)
	}

	// Official non-OpenAI domain short-circuits in billing probe and memoizes shared unsupported identity.
	officialID := int64(29)
	repo.accounts[officialID] = &Account{
		ID: officialID, Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Status: StatusActive, Concurrency: 1,
		Credentials: map[string]any{"base_url": "https://ollama.com/v1", "api_key": "sk-ollama"},
		Extra:       map[string]any{UpstreamBillingProbeEnabledExtraKey: true},
	}
	require.NoError(t, billingSvc.RunDue(context.Background()))
	require.Equal(t, upstreamAPIKeyUsageProbeIdentity(repo.accounts[officialID]), repo.accounts[officialID].Extra[APIKeyUsageUnsupportedIdentityExtraKey])

	// Manual ProbeAccount and manual GetUsage(force=true) bypass memoization and clear it upon valid response.
	recoverID := int64(30)
	repo.accounts[recoverID] = &Account{
		ID: recoverID, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Concurrency: 1,
		Credentials: map[string]any{"base_url": server.URL + "/v1", "api_key": "bill-recover"},
		Extra:       map[string]any{UpstreamBillingProbeEnabledExtraKey: true},
	}
	require.NoError(t, billingSvc.RunDue(context.Background()))
	require.EqualValues(t, 1, hits["bill-recover"].Load())
	require.NotEmpty(t, repo.accounts[recoverID].Extra[APIKeyUsageUnsupportedIdentityExtraKey])
	now = now.Add(25 * time.Hour)
	require.NoError(t, billingSvc.RunDue(context.Background()))
	require.EqualValues(t, 1, hits["bill-recover"].Load())

	// Manual ProbeAccount while still 404 is not blocked.
	snapshot, err := billingSvc.ProbeAccount(context.Background(), recoverID)
	require.NoError(t, err)
	require.Equal(t, UpstreamBillingProbeStatusUnsupported, snapshot.Status)
	require.Equal(t, 2, snapshot.FailureCount)
	require.EqualValues(t, 2, hits["bill-recover"].Load())

	// Upstream recovers: manual ProbeAccount succeeds and clears unsupported memoization, re-enabling automatic GetUsage.
	recoverSupported.Store(true)
	snapshot, err = billingSvc.ProbeAccount(context.Background(), recoverID)
	require.NoError(t, err)
	require.Equal(t, UpstreamBillingProbeStatusOK, snapshot.Status)
	require.EqualValues(t, 3, hits["bill-recover"].Load())
	require.Equal(t, "", repo.accounts[recoverID].Extra[APIKeyUsageUnsupportedIdentityExtraKey])
	usage, err := svc.GetUsage(context.Background(), recoverID)
	require.NoError(t, err)
	require.NotNil(t, usage.Balance)
	require.Equal(t, 42.0, *usage.Balance)
	require.EqualValues(t, 4, hits["bill-recover"].Load())

	// Changing base_url, api_key, proxy_id, or header_overrides invalidates the memoized identity.
	target := repo.accounts[1]
	target.Credentials["base_url"] = server.URL + "/custom/v1"
	_, err = svc.GetUsage(context.Background(), 1)
	require.NoError(t, err)
	require.EqualValues(t, 4, hits["notfound"].Load())

	proxyID := int64(7)
	target.ProxyID = &proxyID
	target.Proxy = &Proxy{ID: proxyID, Protocol: "http", Host: "127.0.0.1", Port: 8080}
	prevIdentity := upstreamAPIKeyUsageProbeIdentity(target)
	require.NotEqual(t, repo.accounts[1].Extra[APIKeyUsageUnsupportedIdentityExtraKey], prevIdentity)
	target.ProxyID = nil
	target.Proxy = nil

	target.Credentials["header_override_enabled"] = true
	target.Credentials["header_overrides"] = map[string]any{"x-custom": "1"}
	_, err = svc.GetUsage(context.Background(), 1)
	require.NoError(t, err)
	require.EqualValues(t, 5, hits["notfound"].Load())

	// Switching api_key to a supported endpoint probes again and clears the unsupported identity marker.
	target.Credentials["api_key"] = "newkey"
	usage, err = svc.GetUsage(context.Background(), 1)
	require.NoError(t, err)
	require.NotNil(t, usage.Balance)
	require.Equal(t, 18.0, *usage.Balance)
	require.EqualValues(t, 1, hits["newkey"].Load())
	require.Equal(t, "", repo.accounts[1].Extra[APIKeyUsageUnsupportedIdentityExtraKey])

	// Verify adminServiceImpl UpdateAccount, BulkUpdateAccounts, and DuplicateAccount preserve/clear managed keys.
	adminSvc := &adminServiceImpl{accountRepo: repo}
	repo.accounts[1].Extra[APIKeyUsageUnsupportedIdentityExtraKey] = "saved-fingerprint"
	repo.accounts[1].Extra[APIKeyQuotaExhaustedExtraKey] = true
	updated, err := adminSvc.UpdateAccount(context.Background(), 1, &UpdateAccountInput{
		Name:  "renamed",
		Extra: map[string]any{"custom": "kept", APIKeyUsageUnsupportedIdentityExtraKey: "forged"},
	})
	require.NoError(t, err)
	require.Equal(t, "saved-fingerprint", updated.Extra[APIKeyUsageUnsupportedIdentityExtraKey])
	require.Equal(t, true, updated.Extra[APIKeyQuotaExhaustedExtraKey])

	for _, key := range APIKeyQuotaManagedExtraKeys {
		updated.Extra[key] = true
	}
	duplicated, err := duplicateAccountExtra(updated.Extra)
	require.NoError(t, err)
	for _, key := range APIKeyQuotaManagedExtraKeys {
		require.NotContains(t, duplicated, key)
	}
	require.Equal(t, "kept", duplicated["custom"])

	futurePause := now.Add(time.Hour)
	repo.accounts[1].TempUnschedulableUntil = &futurePause
	repo.accounts[1].TempUnschedulableReason = apiKeyQuotaTempUnschedulableReasonPrefix
	updated, err = adminSvc.UpdateAccount(context.Background(), 1, &UpdateAccountInput{
		Credentials: map[string]any{"api_key": "rotated-key"},
	})
	require.NoError(t, err)
	require.NotContains(t, updated.Extra, APIKeyUsageUnsupportedIdentityExtraKey)
	require.NotContains(t, updated.Extra, APIKeyQuotaExhaustedExtraKey)
	require.Nil(t, repo.accounts[1].TempUnschedulableUntil)
	require.Empty(t, repo.accounts[1].TempUnschedulableReason)
}

func TestUpstreamQuotaSchedulingIntegration(t *testing.T) {
	var payload atomic.Value
	payload.Store(`{"mode":"quota_limited","remaining":0,"quota":{"limit":100,"used":100,"remaining":0,"unit":"USD"}}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, payload.Load().(string))
	}))
	defer server.Close()

	repo := &quotaAccountRepoFixture{upstreamBillingProbeAccountRepo: &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{
		1: {
			ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
			Status: StatusActive, Schedulable: true,
			Credentials: map[string]any{"base_url": server.URL + "/v1", "api_key": "sched-key"},
			Extra:       map[string]any{},
		},
	}}}
	cfg := &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true}}}
	svc := &AccountUsageService{accountRepo: repo, upstreamHTTP: &quotaHTTPFixture{client: server.Client()}, upstreamConfig: cfg}

	// 1. Undated quota exhaustion (remaining=0 and total used=100/100) pauses scheduling via IsQuotaExceeded.
	usage, err := svc.GetUsage(context.Background(), 1)
	require.NoError(t, err)
	require.NotNil(t, usage.Balance)
	require.Zero(t, *usage.Balance)
	require.True(t, repo.accounts[1].IsQuotaExceeded())
	require.False(t, repo.accounts[1].IsSchedulable())
	require.Equal(t, true, repo.accounts[1].Extra[APIKeyQuotaExhaustedExtraKey])
	updatesAfterFirst := len(repo.updates[1])

	// Repeated identical exhausted query is idempotent (no duplicate UpdateExtra write).
	_, err = svc.GetUsage(context.Background(), 1)
	require.NoError(t, err)
	require.Len(t, repo.updates[1], updatesAfterFirst)

	// 2. Subsequent recovered response (remaining=20, used=40/100) clears exhaustion and resumes scheduling.
	payload.Store(`{"mode":"quota_limited","remaining":20,"quota":{"limit":100,"used":40,"remaining":60,"unit":"USD"}}`)
	usage, err = svc.GetUsage(context.Background(), 1)
	require.NoError(t, err)
	require.Equal(t, 20.0, *usage.Balance)
	require.False(t, repo.accounts[1].IsQuotaExceeded())
	require.True(t, repo.accounts[1].IsSchedulable())
	require.Equal(t, false, repo.accounts[1].Extra[APIKeyQuotaExhaustedExtraKey])

	// 3. Dated window exhaustion (5h used=20/20 with future reset_at) sets TempUnschedulableUntil and pauses until reset_at.
	futureReset := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)
	payload.Store(`{"mode":"quota_limited","remaining":20,"rate_limits":[{"window":"5h","limit":20,"used":20,"reset_at":"` + futureReset.Format(time.RFC3339) + `"}]}`)
	_, err = svc.GetUsage(context.Background(), 1)
	require.NoError(t, err)
	require.True(t, repo.accounts[1].IsQuotaExceeded())
	require.False(t, repo.accounts[1].IsSchedulable())
	require.NotNil(t, repo.accounts[1].TempUnschedulableUntil)
	require.True(t, futureReset.Equal(*repo.accounts[1].TempUnschedulableUntil))
	require.Equal(t, apiKeyQuotaTempUnschedulableReasonPrefix, repo.accounts[1].TempUnschedulableReason)

	// Once reset_at is in the past, IsQuotaExceeded and IsSchedulable automatically recover even before a new probe.
	pastReset := time.Now().UTC().Add(-time.Minute)
	repo.accounts[1].Extra[APIKeyQuota5hResetAtExtraKey] = pastReset.Format(time.RFC3339)
	repo.accounts[1].TempUnschedulableUntil = &pastReset
	require.False(t, repo.accounts[1].IsQuotaExceeded())
	require.True(t, repo.accounts[1].IsSchedulable())

	// Early recovery via subsequent usage query before reset_at clears TempUnschedulableUntil.
	repo.accounts[1].Extra[APIKeyQuota5hResetAtExtraKey] = futureReset.Format(time.RFC3339)
	repo.accounts[1].TempUnschedulableUntil = &futureReset
	payload.Store(`{"mode":"quota_limited","remaining":20,"rate_limits":[{"window":"5h","limit":20,"used":17,"reset_at":"` + futureReset.Format(time.RFC3339) + `"}]}`)
	_, err = svc.GetUsage(context.Background(), 1)
	require.NoError(t, err)
	require.Nil(t, repo.accounts[1].TempUnschedulableUntil)
	require.Empty(t, repo.accounts[1].TempUnschedulableReason)
	require.False(t, repo.accounts[1].IsQuotaExceeded())
	require.True(t, repo.accounts[1].IsSchedulable())

	// 4. Sub-100% window utilization (17/20 = 85%) participates in EvaluateAccountSchedulingThreshold.
	decision := EvaluateAccountSchedulingThreshold(repo.accounts[1], map[string]int{PlatformOpenAI: 80}, time.Now().UTC())
	require.True(t, decision.ShouldPause)
	require.Equal(t, "5h", decision.Window)
	require.InDelta(t, 85.0, decision.UsedPercent, 1e-6)
	require.NotNil(t, decision.Until)
	require.True(t, futureReset.Equal(*decision.Until))
}

func TestUpstreamQuotaPartialResponseSources(t *testing.T) {
	reset := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	for _, scripted := range []bool{false, true} {
		for _, source := range []string{"balance", "undated window", "dated window", "legacy unknown"} {
			t.Run(fmt.Sprintf("script=%t/source=%s", scripted, source), func(t *testing.T) {
				var payload atomic.Value
				payload.Store(`{"balance":0}`)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body := payload.Load().(string)
					if scripted {
						_, _ = io.WriteString(w, body)
						return
					}
					var result map[string]any
					require.NoError(t, json.Unmarshal([]byte(body), &result))
					result["mode"] = "unrestricted"
					if session, ok := result["session"]; ok {
						result["mode"] = "quota_limited"
						window := session.(map[string]any)
						window["window"] = "5h"
						result["rate_limits"] = []any{window}
						if balance, ok := result["balance"]; ok {
							result["remaining"] = balance
						}
					}
					require.NoError(t, json.NewEncoder(w).Encode(result))
				}))
				defer server.Close()
				account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
					Credentials: map[string]any{"base_url": server.URL, "api_key": "fixture"}, Extra: map[string]any{}}
				if scripted {
					account.Extra[QuotaScriptExtraKey] = `function queryQuota(ctx){return JSON.parse(http.request({url:ctx.baseUrl}).body)}`
				}
				repo := &quotaAccountRepoFixture{upstreamBillingProbeAccountRepo: &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{1: account}}}
				svc := &AccountUsageService{accountRepo: repo, upstreamHTTP: &quotaHTTPFixture{client: server.Client()},
					upstreamConfig: &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true}}}}
				query := func(body string) {
					payload.Store(body)
					usage, err := svc.GetUsage(context.Background(), 1, true)
					require.NoError(t, err)
					require.Empty(t, usage.Error)
				}
				healthyWindow := `{"session":{"used":10,"limit":100}}`
				switch source {
				case "balance":
					query(`{"balance":0}`)
					query(healthyWindow)
				case "undated window":
					query(`{"session":{"used":100,"limit":100}}`)
					query(`{"balance":10}`)
				case "dated window":
					query(fmt.Sprintf(`{"session":{"used":100,"limit":100,"reset_at":%q}}`, reset.Format(time.RFC3339)))
					query(`{"balance":10}`)
					require.NotNil(t, repo.accounts[1].TempUnschedulableUntil)
					require.Equal(t, reset, *repo.accounts[1].TempUnschedulableUntil)
				case "legacy unknown":
					account.Extra[APIKeyQuotaExhaustedExtraKey] = true
					account.Extra[APIKeyQuota5hUsedPercentExtraKey] = 100.0
					query(healthyWindow)
				}
				if source == "legacy unknown" {
					require.False(t, repo.accounts[1].IsQuotaExceeded(), "do not infer an unreported legacy balance block")
					require.True(t, repo.accounts[1].IsSchedulable())
				} else {
					require.True(t, repo.accounts[1].IsQuotaExceeded(), "an omitted known source must retain exhaustion")
					require.False(t, repo.accounts[1].IsSchedulable())
				}
				if source == "balance" || source == "legacy unknown" {
					query(`{"balance":10}`)
				} else {
					query(healthyWindow)
				}
				require.False(t, repo.accounts[1].IsQuotaExceeded())
				require.True(t, repo.accounts[1].IsSchedulable())
				require.Nil(t, repo.accounts[1].TempUnschedulableUntil)
			})
		}
	}
}

func TestUpstreamQuotaOverlappingRecovery(t *testing.T) {
	for _, scripted := range []bool{false, true} {
		for _, dated := range []bool{false, true} {
			name := fmt.Sprintf("script=%t/dated=%t", scripted, dated)
			t.Run(name, func(t *testing.T) {
				entered := make(chan int, 2)
				release := []chan struct{}{make(chan struct{}), make(chan struct{})}
				var calls atomic.Int64
				reset := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					n := int(calls.Add(1)) - 1
					entered <- n
					<-release[n]
					if dated {
						used := 100
						if n == 1 {
							used = 10
						}
						fmt.Fprintf(w, `{"mode":"quota_limited","rate_limits":[{"window":"5h","used":%d,"limit":100,"reset_at":%q}],"used":%d,"limit":100,"reset_at":%q}`, used, reset, used, reset)
					} else {
						balance := 0
						if n == 1 {
							balance = 10
						}
						fmt.Fprintf(w, `{"mode":"unrestricted","balance":%d}`, balance)
					}
				}))
				defer server.Close()
				account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
					Credentials: map[string]any{"base_url": server.URL, "api_key": "fixture"}, Extra: map[string]any{}}
				if scripted {
					result := "return {balance:r.balance}"
					if dated {
						result = "return {session:{used:r.used,limit:r.limit,reset_at:r.reset_at}}"
					}
					account.Extra[QuotaScriptExtraKey] = `function queryQuota(ctx){const r=JSON.parse(http.request({url:ctx.baseUrl}).body);` + result + `}`
				}
				repo := &quotaAccountRepoFixture{upstreamBillingProbeAccountRepo: &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{1: account}}}
				svc := &AccountUsageService{accountRepo: repo, upstreamHTTP: &quotaHTTPFixture{client: server.Client()},
					upstreamConfig: &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true}}}}
				done := make(chan error, 2)
				query := func() { _, err := svc.GetUsage(context.Background(), 1, true); done <- err }
				go query()
				require.Equal(t, 0, <-entered)
				go query()
				require.Equal(t, 1, <-entered)
				close(release[0])
				require.NoError(t, <-done)
				require.False(t, repo.accounts[1].IsSchedulable())
				close(release[1])
				require.NoError(t, <-done)
				require.False(t, repo.accounts[1].IsQuotaExceeded())
				require.Nil(t, repo.accounts[1].TempUnschedulableUntil)
				require.True(t, repo.accounts[1].IsSchedulable())
			})
		}
	}
}

func TestUpstreamQuotaUnsupportedIdentityRedactsSecrets(t *testing.T) {
	account := &Account{
		ID:       1,
		Platform: "kimi",
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":       "sk-super-secret-live-credential",
			"base_url":      "https://api.moonshot.cn/v1",
			"api_protocol":  "adaptive",
			"api_base_urls": map[string]any{"chat_completions": "https://relay-a.example/v1"},
			"provider_mode": "official",
			"account_mode":  "coding",
		},
	}
	fingerprint := upstreamAPIKeyUsageProbeIdentity(account)
	require.Len(t, fingerprint, 64)
	require.NotContains(t, fingerprint, "sk-super-secret-live-credential")
	require.NotContains(t, fingerprint, "relay-a.example")

	// Adaptive CN routing changes produce a distinct probe identity.
	changed := cloneFixtureAccount(account)
	changed.Credentials["api_base_urls"] = map[string]any{"chat_completions": "https://relay-b.example/v1"}
	require.NotEqual(t, fingerprint, upstreamAPIKeyUsageProbeIdentity(changed))
}

func TestUpstreamQuotaOrdinaryEditKeepsConcurrentState(t *testing.T) {
	for _, exhausted := range []bool{false, true} {
		for _, expiryEdit := range []string{"absent", "unchanged", "changed"} {
			t.Run(fmt.Sprintf("exhausted=%t/expiry=%s", exhausted, expiryEdit), func(t *testing.T) {
				oldExpiry := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
				newExpiry := oldExpiry.Add(time.Hour)
				pause := oldExpiry.Add(2 * time.Hour)
				account := &Account{ID: 1, Name: "old name", Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
					Credentials: map[string]any{"api_key": "fixture"}, ExpiresAt: &oldExpiry,
					Extra: map[string]any{APIKeyQuotaExhaustedExtraKey: !exhausted, APIKeyUsageUnsupportedIdentityExtraKey: "old-memo"}}
				repo := &quotaAccountRepoFixture{upstreamBillingProbeAccountRepo: &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{1: account}}}
				repo.onBeforeUpdate = func() {
					repo.mu.Lock()
					defer repo.mu.Unlock()
					current := repo.accounts[1]
					current.Extra[APIKeyQuotaExhaustedExtraKey] = exhausted
					current.Extra[APIKeyUsageUnsupportedIdentityExtraKey] = "new-memo"
					current.ExpiresAt = &newExpiry
					if exhausted {
						current.TempUnschedulableUntil = &pause
						current.TempUnschedulableReason = apiKeyQuotaTempUnschedulableReasonPrefix
					}
				}
				input := &UpdateAccountInput{Name: "new name", Extra: map[string]any{"note": "new"}}
				if expiryEdit == "changed" {
					value := oldExpiry.Add(4 * time.Hour).Unix()
					input.ExpiresAt = &value
				}
				if expiryEdit == "unchanged" {
					value := oldExpiry.Unix()
					input.ExpiresAt = &value
				}
				updated, err := (&adminServiceImpl{accountRepo: repo}).UpdateAccount(context.Background(), 1, input)
				require.NoError(t, err)
				require.Equal(t, exhausted, updated.Extra[APIKeyQuotaExhaustedExtraKey])
				require.Equal(t, "new-memo", updated.Extra[APIKeyUsageUnsupportedIdentityExtraKey])
				require.Equal(t, "new name", updated.Name)
				if expiryEdit == "changed" {
					require.Equal(t, *input.ExpiresAt, updated.ExpiresAt.Unix())
				} else {
					require.Equal(t, newExpiry, *updated.ExpiresAt)
				}
				if exhausted {
					require.Equal(t, pause, *updated.TempUnschedulableUntil)
				}
			})
		}
	}
}

func TestUpstreamQuotaAdminSingleAndBulkIdentityAndScriptReset(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	futurePause := now.Add(2 * time.Hour)
	otherPause := now.Add(3 * time.Hour)

	seedManagedExtra := func(withScript bool) map[string]any {
		extra := map[string]any{
			UpstreamBillingProbeEnabledExtraKey:    true,
			UpstreamBillingProbeExtraKey:           map[string]any{"status": "ok"},
			APIKeyUsageUnsupportedIdentityExtraKey: "memoized-hash",
			APIKeyQuotaExhaustedExtraKey:           true,
			APIKeyQuota5hUsedPercentExtraKey:       100.0,
			APIKeyQuota5hResetAtExtraKey:           futurePause.Format(time.RFC3339),
			APIKeyQuotaTotalUsedPercentExtraKey:    100.0,
			"custom_note":                          "preserved",
		}
		if withScript {
			extra[QuotaScriptExtraKey] = "function queryQuota(){return {balance: 0}}"
		}
		return extra
	}

	repo := &quotaAccountRepoFixture{
		upstreamBillingProbeAccountRepo: &upstreamBillingProbeAccountRepo{
			accounts: map[int64]*Account{
				1: {
					ID: 1, Name: "acct-1", Platform: "kimi", Type: AccountTypeAPIKey,
					Status: StatusActive, Schedulable: true,
					TempUnschedulableUntil:  &futurePause,
					TempUnschedulableReason: apiKeyQuotaTempUnschedulableReasonPrefix,
					Credentials: map[string]any{
						"api_key":       "key-1",
						"base_url":      "https://relay.example/v1",
						"api_protocol":  "adaptive",
						"api_base_urls": map[string]any{"chat_completions": "https://relay-1.example/v1"},
						"provider_mode": "custom",
						"account_mode":  "coding",
					},
					Extra: seedManagedExtra(true),
				},
				2: {
					ID: 2, Name: "acct-2", Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
					Status: StatusActive, Schedulable: true,
					TempUnschedulableUntil:  &otherPause,
					TempUnschedulableReason: "cn_balance_low",
					Credentials:             map[string]any{"api_key": "key-2", "base_url": "https://relay.example/v1"},
					Extra:                   seedManagedExtra(true),
				},
				3: {
					ID: 3, Name: "acct-3", Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
					Status: StatusActive, Schedulable: true,
					TempUnschedulableUntil:  &otherPause,
					TempUnschedulableReason: "account_scheduling_threshold",
					Credentials:             map[string]any{"api_key": "key-3", "base_url": "https://relay.example/v1"},
					Extra:                   seedManagedExtra(false),
				},
			},
		},
	}
	adminSvc := &adminServiceImpl{accountRepo: repo}

	// 1. Validation failure in UpdateAccount must NOT clear DB TempUnschedulableUntil or Extra.
	invalidMultiplier := -1.0
	_, err := adminSvc.UpdateAccount(context.Background(), 1, &UpdateAccountInput{
		Credentials:    map[string]any{"api_key": "rotated-but-invalid"},
		RateMultiplier: &invalidMultiplier,
	})
	require.Error(t, err)
	require.NotNil(t, repo.accounts[1].TempUnschedulableUntil)
	require.Equal(t, apiKeyQuotaTempUnschedulableReasonPrefix, repo.accounts[1].TempUnschedulableReason)
	require.Equal(t, true, repo.accounts[1].Extra[APIKeyQuotaExhaustedExtraKey])
	require.Equal(t, "key-1", repo.accounts[1].Credentials["api_key"])

	// 2. Single-account quota_script update strips scheduling keys, preserves UpstreamBillingProbeExtraKey
	// and APIKeyUsageUnsupportedIdentityExtraKey, and clears even an in-flight quota pause set before Update.
	repo.accounts[1].TempUnschedulableUntil = nil
	repo.accounts[1].TempUnschedulableReason = ""
	repo.onBeforeUpdate = func() {
		repo.mu.Lock()
		defer repo.mu.Unlock()
		inFlightUntil := futurePause.Add(15 * time.Minute)
		repo.accounts[1].TempUnschedulableUntil = &inFlightUntil
		repo.accounts[1].TempUnschedulableReason = apiKeyQuotaTempUnschedulableReasonPrefix
	}
	updated, err := adminSvc.UpdateAccount(context.Background(), 1, &UpdateAccountInput{
		Extra: map[string]any{
			QuotaScriptExtraKey: "function queryQuota(){return {balance: 99}}",
			"custom_note":       "preserved",
		},
	})
	repo.onBeforeUpdate = nil
	require.NoError(t, err)
	require.Equal(t, "function queryQuota(){return {balance: 99}}", updated.Extra[QuotaScriptExtraKey])
	require.Equal(t, map[string]any{"status": "ok"}, updated.Extra[UpstreamBillingProbeExtraKey])
	require.Equal(t, "memoized-hash", updated.Extra[APIKeyUsageUnsupportedIdentityExtraKey])
	for _, key := range APIKeyQuotaSchedulingExtraKeys {
		require.NotContains(t, updated.Extra, key)
	}
	require.Nil(t, updated.TempUnschedulableUntil)
	require.Empty(t, updated.TempUnschedulableReason)
	require.True(t, updated.IsSchedulable())

	// Clearing quota_script via blank string also clears quota scheduling state while preserving probe snapshot.
	repo.accounts[1].Extra = seedManagedExtra(true)
	repo.accounts[1].TempUnschedulableUntil = &futurePause
	repo.accounts[1].TempUnschedulableReason = apiKeyQuotaTempUnschedulableReasonPrefix
	updated, err = adminSvc.UpdateAccount(context.Background(), 1, &UpdateAccountInput{
		Extra: map[string]any{QuotaScriptExtraKey: "   ", "custom_note": "preserved"},
	})
	require.NoError(t, err)
	require.NotContains(t, updated.Extra, QuotaScriptExtraKey)
	require.Equal(t, map[string]any{"status": "ok"}, updated.Extra[UpstreamBillingProbeExtraKey])
	for _, key := range APIKeyQuotaSchedulingExtraKeys {
		require.NotContains(t, updated.Extra, key)
	}
	require.Nil(t, updated.TempUnschedulableUntil)

	// 3. Single-account adaptive CN routing update clears probe snapshot + all managed keys + quota pause,
	// while non-quota pauses on account 2 remain untouched when credentials rotate.
	repo.accounts[1].Extra = seedManagedExtra(false)
	repo.accounts[1].TempUnschedulableUntil = &futurePause
	repo.accounts[1].TempUnschedulableReason = apiKeyQuotaTempUnschedulableReasonPrefix
	updated, err = adminSvc.UpdateAccount(context.Background(), 1, &UpdateAccountInput{
		Credentials: map[string]any{
			"api_base_urls": map[string]any{"chat_completions": "https://relay-2.example/v1"},
		},
	})
	require.NoError(t, err)
	require.NotContains(t, updated.Extra, UpstreamBillingProbeExtraKey)
	for _, key := range APIKeyQuotaManagedExtraKeys {
		require.NotContains(t, updated.Extra, key)
	}
	require.Nil(t, updated.TempUnschedulableUntil)

	updated2, err := adminSvc.UpdateAccount(context.Background(), 2, &UpdateAccountInput{
		Credentials: map[string]any{"api_key": "key-2-rotated"},
	})
	require.NoError(t, err)
	require.NotContains(t, updated2.Extra, UpstreamBillingProbeExtraKey)
	for _, key := range APIKeyQuotaManagedExtraKeys {
		require.NotContains(t, updated2.Extra, key)
	}
	require.NotNil(t, updated2.TempUnschedulableUntil)
	require.True(t, otherPause.Equal(*updated2.TempUnschedulableUntil))
	require.Equal(t, "cn_balance_low", updated2.TempUnschedulableReason)

	// 4. BulkUpdateAccounts rejects malformed quota_script before writing.
	bulkCountBefore := len(repo.bulkUpdates)
	_, err = adminSvc.BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{
		AccountIDs: []int64{1, 2, 3},
		Extra:      map[string]any{QuotaScriptExtraKey: 42},
	})
	require.Error(t, err)
	require.Equal(t, "INVALID_QUOTA_SCRIPT", infraerrors.Reason(err))
	require.Len(t, repo.bulkUpdates, bulkCountBefore)

	// 5. BulkUpdateAccounts updating quota_script in a mixed batch clears ONLY apikey_quota_ pauses,
	// strips scheduling keys with zero nil residue, and preserves UpstreamBillingProbeExtraKey.
	repo.accounts[1].Extra = seedManagedExtra(true)
	repo.accounts[1].TempUnschedulableUntil = &futurePause
	repo.accounts[1].TempUnschedulableReason = apiKeyQuotaTempUnschedulableReasonPrefix
	repo.accounts[2].Extra = seedManagedExtra(true)
	repo.accounts[3].Extra = seedManagedExtra(false)

	res, err := adminSvc.BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{
		AccountIDs: []int64{1, 2, 3},
		Extra: map[string]any{
			QuotaScriptExtraKey:                    "function queryQuota(){return {balance: 50}}",
			APIKeyQuotaExhaustedExtraKey:           false, // forged managed key must be ignored/cleared
			APIKeyUsageUnsupportedIdentityExtraKey: "forged",
		},
	})
	require.NoError(t, err)
	require.Equal(t, 3, res.Success)
	for _, id := range []int64{1, 2, 3} {
		acc := repo.accounts[id]
		require.Equal(t, "function queryQuota(){return {balance: 50}}", acc.Extra[QuotaScriptExtraKey])
		require.Equal(t, map[string]any{"status": "ok"}, acc.Extra[UpstreamBillingProbeExtraKey])
		require.Equal(t, "memoized-hash", acc.Extra[APIKeyUsageUnsupportedIdentityExtraKey])
		for _, key := range APIKeyQuotaSchedulingExtraKeys {
			_, exists := acc.Extra[key]
			require.False(t, exists, "account %d must not retain %s", id, key)
		}
	}
	require.Nil(t, repo.accounts[1].TempUnschedulableUntil)
	require.Empty(t, repo.accounts[1].TempUnschedulableReason)
	require.NotNil(t, repo.accounts[2].TempUnschedulableUntil)
	require.Equal(t, "cn_balance_low", repo.accounts[2].TempUnschedulableReason)
	require.NotNil(t, repo.accounts[3].TempUnschedulableUntil)
	require.Equal(t, "account_scheduling_threshold", repo.accounts[3].TempUnschedulableReason)

	// Bulk clearing quota_script with blank string removes quota_script without leaving nil residue.
	repo.accounts[1].Extra = seedManagedExtra(true)
	repo.accounts[1].TempUnschedulableUntil = &futurePause
	repo.accounts[1].TempUnschedulableReason = apiKeyQuotaTempUnschedulableReasonPrefix
	_, err = adminSvc.BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{
		AccountIDs: []int64{1, 2},
		Extra:      map[string]any{QuotaScriptExtraKey: "  "},
	})
	require.NoError(t, err)
	_, hasScript := repo.accounts[1].Extra[QuotaScriptExtraKey]
	require.False(t, hasScript)
	require.Nil(t, repo.accounts[1].TempUnschedulableUntil)
	require.Equal(t, "cn_balance_low", repo.accounts[2].TempUnschedulableReason)

	// 6. BulkUpdateAccounts rotating proxy_id or adaptive CN credentials clears probe snapshot + all managed keys + quota pause.
	repo.accounts[1].Extra = seedManagedExtra(false)
	repo.accounts[1].TempUnschedulableUntil = &futurePause
	repo.accounts[1].TempUnschedulableReason = apiKeyQuotaTempUnschedulableReasonPrefix
	newProxyID := int64(15)
	_, err = adminSvc.BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{
		AccountIDs:  []int64{1, 2},
		ProxyID:     &newProxyID,
		Credentials: map[string]any{"account_mode": "standard"},
	})
	require.NoError(t, err)
	for _, id := range []int64{1, 2} {
		acc := repo.accounts[id]
		require.NotNil(t, acc.ProxyID)
		require.Equal(t, int64(15), *acc.ProxyID)
		require.Equal(t, "standard", acc.Credentials["account_mode"])
		require.NotContains(t, acc.Extra, UpstreamBillingProbeExtraKey)
		for _, key := range APIKeyQuotaManagedExtraKeys {
			_, exists := acc.Extra[key]
			require.False(t, exists, "account %d must not retain %s", id, key)
		}
	}
	require.Nil(t, repo.accounts[1].TempUnschedulableUntil)
	require.Equal(t, "cn_balance_low", repo.accounts[2].TempUnschedulableReason)
}

func TestUpstreamQuotaInFlightIdentityCASRejection(t *testing.T) {
	futureReset := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)
	reportedExpiry := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)

	var onRequest atomic.Value
	onRequest.Store(func() {})
	var responseStatus atomic.Int64
	responseStatus.Store(http.StatusOK)
	var responseBody atomic.Value
	responseBody.Store(`{"mode":"quota_limited","remaining":0,"expires_at":"2026-12-31T00:00:00Z","rate_limits":[{"window":"5h","limit":20,"used":20,"reset_at":"` + futureReset.Format(time.RFC3339) + `"}]}`)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		onRequest.Load().(func())()
		w.Header().Set("Content-Type", "application/json")
		status := int(responseStatus.Load())
		w.WriteHeader(status)
		if status >= 200 && status < 300 {
			_, _ = io.WriteString(w, responseBody.Load().(string))
		}
	}))
	defer server.Close()

	cfg := &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true}}}
	httpClient := &quotaHTTPFixture{client: server.Client()}

	// 1. Fail-closed when repository test double does not implement apiKeyQuotaStateWriter.
	plainRepo := &upstreamBillingProbeAccountRepo{
		accounts: map[int64]*Account{
			1: {
				ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
				Status: StatusActive, Schedulable: true,
				Credentials: map[string]any{"base_url": server.URL + "/v1", "api_key": "plain-key"},
				Extra:       map[string]any{},
			},
		},
	}
	plainSvc := &AccountUsageService{accountRepo: plainRepo, upstreamHTTP: httpClient, upstreamConfig: cfg}
	callerCopy := cloneFixtureAccount(plainRepo.accounts[1])
	usage, err := plainSvc.GetUsageForAccount(context.Background(), callerCopy)
	require.NoError(t, err)
	require.NotNil(t, usage.Balance)
	require.Empty(t, callerCopy.Extra)
	require.Nil(t, callerCopy.ExpiresAt)
	require.Nil(t, callerCopy.TempUnschedulableUntil)
	require.Empty(t, plainRepo.accounts[1].Extra)

	// 2. /v1/usage in-flight api_key rotation rejects stale exhausted scheduling, ExpiresAt, and unsupported-clear writes.
	proxyID := int64(9)
	repo := &quotaAccountRepoFixture{
		upstreamBillingProbeAccountRepo: &upstreamBillingProbeAccountRepo{
			accounts: map[int64]*Account{
				1: {
					ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
					Status: StatusActive, Schedulable: true,
					ProxyID: &proxyID,
					Proxy: &Proxy{
						ID: proxyID, Protocol: "http", Host: "127.0.0.1", Port: 8080, Status: StatusActive,
					},
					Credentials: map[string]any{"base_url": server.URL + "/v1", "api_key": "old-key"},
					Extra:       map[string]any{APIKeyUsageUnsupportedIdentityExtraKey: "stale-memo"},
				},
			},
		},
		proxies: map[int64]*Proxy{
			proxyID: {ID: proxyID, Protocol: "http", Host: "127.0.0.1", Port: 8080, Status: StatusActive},
		},
	}
	svc := &AccountUsageService{accountRepo: repo, upstreamHTTP: httpClient, upstreamConfig: cfg}
	loadedBeforeProbe, err := repo.GetByID(context.Background(), 1)
	require.NoError(t, err)

	onRequest.Store(func() {
		repo.mu.Lock()
		defer repo.mu.Unlock()
		repo.accounts[1].Credentials["api_key"] = "fresh-rotated-key"
		delete(repo.accounts[1].Extra, APIKeyUsageUnsupportedIdentityExtraKey)
	})
	usage, err = svc.GetUsageForAccount(context.Background(), loadedBeforeProbe, true)
	require.NoError(t, err)
	require.Zero(t, *usage.Balance)
	require.Equal(t, "fresh-rotated-key", repo.accounts[1].Credentials["api_key"])
	require.Empty(t, repo.accounts[1].Extra)
	require.Nil(t, repo.accounts[1].ExpiresAt)
	require.Nil(t, repo.accounts[1].TempUnschedulableUntil)
	require.True(t, repo.accounts[1].IsSchedulable())
	// Caller's in-memory *Account must also remain untouched when CAS returns applied=false.
	require.Nil(t, loadedBeforeProbe.ExpiresAt)
	require.False(t, loadedBeforeProbe.ExpiresAt != nil && loadedBeforeProbe.ExpiresAt.Equal(reportedExpiry))
	require.Nil(t, loadedBeforeProbe.TempUnschedulableUntil)
	require.NotContains(t, loadedBeforeProbe.Extra, APIKeyQuotaExhaustedExtraKey)
	require.Equal(t, "stale-memo", loadedBeforeProbe.Extra[APIKeyUsageUnsupportedIdentityExtraKey])

	// 3. /v1/usage in-flight hydrated Proxy host change rejects stale 404 unsupported memoization.
	responseStatus.Store(http.StatusNotFound)
	onRequest.Store(func() {
		repo.mu.Lock()
		defer repo.mu.Unlock()
		repo.proxies[proxyID].Host = "10.0.0.2"
	})
	_, err = svc.GetUsage(context.Background(), 1)
	require.NoError(t, err)
	require.NotContains(t, repo.accounts[1].Extra, APIKeyUsageUnsupportedIdentityExtraKey)

	// Align stored account Proxy with updated proxy table and test in-flight quota_script addition during /v1/usage.
	repo.accounts[1].Proxy.Host = "10.0.0.2"
	responseStatus.Store(http.StatusOK)
	onRequest.Store(func() {
		repo.mu.Lock()
		defer repo.mu.Unlock()
		repo.accounts[1].Extra[QuotaScriptExtraKey] = "function queryQuota(){return {balance: 100}}"
	})
	_, err = svc.GetUsage(context.Background(), 1)
	require.NoError(t, err)
	require.NotContains(t, repo.accounts[1].Extra, APIKeyQuotaExhaustedExtraKey)
	require.Nil(t, repo.accounts[1].TempUnschedulableUntil)

	// 4. /v1/sub2api/billing (ProbeAccount) in-flight api_key rotation rejects stale unsupported memoization and clear.
	delete(repo.accounts[1].Extra, QuotaScriptExtraKey)
	repo.accounts[1].Concurrency = 1
	repo.accounts[1].Extra[UpstreamBillingProbeEnabledExtraKey] = true
	settingsRepo := &upstreamBillingProbeSettingRepo{values: map[string]string{
		SettingKeyUpstreamBillingProbeSettings: `{"enabled":true,"interval_minutes":30}`,
	}}
	billingSvc := newUpstreamBillingProbeTestService(repo, httpClient, settingsRepo)

	responseStatus.Store(http.StatusNotFound)
	onRequest.Store(func() {
		repo.mu.Lock()
		defer repo.mu.Unlock()
		repo.accounts[1].Credentials["api_key"] = "billing-rotated-key-1"
	})
	_, err = billingSvc.ProbeAccount(context.Background(), 1)
	require.ErrorIs(t, err, ErrUpstreamBillingProbeIdentityChanged)
	require.NotContains(t, repo.accounts[1].Extra, APIKeyUsageUnsupportedIdentityExtraKey)

	// Also test ProbeAccount 200 OK recovery when api_key rotates in flight: stale clear must not write "" over new state.
	repo.accounts[1].Extra[APIKeyUsageUnsupportedIdentityExtraKey] = "preexisting-old-hash"
	responseStatus.Store(http.StatusOK)
	responseBody.Store(`{"object":"sub2api.key_billing","schema_version":1,"billing_scope":"token","group_rate_multiplier":1,"resolved_rate_multiplier":1,"peak_rate_enabled":false,"effective_rate_multiplier":1,"observed_at":"2026-10-26T00:00:00Z"}`)
	onRequest.Store(func() {
		repo.mu.Lock()
		defer repo.mu.Unlock()
		repo.accounts[1].Credentials["api_key"] = "billing-rotated-key-2"
		delete(repo.accounts[1].Extra, APIKeyUsageUnsupportedIdentityExtraKey)
	})
	_, err = billingSvc.ProbeAccount(context.Background(), 1)
	require.ErrorIs(t, err, ErrUpstreamBillingProbeIdentityChanged)
	require.NotContains(t, repo.accounts[1].Extra, APIKeyUsageUnsupportedIdentityExtraKey)
}
