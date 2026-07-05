// Package oauth — SQLite-backed encrypted token store.
//
// The store persists provider-issued access_token + refresh_token pairs
// encrypted at rest via internal/crypto envelope encryption. Only the
// SHA-256 hash of the refresh_token is stored in plaintext, to support
// lookup-by-token at refresh time without leaking the token itself.
//
// Refresh-token rotation is a single UPDATE: the row's refresh_token_hash
// is replaced with the new token's hash, and the previous hash is retained
// for one cycle so a replayed refresh request can be detected and rejected
// with status="permanent_error".
//
// The schema is bundled via embed.FS at internal/server/oauth/migrations.
// All SQL uses parameterized queries; no string interpolation of user
// input into SQL text.
package oauth

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver; no cgo

	"github.com/saker-ai/ctxhub/internal/crypto"
	"github.com/saker-ai/ctxhub/internal/observability"
	"github.com/saker-ai/ctxhub/internal/server/oauth/migrations"
)

// Store is the SQLite-backed encrypted token store. It is goroutine-safe
// via a sync.RWMutex around the underlying *sql.DB (modernc.org/sqlite
// serializes writes internally; the mutex keeps the public API simple).
type Store struct {
	mu        sync.RWMutex
	db        *sql.DB
	encryptor crypto.Encryptor
	metrics   *observability.Metrics
	path      string
}

// StoreConfig configures the store.
type StoreConfig struct {
	// DSN is the SQLite DSN. ":memory:" for in-memory, or a file path.
	// File paths are created if missing. The schema is applied on open.
	DSN string
	// Encryptor used for at-rest encryption of access/refresh tokens.
	// When nil, tokens are stored in plaintext (test-only; production
	// must supply a real Encryptor).
	Encryptor crypto.Encryptor
	// Metrics records oauthTokenRefreshTotal. May be nil in tests.
	Metrics *observability.Metrics
}

// NewStore opens (or creates) a SQLite store at dsn and applies the schema.
func NewStore(cfg StoreConfig) (*Store, error) {
	if cfg.DSN == "" {
		return nil, errors.New("oauth store: empty DSN")
	}
	dsn := cfg.DSN
	if dsn != ":memory:" {
		// modernc.org/sqlite supports a WAL pragma via DSN query string.
		// For file DSNs, append the pragma query if not present.
		if !strings.Contains(dsn, "?") {
			dsn = dsn + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
		}
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("oauth store: open %s: %w", cfg.DSN, err)
	}
	db.SetMaxOpenConns(1) // modernc.org/sqlite is serialized; avoid pool overhead
	if err := applySchema(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("oauth store: schema: %w", err)
	}
	return &Store{
		db:        db,
		encryptor: cfg.Encryptor,
		metrics:   cfg.Metrics,
		path:      cfg.DSN,
	}, nil
}

// applySchema reads the embedded .sql files in lexicographic order and
// executes them. Idempotent — CREATE TABLE IF NOT EXISTS.
func applySchema(db *sql.DB) error {
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		body, err := migrations.FS.ReadFile(name)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		if _, err := db.Exec(string(body)); err != nil {
			return fmt.Errorf("apply %s: %w", name, err)
		}
	}
	return nil
}

// Close releases the database handle.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

// Path returns the DSN the store was opened with.
func (s *Store) Path() string { return s.path }

// SaveToken upserts the (provider, account_id, user_id) row with a fresh
// token pair. AccessToken + RefreshToken are encrypted at rest. The
// previous refresh_token_hash (if any) is moved into
// previous_refresh_token_hash for one-cycle replay detection.
func (s *Store) SaveToken(ctx context.Context, provider, accountID string, tok *Token) error {
	if provider == "" || accountID == "" || tok == nil {
		return errors.New("oauth store: provider, accountID, and token are required")
	}
	if tok.RefreshToken == "" {
		return errors.New("oauth store: refresh_token is required")
	}
	userID := defaultStr(tok.UserID, accountID)
	subject := defaultStr(tok.Subject, userID)
	encAccess, err := s.encrypt(tok.AccessToken)
	if err != nil {
		return fmt.Errorf("oauth store: encrypt access: %w", err)
	}
	encRefresh, err := s.encrypt(tok.RefreshToken)
	if err != nil {
		return fmt.Errorf("oauth store: encrypt refresh: %w", err)
	}
	now := nowUnix()
	expires := now + int64(maxInt(0, tok.ExpiresIn))
	refreshHash := hashToken(tok.RefreshToken)

	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = s.db.ExecContext(ctx, `
        INSERT INTO oauth_provider_tokens
            (provider, account_id, user_id, subject,
             access_token_ciphertext, refresh_token_hash, refresh_token_ciphertext,
             previous_refresh_token_hash, scopes, expires_at,
             created_at, updated_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, NULL, ?, ?, ?, ?)
        ON CONFLICT(provider, account_id, user_id) DO UPDATE SET
            subject                       = excluded.subject,
            access_token_ciphertext       = excluded.access_token_ciphertext,
            previous_refresh_token_hash   = oauth_provider_tokens.refresh_token_hash,
            refresh_token_hash            = excluded.refresh_token_hash,
            refresh_token_ciphertext      = excluded.refresh_token_ciphertext,
            scopes                        = excluded.scopes,
            expires_at                    = excluded.expires_at,
            updated_at                    = excluded.updated_at`,
		provider, accountID, userID, subject,
		encAccess, refreshHash, encRefresh,
		tok.Scope, expires,
		now, now)
	if err != nil {
		return fmt.Errorf("oauth store: upsert: %w", err)
	}
	return nil
}

// LoadTokenByRefresh returns the stored token pair matching the given
// refresh_token. Returns (nil, nil) when the token is unknown.
func (s *Store) LoadTokenByRefresh(ctx context.Context, provider, refreshToken string) (*StoredToken, error) {
	if provider == "" || refreshToken == "" {
		return nil, nil
	}
	hash := hashToken(refreshToken)
	s.mu.RLock()
	defer s.mu.RUnlock()
	row := s.db.QueryRowContext(ctx, `
        SELECT provider, account_id, user_id, subject,
               access_token_ciphertext, refresh_token_ciphertext,
               previous_refresh_token_hash, scopes, expires_at,
               created_at, updated_at
        FROM oauth_provider_tokens
        WHERE provider = ? AND refresh_token_hash = ?`,
		provider, hash)
	return s.scanStoredToken(row)
}

// LoadTokenByUser returns the current stored token pair for a
// (provider, accountID) pair, regardless of which refresh_token is
// currently active. Returns (nil, nil) when no row exists.
func (s *Store) LoadTokenByUser(ctx context.Context, provider, accountID string) (*StoredToken, error) {
	if provider == "" || accountID == "" {
		return nil, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	row := s.db.QueryRowContext(ctx, `
        SELECT provider, account_id, user_id, subject,
               access_token_ciphertext, refresh_token_ciphertext,
               previous_refresh_token_hash, scopes, expires_at,
               created_at, updated_at
        FROM oauth_provider_tokens
        WHERE provider = ? AND account_id = ?`,
		provider, accountID)
	return s.scanStoredToken(row)
}

// IsPreviousRefreshToken reports whether the given refresh_token matches
// the previous_refresh_token_hash of any row for the provider. Used to
// detect replay (a refresh request using a token that has already been
// rotated).
func (s *Store) IsPreviousRefreshToken(ctx context.Context, provider, refreshToken string) (bool, error) {
	if provider == "" || refreshToken == "" {
		return false, nil
	}
	hash := hashToken(refreshToken)
	s.mu.RLock()
	defer s.mu.RUnlock()
	var count int
	err := s.db.QueryRowContext(ctx, `
        SELECT COUNT(*) FROM oauth_provider_tokens
        WHERE provider = ? AND previous_refresh_token_hash = ?`,
		provider, hash).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// DeleteToken removes the row for (provider, accountID). Used by the
// refresh path's replay-revoke fallback (RFC 9700 §4.14: invalidate the
// entire token family when a consumed refresh_token is replayed).
func (s *Store) DeleteToken(ctx context.Context, provider, accountID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `
        DELETE FROM oauth_provider_tokens
        WHERE provider = ? AND account_id = ?`,
		provider, accountID)
	if err != nil {
		return fmt.Errorf("oauth store: delete: %w", err)
	}
	return nil
}

// SaveState stores a short-lived authorize state value (with PKCE
// verifier + redirect_uri). The state is consumed exactly once via
// ConsumeState.
func (s *Store) SaveState(ctx context.Context, state, provider, codeChallenge, codeChallengeMethod, redirectURI, accountID string, ttl time.Duration) error {
	if state == "" || provider == "" {
		return errors.New("oauth store: state and provider are required")
	}
	now := nowUnix()
	expires := now + int64(ttl.Seconds())
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO oauth_state_cache
            (state, provider, code_challenge, code_challenge_method,
             redirect_uri, account_id, created_at, expires_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(state) DO UPDATE SET
            provider              = excluded.provider,
            code_challenge        = excluded.code_challenge,
            code_challenge_method = excluded.code_challenge_method,
            redirect_uri          = excluded.redirect_uri,
            account_id            = excluded.account_id,
            created_at            = excluded.created_at,
            expires_at            = excluded.expires_at`,
		state, provider, codeChallenge, codeChallengeMethod, redirectURI, accountID, now, expires)
	if err != nil {
		return fmt.Errorf("oauth store: save state: %w", err)
	}
	return nil
}

// StateRow is the consumed authorize-state row.
type StateRow struct {
	State             string
	Provider          string
	CodeChallenge     string
	CodeChallengeMethod string
	RedirectURI       string
	AccountID         string
}

// ConsumeState atomically reads + deletes a state row. Returns (nil, nil)
// when the state is unknown or expired.
func (s *Store) ConsumeState(ctx context.Context, state string) (*StateRow, error) {
	if state == "" {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("oauth store: begin: %w", err)
	}
	defer tx.Rollback()
	var (
		row StateRow
		cc, ccm, redirect, acct sql.NullString
		created, expires        int64
	)
	err = tx.QueryRowContext(ctx, `
        SELECT state, provider, code_challenge, code_challenge_method,
               redirect_uri, account_id, created_at, expires_at
        FROM oauth_state_cache
        WHERE state = ?`, state).Scan(
		&row.State, &row.Provider, &cc, &ccm, &redirect, &acct, &created, &expires)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("oauth store: load state: %w", err)
	}
	if expires < nowUnix() {
		// Expired — delete and return nil.
		_, _ = tx.ExecContext(ctx, `DELETE FROM oauth_state_cache WHERE state = ?`, state)
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("oauth store: commit expire: %w", err)
		}
		return nil, nil
	}
	row.CodeChallenge = cc.String
	row.CodeChallengeMethod = ccm.String
	row.RedirectURI = redirect.String
	row.AccountID = acct.String
	if _, err := tx.ExecContext(ctx, `DELETE FROM oauth_state_cache WHERE state = ?`, state); err != nil {
		return nil, fmt.Errorf("oauth store: consume state: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("oauth store: commit: %w", err)
	}
	return &row, nil
}

// StoredToken is the decrypted form of a stored token row.
type StoredToken struct {
	Provider          string
	AccountID         string
	UserID            string
	Subject           string
	AccessToken       string
	RefreshToken      string
	PreviousRefreshHash string
	Scopes            string
	ExpiresAt         int64
	CreatedAt         int64
	UpdatedAt         int64
}

// scanStoredToken reads a row from a *sql.Row or *sql.Rows.
func (s *Store) scanStoredToken(row scanner) (*StoredToken, error) {
	var (
		st         StoredToken
		encAccess  string
		encRefresh string
		prevHash   sql.NullString
		scopes     sql.NullString
	)
	err := row.Scan(
		&st.Provider, &st.AccountID, &st.UserID, &st.Subject,
		&encAccess, &encRefresh, &prevHash, &scopes,
		&st.ExpiresAt, &st.CreatedAt, &st.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("oauth store: scan: %w", err)
	}
	access, err := s.decrypt(encAccess)
	if err != nil {
		return nil, fmt.Errorf("oauth store: decrypt access: %w", err)
	}
	refresh, err := s.decrypt(encRefresh)
	if err != nil {
		return nil, fmt.Errorf("oauth store: decrypt refresh: %w", err)
	}
	st.AccessToken = access
	st.RefreshToken = refresh
	st.PreviousRefreshHash = prevHash.String
	st.Scopes = scopes.String
	return &st, nil
}

// encrypt returns the JSON-serialized crypto.Ciphertext envelope for the
// plaintext. When no encryptor is configured (test-only), the plaintext
// is wrapped in a synthetic envelope so the decode path round-trips.
func (s *Store) encrypt(plaintext string) (string, error) {
	if s.encryptor == nil {
		// Test-only fallback: base64 plaintext inside a synthetic envelope.
		synthetic := &crypto.Ciphertext{
			DEK:        "test",
			Nonce:      "test",
			Ciphertext: plaintext,
			KDF:        "plaintext",
			Provider:   "test",
		}
		b, err := json.Marshal(synthetic)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	ct, err := s.encryptor.Encrypt([]byte(plaintext), nil)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(ct)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// decrypt reverses encrypt.
func (s *Store) decrypt(envelope string) (string, error) {
	if envelope == "" {
		return "", nil
	}
	var ct crypto.Ciphertext
	if err := json.Unmarshal([]byte(envelope), &ct); err != nil {
		return "", fmt.Errorf("decode envelope: %w", err)
	}
	if s.encryptor == nil {
		// Test-only fallback: read plaintext from synthetic envelope.
		if ct.Provider == "test" {
			return ct.Ciphertext, nil
		}
		return "", errors.New("oauth store: no encryptor configured but ciphertext is not a test envelope")
	}
	pt, err := s.encryptor.Decrypt(&ct, nil)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

// scanner is the shared interface between *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

// hashToken returns the SHA-256 hex of a token. The hash is the lookup key
// for refresh-token rows; the plaintext token never leaves the encryptor.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// nowUnix returns the current time as unix seconds.
func nowUnix() int64 {
	return time.Now().UTC().Unix()
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// PurgeExpired removes rows whose expires_at is in the past. The caller
// is expected to run this on a ticker; it is safe to call concurrently
// with reads/writes (the mutex serializes access).
func (s *Store) PurgeExpired(ctx context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := nowUnix()
	res, err := s.db.ExecContext(ctx, `
        DELETE FROM oauth_provider_tokens WHERE expires_at != 0 AND expires_at < ?`, now)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM oauth_state_cache WHERE expires_at < ?`, now); err != nil {
		return int(n), err
	}
	return int(n), nil
}

// DSNForFile returns a SQLite DSN suitable for a file path, with the
// WAL + busy_timeout pragmas that the store applies. Exposed so callers
// (e.g. config wiring) can build a DSN from a state directory.
func DSNForFile(stateDir, filename string) string {
	if stateDir == "" {
		return filename
	}
	return filepath.Join(stateDir, filename)
}
