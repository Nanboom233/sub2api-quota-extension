// sub2api-package: backend/internal/repository
package repository

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
)

func TestUpdateAPIKeyQuotaStateSQLCASAndOutbox(t *testing.T) {
	proxyID := int64(9)
	futureReset := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	expiresAt := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)

	t.Run("scheduling mutation commits CAS update and outbox atomically", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		require.NoError(t, err)
		t.Cleanup(func() { _ = db.Close() })
		client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
		t.Cleanup(func() { _ = client.Close() })

		mock.ExpectBegin()
		mock.ExpectQuery(`(?s)` + regexp.QuoteMeta("SELECT protocol, host, port") + `.*` + regexp.QuoteMeta("FOR SHARE")).
			WithArgs(proxyID).
			WillReturnRows(sqlmock.NewRows([]string{"protocol", "host", "port", "username", "password", "status"}).
				AddRow("http", "127.0.0.1", 3128, "user", "pass", service.StatusActive))
		mock.ExpectQuery(`(?s)SELECT extra FROM accounts.*FOR NO KEY UPDATE`).
			WillReturnRows(sqlmock.NewRows([]string{"extra"}).AddRow(`{}`))
		payload, err := json.Marshal(map[string]any{
			service.APIKeyQuota5hUsedPercentExtraKey:    100.0,
			service.APIKeyQuota5hResetAtExtraKey:        futureReset.Format(time.RFC3339),
			service.APIKeyQuotaBalanceExhaustedExtraKey: false,
			service.APIKeyQuotaExhaustedExtraKey:        false,
		})
		require.NoError(t, err)
		mock.ExpectExec(`(?s)`+regexp.QuoteMeta("UPDATE accounts SET extra = COALESCE(extra, '{}'::jsonb) || $1::jsonb, expires_at = $2")+`.*`+regexp.QuoteMeta("temp_unschedulable_until = CASE WHEN status = 'active' AND schedulable = TRUE")+`.*`+regexp.QuoteMeta("updated_at = CASE WHEN expires_at IS DISTINCT FROM $2 THEN NOW() ELSE updated_at END WHERE id = $5 AND deleted_at IS NULL AND platform = $6 AND type = $7 AND COALESCE(credentials, '{}'::jsonb) = $8::jsonb AND proxy_id IS NOT DISTINCT FROM $9 AND CASE WHEN BTRIM(COALESCE(extra ->> 'quota_script', '')) = '' THEN '' ELSE COALESCE(extra ->> 'quota_script', '') END = $10")+`.*`+regexp.QuoteMeta("IS DISTINCT FROM COALESCE(extra, '{}'::jsonb)")).
			WithArgs(
				string(payload),
				expiresAt,
				futureReset,
				"apikey_quota_window_exhausted",
				int64(17),
				service.PlatformOpenAI,
				service.AccountTypeAPIKey,
				`{"api_key":"sk-test","base_url":"http://127.0.0.1:8080"}`,
				proxyID,
				"function queryQuota(){return {}}",
			).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).
			WithArgs(service.SchedulerOutboxEventAccountChanged, int64(17), nil, nil, sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(1, 1))
		mock.ExpectCommit()

		repo := newAccountRepositoryWithSQL(client, &recordingSQLExecutor{err: errors.New("must use tx client")}, nil)
		account := &service.Account{
			ID:       17,
			Platform: service.PlatformOpenAI,
			Type:     service.AccountTypeAPIKey,
			Credentials: map[string]any{
				"api_key":  "sk-test",
				"base_url": "http://127.0.0.1:8080",
			},
			ProxyID: &proxyID,
			Proxy: &service.Proxy{
				ID: proxyID, Protocol: "http", Host: "127.0.0.1", Port: 3128,
				Username: "user", Password: "pass", Status: service.StatusActive,
			},
			Extra: map[string]any{
				service.QuotaScriptExtraKey: "function queryQuota(){return {}}",
			},
		}
		applied, err := repo.UpdateAPIKeyQuotaState(context.Background(), account,
			&service.UsageInfo{FiveHour: &service.UsageProgress{Utilization: 100, ResetsAt: &futureReset}}, &expiresAt, nil)
		require.NoError(t, err)
		require.True(t, applied)
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("scheduler-neutral unsupported memoization skips outbox and preserves updated_at", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		require.NoError(t, err)
		t.Cleanup(func() { _ = db.Close() })
		client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
		t.Cleanup(func() { _ = client.Close() })

		mock.ExpectBegin()
		mock.ExpectExec(`(?s)`+regexp.QuoteMeta("UPDATE accounts SET extra = COALESCE(extra, '{}'::jsonb) || $1::jsonb WHERE id = $2")).
			WithArgs(`{"apikey_usage_unsupported_identity":"hash-1"}`, int64(21), service.PlatformOpenAI, service.AccountTypeAPIKey, `{"api_key":"sk-memo"}`, nil, "").
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectCommit()

		repo := newAccountRepositoryWithSQL(client, db, nil)
		applied, err := repo.UpdateAPIKeyQuotaState(context.Background(), &service.Account{
			ID:          21,
			Platform:    service.PlatformOpenAI,
			Type:        service.AccountTypeAPIKey,
			Credentials: map[string]any{"api_key": "sk-memo"},
			Extra:       map[string]any{service.QuotaScriptExtraKey: "   "},
		}, nil, nil, stringPointer("hash-1"))
		require.NoError(t, err)
		require.True(t, applied)
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("proxy identity mismatch or zero rows affected returns false without outbox", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		require.NoError(t, err)
		t.Cleanup(func() { _ = db.Close() })
		client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
		t.Cleanup(func() { _ = client.Close() })

		// First call: proxy host changed in DB -> rolls back without UPDATE.
		mock.ExpectBegin()
		mock.ExpectQuery(`(?s)` + regexp.QuoteMeta("SELECT protocol, host, port") + `.*` + regexp.QuoteMeta("FOR SHARE")).
			WithArgs(proxyID).
			WillReturnRows(sqlmock.NewRows([]string{"protocol", "host", "port", "username", "password", "status"}).
				AddRow("http", "rotated.example", 3128, "user", "pass", service.StatusActive))
		mock.ExpectRollback()

		repo := newAccountRepositoryWithSQL(client, db, nil)
		applied, err := repo.UpdateAPIKeyQuotaState(context.Background(), &service.Account{
			ID: 22, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
			Credentials: map[string]any{"api_key": "sk-stale"},
			ProxyID:     &proxyID,
			Proxy:       &service.Proxy{ID: proxyID, Protocol: "http", Host: "old.example", Port: 3128, Username: "user", Password: "pass", Status: service.StatusActive},
		}, nil, &expiresAt, nil)
		require.NoError(t, err)
		require.False(t, applied)

		require.NoError(t, mock.ExpectationsWereMet())
	})
}

func stringPointer(value string) *string { return &value }

func TestQuotaObservationMergesLockedSources(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	reset := now.Add(time.Hour)
	healthy := 10.0
	for _, source := range []string{"balance", "undated window", "dated window", "legacy unknown", "window recovery", "identity conflict", "commit failure", "no change"} {
		t.Run(source, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
			t.Cleanup(func() { _ = client.Close() })
			account := &service.Account{ID: 31, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
				Credentials: map[string]any{"api_key": "fixture"}, Extra: map[string]any{"unrelated": "old snapshot"}}
			usage := &service.UsageInfo{Balance: &healthy}
			current := map[string]any{service.APIKeyQuotaBalanceExhaustedExtraKey: false, service.APIKeyQuotaExhaustedExtraKey: true,
				service.APIKeyQuota5hUsedPercentExtraKey: 100.0, service.APIKeyQuota5hResetAtExtraKey: "", "unrelated": "current"}
			want := map[string]any{service.APIKeyQuotaBalanceExhaustedExtraKey: false, service.APIKeyQuotaExhaustedExtraKey: true,
				service.APIKeyQuota5hUsedPercentExtraKey: 100.0, service.APIKeyQuota5hResetAtExtraKey: ""}
			if source == "balance" || source == "legacy unknown" || source == "window recovery" {
				usage = &service.UsageInfo{FiveHour: &service.UsageProgress{Utilization: 10}}
				want[service.APIKeyQuota5hUsedPercentExtraKey] = 10.0
				if source == "window recovery" {
					want[service.APIKeyQuotaExhaustedExtraKey] = false
				} else {
					want[service.APIKeyQuotaBalanceExhaustedExtraKey] = true
					current[service.APIKeyQuotaBalanceExhaustedExtraKey] = true
					if source == "legacy unknown" {
						delete(current, service.APIKeyQuotaBalanceExhaustedExtraKey)
						want[service.APIKeyQuotaBalanceExhaustedExtraKey] = false
						want[service.APIKeyQuotaExhaustedExtraKey] = false
					}
				}
			}
			if source == "dated window" {
				current[service.APIKeyQuota5hResetAtExtraKey] = reset.Format(time.RFC3339)
				want[service.APIKeyQuota5hResetAtExtraKey] = reset.Format(time.RFC3339)
				want[service.APIKeyQuotaExhaustedExtraKey] = false
			}
			if source == "commit failure" {
				current[service.APIKeyQuotaBalanceExhaustedExtraKey] = true
			}
			mock.ExpectBegin()
			query := mock.ExpectQuery(`(?s)SELECT extra FROM accounts.*COALESCE\(credentials.*FOR NO KEY UPDATE`).
				WithArgs(int64(31), service.PlatformOpenAI, service.AccountTypeAPIKey, `{"api_key":"fixture"}`, nil, "")
			rows := sqlmock.NewRows([]string{"extra"})
			if source != "identity conflict" {
				body, err := json.Marshal(current)
				require.NoError(t, err)
				rows.AddRow(body)
			}
			query.WillReturnRows(rows)
			if source == "identity conflict" {
				mock.ExpectRollback()
			} else {
				body, err := json.Marshal(want)
				require.NoError(t, err)
				update := mock.ExpectExec(`(?s)UPDATE accounts SET extra = .*IS DISTINCT FROM.*`)
				if source == "dated window" {
					update.WithArgs(string(body), reset, "apikey_quota_window_exhausted", int64(31), service.PlatformOpenAI, service.AccountTypeAPIKey, `{"api_key":"fixture"}`, nil, "")
				} else {
					update.WithArgs(string(body), int64(31), service.PlatformOpenAI, service.AccountTypeAPIKey, `{"api_key":"fixture"}`, nil, "")
				}
				if source == "no change" {
					update.WillReturnResult(sqlmock.NewResult(0, 0))
					mock.ExpectRollback()
				} else {
					update.WillReturnResult(sqlmock.NewResult(0, 1))
					mock.ExpectExec("INSERT INTO scheduler_outbox").WillReturnResult(sqlmock.NewResult(1, 1))
					if source == "commit failure" {
						mock.ExpectCommit().WillReturnError(errors.New("commit failed"))
					} else {
						mock.ExpectCommit()
					}
				}
			}
			repo := newAccountRepositoryWithSQL(client, db, nil)
			applied, err := repo.UpdateAPIKeyQuotaState(context.Background(), account, usage, nil, nil)
			if source == "commit failure" {
				require.ErrorContains(t, err, "commit failed")
				require.False(t, applied)
			} else {
				require.NoError(t, err)
				require.Equal(t, source != "identity conflict" && source != "no change", applied)
				if applied {
					for key, value := range want {
						require.Equal(t, value, account.Extra[key])
					}
				}
			}
			if !applied {
				require.Equal(t, map[string]any{"unrelated": "old snapshot"}, account.Extra)
				require.Nil(t, account.TempUnschedulableUntil)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestQuotaBalanceSourceCacheAndReset(t *testing.T) {
	extra := filterSchedulerExtra(map[string]any{service.APIKeyQuotaBalanceExhaustedExtraKey: true})
	require.Equal(t, true, extra[service.APIKeyQuotaBalanceExhaustedExtraKey])
	exec := &recordingSQLExecutor{result: rowsAffectedResult(1)}
	repo := &accountRepository{sql: exec}
	require.NoError(t, repo.ResetQuotaUsedAndClearRateLimitCooldown(context.Background(), 19))
	require.Contains(t, normalizeSQLWhitespace(exec.execQueries[0]), "- 'apikey_quota_balance_exhausted'")
}

func TestQuotaAccountEditUsesNativeTransaction(t *testing.T) {
	for _, outcome := range []string{"exhausted", "recovered", "identity reset", "script reset", "explicit expiry", "clear expiry", "native expiry", "outbox failure"} {
		t.Run(outcome, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
			t.Cleanup(func() { _ = client.Close() })
			oldExpiry := time.Now().UTC().Truncate(time.Second)
			newExpiry, pause := oldExpiry.Add(time.Hour), oldExpiry.Add(2*time.Hour)
			account := &service.Account{ID: 27, Name: "edited", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
				Credentials: map[string]any{"api_key": "fixture"}, Extra: map[string]any{
					service.APIKeyQuotaExhaustedExtraKey: true, service.APIKeyUsageUnsupportedIdentityExtraKey: "old", "note": "edited"},
				ExpiresAt: &oldExpiry, Status: service.StatusActive, Schedulable: true, Concurrency: 1, Priority: 1}
			identityUnchanged := outcome != "identity reset"
			quotaPause := outcome == "identity reset" || outcome == "script reset"
			if outcome == "script reset" {
				account.Extra[service.QuotaScriptExtraKey] = "new script"
			}
			if outcome == "explicit expiry" {
				account.ExpiresAt = &pause
			}
			if outcome == "clear expiry" {
				account.ExpiresAt = nil
			}
			current := map[string]any{service.APIKeyQuotaExhaustedExtraKey: outcome == "exhausted", service.APIKeyUsageUnsupportedIdentityExtraKey: "new"}
			body, err := json.Marshal(current)
			require.NoError(t, err)
			reason := "non_quota_pause"
			if quotaPause {
				reason = "apikey_quota_window_exhausted"
			}
			mock.ExpectBegin()
			mock.ExpectQuery(`(?s)SELECT.*extra,.*expires_at,.*temp_unschedulable_until,.*temp_unschedulable_reason.*FOR NO KEY UPDATE`).
				WithArgs(int64(27), service.PlatformOpenAI, service.AccountTypeAPIKey, `{"api_key":"fixture"}`, nil).
				WillReturnRows(sqlmock.NewRows([]string{"identity", "ollama", "proxy", "enabled", "sync", "snapshot", "session", "auto", "usage", "opencode", "go_auto", "go_usage", "models", "metadata", "extra", "expires_at", "paused_until", "paused_reason"}).
					AddRow(identityUnchanged, false, true, nil, nil, nil, nil, nil, nil, false, nil, nil, nil, nil, body, newExpiry, pause, reason))
			mock.ExpectExec(`(?s)UPDATE .*accounts.*SET.*WHERE .*id.*`).WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectQuery(`(?s)SELECT .* FROM "accounts" WHERE "id" = \$1`).WithArgs(int64(27)).WillReturnRows(updatedAccountRows(27, `{}`))
			outbox := mock.ExpectExec("INSERT INTO scheduler_outbox")
			if outcome == "outbox failure" {
				outbox.WillReturnError(errors.New("outbox failed"))
				mock.ExpectRollback()
			} else {
				outbox.WillReturnResult(sqlmock.NewResult(1, 1))
				mock.ExpectCommit()
			}
			repo := newAccountRepositoryWithSQL(client, db, nil)
			if outcome == "native expiry" {
				err = repo.UpdateWithAccountBillingSettings(context.Background(), account, nil, nil, nil)
			} else {
				err = repo.UpdateWithAccountBillingSettings(context.Background(), account, nil, nil, nil, &oldExpiry)
			}
			if outcome == "outbox failure" {
				require.ErrorContains(t, err, "outbox failed")
			} else {
				require.NoError(t, err)
			}
			if outcome == "native expiry" {
				require.Equal(t, true, account.Extra[service.APIKeyQuotaExhaustedExtraKey])
				require.Nil(t, account.TempUnschedulableUntil)
			} else if quotaPause {
				require.NotContains(t, account.Extra, service.APIKeyQuotaExhaustedExtraKey)
				require.Nil(t, account.TempUnschedulableUntil)
			} else {
				require.Equal(t, outcome == "exhausted", account.Extra[service.APIKeyQuotaExhaustedExtraKey])
				require.Equal(t, pause, *account.TempUnschedulableUntil)
				require.Equal(t, "non_quota_pause", account.TempUnschedulableReason)
			}
			if outcome == "native expiry" {
				require.Equal(t, "old", account.Extra[service.APIKeyUsageUnsupportedIdentityExtraKey])
			} else if identityUnchanged {
				require.Equal(t, "new", account.Extra[service.APIKeyUsageUnsupportedIdentityExtraKey])
			} else {
				require.NotContains(t, account.Extra, service.APIKeyUsageUnsupportedIdentityExtraKey)
			}
			switch outcome {
			case "explicit expiry":
				require.Equal(t, pause, *account.ExpiresAt)
			case "clear expiry":
				require.Nil(t, account.ExpiresAt)
			case "native expiry":
				require.Equal(t, oldExpiry, *account.ExpiresAt)
			default:
				require.Equal(t, newExpiry, *account.ExpiresAt)
			}
			require.Equal(t, "edited", account.Extra["note"])
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestUpdateExtraAndBulkUpdateStripNilQuotaKeysAndConditionalPause(t *testing.T) {
	t.Run("UpdateExtra strips nil quota_script and managed keys without leaving JSON null", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		require.NoError(t, err)
		t.Cleanup(func() { _ = db.Close() })
		client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
		t.Cleanup(func() { _ = client.Close() })

		mock.ExpectBegin()
		mock.ExpectExec(`(?s)UPDATE accounts SET extra = \(COALESCE\(extra, '\{\}'::jsonb\) \|\| \$1::jsonb\) - 'quota_script' - 'apikey_usage_unsupported_identity' - 'apikey_quota_exhausted'`).
			WithArgs(sqlmock.AnyArg(), int64(27)).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).
			WithArgs(service.SchedulerOutboxEventAccountChanged, int64(27), nil, nil, sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(1, 1))
		mock.ExpectCommit()

		repo := newAccountRepositoryWithSQL(client, db, nil)
		err = repo.UpdateExtra(context.Background(), 27, map[string]any{
			service.QuotaScriptExtraKey:                    nil,
			service.APIKeyUsageUnsupportedIdentityExtraKey: nil,
			service.APIKeyQuotaExhaustedExtraKey:           nil,
		})
		require.NoError(t, err)
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("BulkUpdate strips nil quota keys and conditionally clears only apikey_quota_ pauses", func(t *testing.T) {
		exec := &recordingSQLExecutor{result: rowsAffectedResult(2)}
		repo := newAccountRepositoryWithSQL(nil, exec, nil)

		extra := map[string]any{
			service.UpstreamBillingProbeExtraKey: nil,
			service.QuotaScriptExtraKey:          nil,
		}
		for _, key := range service.APIKeyQuotaManagedExtraKeys {
			extra[key] = nil
		}
		rows, err := repo.BulkUpdate(context.Background(), []int64{10, 11}, service.AccountBulkUpdate{
			Credentials: map[string]any{"api_key": "rotated"},
			Extra:       extra,
		})
		require.NoError(t, err)
		require.EqualValues(t, 2, rows)
		require.Len(t, exec.execQueries, 2)

		sqlText := normalizeSQLWhitespace(exec.execQueries[0])
		require.Contains(t, sqlText, "- 'upstream_billing_probe'")
		require.Contains(t, sqlText, "- 'quota_script'")
		for _, key := range service.APIKeyQuotaManagedExtraKeys {
			require.Contains(t, sqlText, "- '"+key+"'")
		}
		require.Contains(t, sqlText, "temp_unschedulable_until = CASE WHEN temp_unschedulable_reason LIKE 'apikey_quota_%' THEN NULL ELSE temp_unschedulable_until END")
		require.Contains(t, sqlText, "temp_unschedulable_reason = CASE WHEN temp_unschedulable_reason LIKE 'apikey_quota_%' THEN NULL ELSE temp_unschedulable_reason END")
		require.Contains(t, sqlText, "WHERE id = ANY($3) AND deleted_at IS NULL")
		require.Len(t, exec.execArgs[0], 3)
		require.True(t, strings.Contains(normalizeSQLWhitespace(exec.execQueries[1]), "INSERT INTO scheduler_outbox"))
	})
}
