package oauth

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/crypto"
	"github.com/saker-ai/ctxhub/internal/observability"
)

// newTestStore returns a Store backed by an in-memory SQLite DB and a
// real envelope-encryption Encryptor. The Encryptor uses a fixed
// passphrase so ciphertexts are deterministic for assertions.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	return newTestStoreWithDSN(t, ":memory:")
}

func newTestStoreWithDSN(t *testing.T, dsn string) *Store {
	t.Helper()
	enc, err := crypto.New(crypto.Config{
		Provider: "local",
		Local: crypto.LocalConfig{
			Passphrase: "test-passphrase-do-not-use-in-prod",
		},
	})
	require.NoError(t, err)
	s, err := NewStore(StoreConfig{
		DSN:       dsn,
		Encryptor: enc,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// newTestStoreWithMetrics returns a store wired to an isolated
// Prometheus registry so the test can read oauthTokenRefreshTotal.
func newTestStoreWithMetrics(t *testing.T) (*Store, *observability.Metrics) {
	t.Helper()
	enc, err := crypto.New(crypto.Config{
		Provider: "local",
		Local: crypto.LocalConfig{
			Passphrase: "test-passphrase-do-not-use-in-prod",
		},
	})
	require.NoError(t, err)
	reg := prometheus.NewRegistry()
	metrics := observability.NewMetricsFor(reg)
	s, err := NewStore(StoreConfig{
		DSN:       ":memory:",
		Encryptor: enc,
		Metrics:   metrics,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s, metrics
}

func TestStore_SaveAndLoadToken(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	tok := &Token{
		AccessToken:  "at-1",
		RefreshToken: "rt-1",
		TokenType:    "Bearer",
		ExpiresIn:    3600,
		UserID:       "uid-1",
		Subject:      "Alice",
		Scope:        "openid email",
	}
	require.NoError(t, s.SaveToken(ctx, ProviderFeishu, "acct-1", tok))

	stored, err := s.LoadTokenByRefresh(ctx, ProviderFeishu, "rt-1")
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, "at-1", stored.AccessToken)
	assert.Equal(t, "rt-1", stored.RefreshToken)
	assert.Equal(t, "acct-1", stored.AccountID)
	assert.Equal(t, "uid-1", stored.UserID)
	assert.Equal(t, "Alice", stored.Subject)
	assert.Equal(t, "openid email", stored.Scopes)
	assert.Equal(t, ProviderFeishu, stored.Provider)
}

func TestStore_LoadTokenByRefresh_Unknown(t *testing.T) {
	s := newTestStore(t)
	stored, err := s.LoadTokenByRefresh(context.Background(), ProviderFeishu, "nope")
	require.NoError(t, err)
	assert.Nil(t, stored)
}

func TestStore_TokensAreEncryptedAtRest(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	tok := &Token{
		AccessToken:  "secret-access-token",
		RefreshToken: "secret-refresh-token",
		TokenType:    "Bearer",
		ExpiresIn:    3600,
		UserID:       "uid-1",
	}
	require.NoError(t, s.SaveToken(ctx, ProviderGoogle, "acct-1", tok))

	// Read the raw ciphertext columns from the DB. They must NOT
	// contain the plaintext tokens.
	s.mu.RLock()
	row := s.db.QueryRowContext(ctx, `
        SELECT access_token_ciphertext, refresh_token_ciphertext
        FROM oauth_provider_tokens
        WHERE provider = ? AND account_id = ?`, ProviderGoogle, "acct-1")
	var encAccess, encRefresh string
	require.NoError(t, row.Scan(&encAccess, &encRefresh))
	s.mu.RUnlock()

	assert.NotContains(t, encAccess, "secret-access-token")
	assert.NotContains(t, encRefresh, "secret-refresh-token")
	assert.NotEqual(t, "secret-access-token", encAccess)
	assert.NotEqual(t, "secret-refresh-token", encRefresh)
	// Ciphertexts are JSON envelopes with the provider field set.
	assert.Contains(t, encAccess, `"provider":"local"`)
	assert.Contains(t, encRefresh, `"provider":"local"`)
}

func TestStore_PersistsAcrossInstances(t *testing.T) {
	// Use a temp file so the second store instance sees the same DB.
	dsn := filepath.Join(t.TempDir(), "oauth.db")
	ctx := context.Background()

	s1 := newTestStoreWithDSN(t, dsn)
	tok := &Token{
		AccessToken:  "at-persist",
		RefreshToken: "rt-persist",
		TokenType:    "Bearer",
		ExpiresIn:    3600,
		UserID:       "uid-p",
	}
	require.NoError(t, s1.SaveToken(ctx, ProviderFeishu, "acct-p", tok))
	// Close s1 so the WAL is flushed.
	require.NoError(t, s1.Close())

	// Open a second store at the same DSN. The token must survive.
	s2 := newTestStoreWithDSN(t, dsn)
	stored, err := s2.LoadTokenByRefresh(ctx, ProviderFeishu, "rt-persist")
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, "at-persist", stored.AccessToken)
	assert.Equal(t, "rt-persist", stored.RefreshToken)
	assert.Equal(t, "acct-p", stored.AccountID)
}

func TestStore_RefreshTokenRotation_OldInvalid(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Initial token pair.
	tok1 := &Token{
		AccessToken:  "at-1",
		RefreshToken: "rt-1",
		TokenType:    "Bearer",
		ExpiresIn:    3600,
		UserID:       "uid-1",
	}
	require.NoError(t, s.SaveToken(ctx, ProviderFeishu, "acct-1", tok1))

	// Refresh: save a new pair with rt-2.
	tok2 := &Token{
		AccessToken:  "at-2",
		RefreshToken: "rt-2",
		TokenType:    "Bearer",
		ExpiresIn:    3600,
		UserID:       "uid-1",
	}
	require.NoError(t, s.SaveToken(ctx, ProviderFeishu, "acct-1", tok2))

	// rt-1 should no longer match the current refresh_token_hash.
	stored, err := s.LoadTokenByRefresh(ctx, ProviderFeishu, "rt-1")
	require.NoError(t, err)
	assert.Nil(t, stored, "old refresh token must not load after rotation")

	// But rt-1 should match the previous_refresh_token_hash, so a
	// replay can be detected.
	isReplay, err := s.IsPreviousRefreshToken(ctx, ProviderFeishu, "rt-1")
	require.NoError(t, err)
	assert.True(t, isReplay, "old refresh token should be flagged as replay")

	// rt-2 is the current token.
	stored2, err := s.LoadTokenByRefresh(ctx, ProviderFeishu, "rt-2")
	require.NoError(t, err)
	require.NotNil(t, stored2)
	assert.Equal(t, "at-2", stored2.AccessToken)
	assert.Equal(t, "rt-2", stored2.RefreshToken)

	// The new refresh token is NOT a replay.
	isReplay2, err := s.IsPreviousRefreshToken(ctx, ProviderFeishu, "rt-2")
	require.NoError(t, err)
	assert.False(t, isReplay2)
}

func TestStore_DeleteToken(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	tok := &Token{AccessToken: "at", RefreshToken: "rt", ExpiresIn: 60, UserID: "u"}
	require.NoError(t, s.SaveToken(ctx, ProviderSlack, "acct-1", tok))

	require.NoError(t, s.DeleteToken(ctx, ProviderSlack, "acct-1"))
	stored, err := s.LoadTokenByRefresh(ctx, ProviderSlack, "rt")
	require.NoError(t, err)
	assert.Nil(t, stored)
}

func TestStore_StateSaveAndConsume(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.SaveState(ctx, "state-1", ProviderFeishu, "cc", "S256", "https://app.test/cb", "acct-1", 10*time.Minute))

	row, err := s.ConsumeState(ctx, "state-1")
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, "state-1", row.State)
	assert.Equal(t, ProviderFeishu, row.Provider)
	assert.Equal(t, "cc", row.CodeChallenge)
	assert.Equal(t, "S256", row.CodeChallengeMethod)
	assert.Equal(t, "https://app.test/cb", row.RedirectURI)
	assert.Equal(t, "acct-1", row.AccountID)

	// Consume again — should return nil (already consumed).
	row2, err := s.ConsumeState(ctx, "state-1")
	require.NoError(t, err)
	assert.Nil(t, row2)
}

func TestStore_StateExpires(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// 1-second TTL. The schema stores expires_at as unix seconds, so
	// the test must sleep past the next second boundary to make the
	// expiry deterministic across CI clock jitter. 2.5s guarantees
	// the boundary is crossed regardless of where in the current
	// second the SaveState landed.
	require.NoError(t, s.SaveState(ctx, "state-exp", ProviderFeishu, "", "", "", "", time.Second))
	time.Sleep(2500 * time.Millisecond)

	row, err := s.ConsumeState(ctx, "state-exp")
	require.NoError(t, err)
	assert.Nil(t, row, "expired state should not load")
}

func TestStore_PurgeExpired(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Token with 1-second expiry. The schema stores expires_at as
	// unix seconds, so we sleep 2s to ensure the second-boundary is
	// crossed on slow CI runners.
	tok := &Token{AccessToken: "at", RefreshToken: "rt-soon", ExpiresIn: 1, UserID: "u"}
	require.NoError(t, s.SaveToken(ctx, ProviderFeishu, "acct-1", tok))
	time.Sleep(2 * time.Second)

	n, err := s.PurgeExpired(ctx)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, n, 1)

	stored, err := s.LoadTokenByRefresh(ctx, ProviderFeishu, "rt-soon")
	require.NoError(t, err)
	assert.Nil(t, stored)
}
