package routers

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"path"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/saker-ai/ctxhub/internal/auth/apikeys"
	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/ragfs"
)

// RegisterAdmin wires /api/v1/admin/* — account / user / API key management.
//
// Admin records are stored as JSON files under the global /_admin prefix
// (not scoped to the caller's account, since admin endpoints manage
// accounts cross-cutting):
//   - accounts:   /_admin/accounts/{id}.json
//   - api keys:   /_admin/accounts/{id}/api_keys/{key_id}.json
//   - users:      /_admin/users/{id}.json
//
// When RAGFS is nil every endpoint returns 501 UNSUPPORTED so the server
// still boots without a configured admin store.
//
// SDK-compat aliases (Go SDK at sdk/go calls account-scoped user routes):
//   - GET    /admin/accounts/:id/users                   — list account users
//   - POST   /admin/accounts/:id/users                   — register user
//   - DELETE /admin/accounts/:id/users/:user_id          — remove user
//   - PUT    /admin/accounts/:id/users/:user_id/role     — set role
//   - POST   /admin/accounts/:id/users/:user_id/regenerate_key — new key
//   - POST   /admin/migrate                              — migrate/cleanup
func RegisterAdmin(g *gin.RouterGroup, deps *Deps) {
	r := g.Group("/admin")
	r.GET("/accounts", listAccounts(deps))
	r.POST("/accounts", createAccount(deps))
	r.GET("/accounts/:id", getAccount(deps))
	r.DELETE("/accounts/:id", deleteAccount(deps))
	r.POST("/accounts/:id/api-keys", createAPIKey(deps))
	r.DELETE("/accounts/:id/api-keys/:key_id", revokeAPIKey(deps))
	r.GET("/users", listUsers(deps))
	r.POST("/users", createUser(deps))
	r.DELETE("/users/:id", deleteUser(deps))
	// SDK-compat: account-scoped user routes.
	r.GET("/accounts/:id/users", listAccountUsers(deps))
	r.POST("/accounts/:id/users", registerAccountUser(deps))
	r.DELETE("/accounts/:id/users/:user_id", removeAccountUser(deps))
	r.PUT("/accounts/:id/users/:user_id/role", setAccountUserRole(deps))
	r.POST("/accounts/:id/users/:user_id/regenerate_key", regenerateAccountUserKey(deps))
	r.POST("/migrate", adminMigrate(deps))
}

// adminAccountsDir is the ragfs directory holding account records.
const adminAccountsDir = "/_admin/accounts"

// adminUsersDir is the ragfs directory holding user records.
const adminUsersDir = "/_admin/users"

func adminAccountPath(id string) string {
	return ragfs.Normalize(path.Join(adminAccountsDir, id+".json"))
}

// adminAPIKeyDir retained for backward-compat with any caller that
// references it; new code should use the Manager (Deps.APIKeys) which
// stores keys in a single JSON file rather than per-key ragfs entries.
func adminAPIKeyDir(id string) string {
	return ragfs.Normalize(path.Join(adminAccountsDir, id, "api_keys"))
}

func adminAPIKeyPath(id, keyID string) string {
	return ragfs.Normalize(path.Join(adminAPIKeyDir(id), keyID+".json"))
}

func adminUserPath(id string) string {
	return ragfs.Normalize(path.Join(adminUsersDir, id+".json"))
}

// accountRecord is the JSON shape stored under /_admin/accounts/{id}.json.
type accountRecord struct {
	ID        string         `json:"id"`
	Name      string         `json:"name,omitempty"`
	State     string         `json:"state"`
	CreatedAt time.Time      `json:"created_at"`
	Disabled  bool           `json:"disabled"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// accountRequest is the JSON body for POST /admin/accounts.
//
// Field shape mirrors openviking/server/routers/admin.py CreateAccountRequest
// so Python-authored SDK / bot / eval clients can post the same payload:
//   - account_id: required Python field; mapped to ID.
//   - admin_user_id: required Python field naming the first admin user.
//   - seed: optional Python seed for deterministic key generation.
//   - user_config: optional Python user-config payload (kept as raw JSON;
//     Go does not yet wire a full UserConfig validator).
//
// The legacy Go fields (id, name, metadata) remain accepted as aliases so
// existing Go-authored clients keep working. Python fields take precedence
// when both are supplied.
type accountRequest struct {
	// Python-aligned fields (preferred).
	AccountID    string         `json:"account_id,omitempty"`
	AdminUserID  string         `json:"admin_user_id,omitempty"`
	Seed         string         `json:"seed,omitempty"`
	UserConfig   map[string]any `json:"user_config,omitempty"`
	// Go-specific aliases (kept for backward compat with Go-only clients).
	ID       string         `json:"id,omitempty"`
	Name     string         `json:"name,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// resolvedAccount returns the effective account id (Python field preferred
// over the Go alias) and whether the request specified one at all.
func (r *accountRequest) resolvedAccount() string {
	if r == nil {
		return ""
	}
	if r.AccountID != "" {
		return r.AccountID
	}
	return r.ID
}

// listAccounts handles GET /admin/accounts — list account IDs.
//
// Response shape mirrors openviking/server/routers/admin.py list_accounts:
// {"status":"ok","result":[<account_id>,...]}. The path field is a Go-only
// extension kept under result so existing Go clients can still introspect
// the storage prefix.
func listAccounts(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		entries, err := deps.RAGFS.ReadDir(c.Request.Context(), adminAccountsDir)
		if err != nil {
			if ragfs.IsNotFound(err) {
				c.JSON(http.StatusOK, okResponse(gin.H{
					"accounts": []any{},
					"path":     adminAccountsDir, // Go-only extension
				}))
				return
			}
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		ids := make([]string, 0, len(entries))
		for _, e := range entries {
			if e == nil || e.Info == nil || e.Info.IsDir {
				continue
			}
			if name := e.Info.Name; len(name) > 5 && name[len(name)-5:] == ".json" {
				ids = append(ids, name[:len(name)-5])
			}
		}
		c.JSON(http.StatusOK, okResponse(gin.H{
			"accounts": ids,
			"path":     adminAccountsDir, // Go-only extension
		}))
	}
}

// createAccount handles POST /admin/accounts — create an account record.
//
// Request shape mirrors openviking/server/routers/admin.py create_account
// (account_id, admin_user_id, seed, user_config) with Go-only aliases
// (id, name, metadata) kept for backward compatibility. Response wraps the
// created record in the standard {"status":"ok","result":...} envelope.
func createAccount(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		var req accountRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeValidationFailed, 422, err))
			return
		}
		id := req.resolvedAccount()
		if id == "" {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "id is required"))
			return
		}
		p := adminAccountPath(id)
		if _, err := deps.RAGFS.Stat(c.Request.Context(), p); err == nil {
			abortWithError(c, domain.ErrConflict.WithDetail("id", id))
			return
		}
		rec := accountRecord{
			ID:        id,
			Name:      req.Name,
			State:     "active",
			CreatedAt: time.Now().UTC(),
			Metadata:  req.Metadata,
		}
		payload, err := json.Marshal(rec)
		if err != nil {
			abortWithError(c, domain.Wrap(domain.CodeParseFailed, 422, err))
			return
		}
		if err := deps.RAGFS.Write(c.Request.Context(), p, bytes.NewReader(payload), 0o644); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		// Result shape mirrors Python: {account_id, admin_user_id, ...}.
		// The Go-only "path" and full "account" record are kept as
		// extensions so existing Go clients can introspect storage.
		result := gin.H{
			"account_id":   id,
			"admin_user_id": req.AdminUserID,
			"path":         p, // Go-only extension
			"account":      rec, // Go-only extension
		}
		c.JSON(http.StatusCreated, okResponse(result))
	}
}

// getAccount handles GET /admin/accounts/:id — read an account record.
//
// Note: openviking/server/routers/admin.py has no GET /accounts/{account_id}
// endpoint (only GET /accounts for listing). This route is a Go-only
// extension; the response is wrapped in the standard envelope so the shape
// stays consistent with the rest of the admin API.
func getAccount(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		id := c.Param("id")
		p := adminAccountPath(id)
		var buf bytes.Buffer
		if err := deps.RAGFS.Read(c.Request.Context(), p, &buf); err != nil {
			if ragfs.IsNotFound(err) {
				abortWithError(c, domain.Wrap(domain.CodeAccountNotFound, 404, err))
				return
			}
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		var rec accountRecord
		if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeParseFailed, 422, err))
			return
		}
		c.JSON(http.StatusOK, okResponse(gin.H{
			"id":      id,
			"path":    p,
			"account": rec,
		}))
	}
}

// deleteAccount handles DELETE /admin/accounts/:id — remove an account
// record (recursive — also clears any API keys stored under it).
//
// Response shape mirrors openviking/server/routers/admin.py delete_account:
// {"status":"ok","result":{"deleted":true}}. The Go-only "id" field is kept
// under result as a backward-compatible extension.
func deleteAccount(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		id := c.Param("id")
		// Remove the account record file.
		recPath := adminAccountPath(id)
		if err := deps.RAGFS.Remove(c.Request.Context(), recPath, false); err != nil {
			if ragfs.IsNotFound(err) {
				abortWithError(c, domain.Wrap(domain.CodeAccountNotFound, 404, err))
				return
			}
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		// Best-effort purge of any API keys subtree. Missing-target
		// errors are fine — accounts without API keys have no subtree.
		subtree := ragfs.Normalize(path.Join(adminAccountsDir, id))
		_ = deps.RAGFS.Remove(c.Request.Context(), subtree, true)
		c.JSON(http.StatusOK, okResponse(gin.H{
			"id":      id, // Go-only extension
			"deleted": true,
		}))
	}
}

// apiKeyRequest is the JSON body for POST /admin/accounts/:id/api-keys.
// Name is the human-readable label; KeyID is accepted for backward
// compatibility with the old client-supplied-key API and treated as
// the name when Name is empty. The actual key material is generated
// server-side; only the argon2id hash is persisted.
type apiKeyRequest struct {
	Name  string `json:"name"`
	KeyID string `json:"key_id"` // legacy, treated as Name when Name is empty
	Scope string `json:"scope,omitempty"`
}

// displayName returns the label used for the APIKeyRecord.Name field,
// preferring Name and falling back to the legacy KeyID.
func (r *apiKeyRequest) displayName() string {
	if r == nil {
		return ""
	}
	if r.Name != "" {
		return r.Name
	}
	return r.KeyID
}

// apiKeyResponse is the JSON shape returned in the api_key field of
// admin responses. It omits the Hash field so the argon2id hash never
// leaks over HTTP.
type apiKeyResponse struct {
	ID          string    `json:"id"`
	Name        string    `json:"name,omitempty"`
	AccountID   string    `json:"account_id,omitempty"`
	Scope       string    `json:"scope,omitempty"`
	KeyPrefix   string    `json:"key_prefix"`
	Fingerprint string    `json:"fingerprint"`
	CreatedAt   time.Time `json:"created_at"`
	LastUsedAt  time.Time `json:"last_used_at,omitempty"`
	RevokedAt   time.Time `json:"revoked_at,omitempty"`
	Revoked     bool      `json:"revoked"`
}

func newAPIKeyResponse(r *apikeys.APIKeyRecord) apiKeyResponse {
	return apiKeyResponse{
		ID:          r.ID,
		Name:        r.Name,
		AccountID:   r.AccountID,
		Scope:       r.Scope,
		KeyPrefix:   r.KeyPrefix,
		Fingerprint: r.Fingerprint,
		CreatedAt:   r.CreatedAt,
		LastUsedAt:  r.LastUsedAt,
		RevokedAt:   r.RevokedAt,
		Revoked:     r.Revoked(),
	}
}

// createAPIKey handles POST /admin/accounts/:id/api-keys — generate a new
// API key for an account. The plaintext key is returned once in the
// response body; only the argon2id hash is persisted.
func createAPIKey(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil || deps.APIKeys == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		id := c.Param("id")
		// Verify the account exists.
		if _, err := deps.RAGFS.Stat(c.Request.Context(), adminAccountPath(id)); err != nil {
			if ragfs.IsNotFound(err) {
				abortWithError(c, domain.Wrap(domain.CodeAccountNotFound, 404, err))
				return
			}
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		var req apiKeyRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeValidationFailed, 422, err))
			return
		}
		name := req.displayName()
		if name == "" {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "name is required"))
			return
		}
		plaintext, record, err := deps.APIKeys.Generate(name)
		if err != nil {
			abortWithError(c, domain.Wrap(domain.CodeInternalError, 500, err))
			return
		}
		if err := deps.APIKeys.SetAccountScope(record.ID, id, req.Scope); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeInternalError, 500, err))
			return
		}
		if err := deps.APIKeys.Persist(); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeInternalError, 500, err))
			return
		}
		c.JSON(http.StatusCreated, gin.H{
			"account_id": id,
			"key_id":     record.ID,
			"key":        plaintext, // returned once; never persisted
			"api_key":    newAPIKeyResponse(record),
		})
	}
}

// revokeAPIKey handles DELETE /admin/accounts/:id/api-keys/:key_id —
// soft-revoke an API key. The :key_id path parameter is the uuid
// returned by createAPIKey. The ?purge=1 query is accepted for
// backward compatibility but treated as a soft-revoke (the Manager
// only supports soft-revoke; hard-delete is a future task).
func revokeAPIKey(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil || deps.APIKeys == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		id := c.Param("id")
		keyID := c.Param("key_id")
		// Verify the account exists.
		if _, err := deps.RAGFS.Stat(c.Request.Context(), adminAccountPath(id)); err != nil {
			if ragfs.IsNotFound(err) {
				abortWithError(c, domain.Wrap(domain.CodeAccountNotFound, 404, err))
				return
			}
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		record, err := deps.APIKeys.Get(keyID)
		if err != nil {
			abortWithError(c, domain.Wrap(domain.CodeResourceNotFound, 404, err))
			return
		}
		// Enforce account scoping: the key's AccountID must match the
		// account in the URL. Cross-account revocation is rejected.
		if record.AccountID != "" && record.AccountID != id {
			abortWithError(c, domain.Wrap(domain.CodeResourceNotFound, 404, err))
			return
		}
		if err := deps.APIKeys.Revoke(keyID); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeResourceNotFound, 404, err))
			return
		}
		if err := deps.APIKeys.Persist(); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeInternalError, 500, err))
			return
		}
		// Refresh the record to pick up the RevokedAt timestamp set by
		// Revoke.
		record, _ = deps.APIKeys.Get(keyID)
		resp := gin.H{
			"account_id": id,
			"key_id":     keyID,
			"revoked":    true,
		}
		if record != nil {
			resp["api_key"] = newAPIKeyResponse(record)
		}
		c.JSON(http.StatusOK, resp)
	}
}

// userRequest is the JSON body for POST /admin/users.
type userRequest struct {
	ID       string         `json:"id"`
	Account  string         `json:"account"`
	Metadata map[string]any `json:"metadata"`
}

// userRecord is the JSON shape stored under /_admin/users/{id}.json.
type userRecord struct {
	ID        string         `json:"id"`
	Account   string         `json:"account"`
	State     string         `json:"state"`
	CreatedAt time.Time      `json:"created_at"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// listUsers handles GET /admin/users — list user IDs.
func listUsers(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		entries, err := deps.RAGFS.ReadDir(c.Request.Context(), adminUsersDir)
		if err != nil {
			if ragfs.IsNotFound(err) {
				c.JSON(http.StatusOK, gin.H{"users": []any{}, "path": adminUsersDir})
				return
			}
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		ids := make([]string, 0, len(entries))
		for _, e := range entries {
			if e == nil || e.Info == nil || e.Info.IsDir {
				continue
			}
			if name := e.Info.Name; len(name) > 5 && name[len(name)-5:] == ".json" {
				ids = append(ids, name[:len(name)-5])
			}
		}
		c.JSON(http.StatusOK, gin.H{"users": ids, "path": adminUsersDir})
	}
}

// createUser handles POST /admin/users — create a user record.
func createUser(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		var req userRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeValidationFailed, 422, err))
			return
		}
		if req.ID == "" {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "id is required"))
			return
		}
		p := adminUserPath(req.ID)
		if _, err := deps.RAGFS.Stat(c.Request.Context(), p); err == nil {
			abortWithError(c, domain.ErrConflict.WithDetail("id", req.ID))
			return
		}
		rec := userRecord{
			ID:        req.ID,
			Account:   req.Account,
			State:     "active",
			CreatedAt: time.Now().UTC(),
			Metadata:  req.Metadata,
		}
		payload, err := json.Marshal(rec)
		if err != nil {
			abortWithError(c, domain.Wrap(domain.CodeParseFailed, 422, err))
			return
		}
		if err := deps.RAGFS.Write(c.Request.Context(), p, bytes.NewReader(payload), 0o644); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		c.JSON(http.StatusCreated, gin.H{"id": req.ID, "path": p, "user": rec})
	}
}

// deleteUser handles DELETE /admin/users/:id — remove a user record.
func deleteUser(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		id := c.Param("id")
		p := adminUserPath(id)
		if err := deps.RAGFS.Remove(c.Request.Context(), p, false); err != nil {
			if ragfs.IsNotFound(err) {
				abortWithError(c, domain.Wrap(domain.CodeResourceNotFound, 404, err))
				return
			}
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		c.JSON(http.StatusOK, gin.H{"id": id, "deleted": true})
	}
}

// readUserRecord reads and unmarshals a user JSON document.
func readUserRecord(ctx context.Context, fs ragfs.FileSystem, p string) (*userRecord, error) {
	var buf bytes.Buffer
	if err := fs.Read(ctx, p, &buf); err != nil {
		return nil, err
	}
	var u userRecord
	if err := json.Unmarshal(buf.Bytes(), &u); err != nil {
		return nil, err
	}
	return &u, nil
}

// writeUserRecord marshals and writes a user JSON document.
func writeUserRecord(ctx context.Context, fs ragfs.FileSystem, p string, u *userRecord) error {
	body, err := json.MarshalIndent(u, "", "  ")
	if err != nil {
		return err
	}
	return fs.Write(ctx, p, bytes.NewReader(body), 0o644)
}

// listAccountUsers handles GET /admin/accounts/:id/users — list user IDs
// whose record's Account field matches the account ID. The Go SDK calls
// this to enumerate account members.
func listAccountUsers(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		accountID := c.Param("id")
		entries, err := deps.RAGFS.ReadDir(c.Request.Context(), adminUsersDir)
		if err != nil {
			if ragfs.IsNotFound(err) {
				c.JSON(http.StatusOK, okResponse(gin.H{"users": []any{}, "account": accountID}))
				return
			}
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		out := make([]*userRecord, 0, len(entries))
		for _, e := range entries {
			if e == nil || e.Info == nil || e.Info.IsDir {
				continue
			}
			name := e.Info.Name
			if len(name) <= 5 || name[len(name)-5:] != ".json" {
				continue
			}
			u, uerr := readUserRecord(c.Request.Context(), deps.RAGFS, ragfs.Normalize(path.Join(adminUsersDir, name)))
			if uerr != nil || u == nil {
				continue
			}
			if u.Account != accountID {
				continue
			}
			out = append(out, u)
		}
		c.JSON(http.StatusOK, okResponse(gin.H{"users": out, "account": accountID}))
	}
}

// registerAccountUserRequest is the JSON body for POST /admin/accounts/:id/users.
type registerAccountUserRequest struct {
	UserID string         `json:"user_id,omitempty"`
	Role   string         `json:"role,omitempty"`
	Config map[string]any `json:"user_config,omitempty"`
}

// registerAccountUser handles POST /admin/accounts/:id/users — register a
// user under an account. The Go SDK posts {user_id, role, user_config}.
// Default role is "user" when empty.
func registerAccountUser(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		accountID := c.Param("id")
		var req registerAccountUserRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeValidationFailed, 422, err))
			return
		}
		if req.UserID == "" {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "user_id is required"))
			return
		}
		role := req.Role
		if role == "" {
			role = "user"
		}
		p := adminUserPath(req.UserID)
		if _, err := deps.RAGFS.Stat(c.Request.Context(), p); err == nil {
			abortWithError(c, domain.NewAppError(domain.CodeConflict, 409, "user already exists"))
			return
		}
		meta := map[string]any{"role": role}
		for k, v := range req.Config {
			meta[k] = v
		}
		rec := userRecord{
			ID:        req.UserID,
			Account:   accountID,
			State:     "active",
			CreatedAt: time.Now().UTC(),
			Metadata:  meta,
		}
		if err := writeUserRecord(c.Request.Context(), deps.RAGFS, p, &rec); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		c.JSON(http.StatusCreated, okResponse(gin.H{"user": rec, "account": accountID}))
	}
}

// removeAccountUser handles DELETE /admin/accounts/:id/users/:user_id —
// remove a user from an account. The user record is deleted entirely
// (cross-account membership is not supported in the single-record model).
func removeAccountUser(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		userID := c.Param("user_id")
		p := adminUserPath(userID)
		if err := deps.RAGFS.Remove(c.Request.Context(), p, false); err != nil {
			if ragfs.IsNotFound(err) {
				abortWithError(c, domain.Wrap(domain.CodeResourceNotFound, 404, err))
				return
			}
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		c.JSON(http.StatusOK, okResponse(gin.H{"user_id": userID, "deleted": true}))
	}
}

// setAccountUserRole handles PUT /admin/accounts/:id/users/:user_id/role —
// update the user's role. The role is stored in userRecord.Metadata["role"].
func setAccountUserRole(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		userID := c.Param("user_id")
		p := adminUserPath(userID)
		u, err := readUserRecord(c.Request.Context(), deps.RAGFS, p)
		if err != nil {
			if ragfs.IsNotFound(err) {
				abortWithError(c, domain.Wrap(domain.CodeResourceNotFound, 404, err))
				return
			}
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		var req struct {
			Role string `json:"role,omitempty"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeValidationFailed, 422, err))
			return
		}
		if req.Role == "" {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "role is required"))
			return
		}
		if u.Metadata == nil {
			u.Metadata = make(map[string]any)
		}
		u.Metadata["role"] = req.Role
		if err := writeUserRecord(c.Request.Context(), deps.RAGFS, p, u); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		c.JSON(http.StatusOK, okResponse(gin.H{"user": u}))
	}
}

// regenerateAccountUserKey handles POST /admin/accounts/:id/users/:user_id/regenerate_key
// — regenerate the user's API key. A new key is generated and stored in
// userRecord.Metadata["api_key"]; the old key is overwritten.
func regenerateAccountUserKey(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		userID := c.Param("user_id")
		p := adminUserPath(userID)
		u, err := readUserRecord(c.Request.Context(), deps.RAGFS, p)
		if err != nil {
			if ragfs.IsNotFound(err) {
				abortWithError(c, domain.Wrap(domain.CodeResourceNotFound, 404, err))
				return
			}
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		var randBuf [8]byte
		if _, err := rand.Read(randBuf[:]); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeInternalError, 500, err))
			return
		}
		newKey := "ov-key-" + userID + "-" + hex.EncodeToString(randBuf[:])
		if u.Metadata == nil {
			u.Metadata = make(map[string]any)
		}
		u.Metadata["api_key"] = newKey
		if err := writeUserRecord(c.Request.Context(), deps.RAGFS, p, u); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		c.JSON(http.StatusOK, okResponse(gin.H{
			"user_id":   userID,
			"api_key":   newKey,
			"generated": true,
		}))
	}
}

// adminMigrate handles POST /admin/migrate — run a migration action
// ("migrate" or "cleanup"). Both actions walk every account's sessions
// and tmp directories: committed sessions older than 24h are deleted, and
// temp files older than 1h are removed. Errors per account/session are
// tolerated and do not abort the walk.
func adminMigrate(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		var req struct {
			Action string `json:"action,omitempty"`
		}
		_ = c.ShouldBindJSON(&req)
		if req.Action == "" {
			req.Action = "migrate"
		}
		ctx := c.Request.Context()
		accounts, err := deps.RAGFS.ReadDir(ctx, "/accounts")
		if err != nil {
			c.JSON(http.StatusOK, okResponse(gin.H{
				"action":          req.Action,
				"status":          "completed",
				"migrated":        0,
				"cleaned":         0,
				"reclaimed_bytes": 0,
			}))
			return
		}
		var migrated, cleaned, reclaimed int64
		for _, acc := range accounts {
			if acc == nil || acc.Info == nil || !acc.Info.IsDir {
				continue
			}
			migrated++
			sessionsDir := ragfs.Normalize(path.Join("/accounts", acc.Info.Name, "sessions"))
			if entries, err := deps.RAGFS.ReadDir(ctx, sessionsDir); err == nil {
				for _, e := range entries {
					if e == nil || e.Info == nil || e.Info.IsDir {
						continue
					}
					name := e.Info.Name
					if len(name) <= 5 || name[len(name)-5:] != ".json" {
						continue
					}
					sp := ragfs.Normalize(path.Join(sessionsDir, name))
					if !shouldCleanSession(ctx, deps.RAGFS, sp) {
						continue
					}
					if rerr := deps.RAGFS.Remove(ctx, sp, false); rerr != nil && !ragfs.IsNotFound(rerr) {
						continue
					}
					cleaned++
				}
			}
			tmpDir := ragfs.Normalize(path.Join("/accounts", acc.Info.Name, "tmp"))
			if entries, err := deps.RAGFS.ReadDir(ctx, tmpDir); err == nil {
				for _, e := range entries {
					if e == nil || e.Info == nil || e.Info.IsDir {
						continue
					}
					if time.Since(e.Info.ModTime) < time.Hour {
						continue
					}
					tp := ragfs.Normalize(path.Join(tmpDir, e.Info.Name))
					if rerr := deps.RAGFS.Remove(ctx, tp, false); rerr != nil && !ragfs.IsNotFound(rerr) {
						continue
					}
					reclaimed += e.Info.Size
				}
			}
		}
		c.JSON(http.StatusOK, okResponse(gin.H{
			"action":          req.Action,
			"status":          "completed",
			"migrated":        migrated,
			"cleaned":         cleaned,
			"reclaimed_bytes": reclaimed,
		}))
	}
}

// shouldCleanSession reports whether the session JSON at p is committed and
// older than 24h. Read failures default to false so a corrupt session is
// never silently deleted.
func shouldCleanSession(ctx context.Context, fs ragfs.FileSystem, p string) bool {
	var buf bytes.Buffer
	if err := fs.Read(ctx, p, &buf); err != nil {
		return false
	}
	var s struct {
		Status      string     `json:"status"`
		CommittedAt *time.Time `json:"committed_at"`
	}
	if err := json.Unmarshal(buf.Bytes(), &s); err != nil {
		return false
	}
	if s.Status != "committed" || s.CommittedAt == nil {
		return false
	}
	return time.Since(*s.CommittedAt) > 24*time.Hour
}
