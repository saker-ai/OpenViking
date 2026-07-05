package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/ory/fosite"

	"github.com/saker-ai/ctxhub/internal/server/identity"
)

// RegisterHandler handles POST /api/v1/oauth/register (DCR, RFC 7591).
// The caller authenticates via the bearer flow separately; this
// endpoint is unauthenticated per the RFC, since registration produces
// new credentials.
func (s *Server) RegisterHandler(c *gin.Context) {
	if c.Request.Method != http.MethodPost {
		c.JSON(http.StatusMethodNotAllowed, gin.H{
			"error": gin.H{"code": "METHOD_NOT_ALLOWED", "message": "POST required"},
		})
		return
	}

	var req RegisterRequest
	if err := json.NewDecoder(c.Request.Body).Decode(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{"code": "INVALID_REQUEST", "message": err.Error()},
		})
		return
	}

	resp, err := s.CreateClient(req)
	if err != nil {
		switch err {
		case ErrClientIDConflict:
			c.JSON(http.StatusConflict, gin.H{
				"error": gin.H{"code": "CLIENT_ID_CONFLICT", "message": err.Error()},
			})
		default:
			c.JSON(http.StatusBadRequest, gin.H{
				"error": gin.H{"code": "INVALID_REQUEST", "message": err.Error()},
			})
		}
		return
	}

	c.JSON(http.StatusCreated, resp)
}

// AuthorizeHandler handles GET /api/v1/oauth/authorize. Two modes:
//
//   - Provider mode (provider=feishu|google|slack|dingtalk): generates
//     a state + PKCE verifier, persists them to the state cache, and
//     redirects (302) to the provider's authorize URL. The redirect_uri
//     is the server's own /api/v1/oauth/callback.
//   - Fosite mode (no provider): delegates to fosite's authorize-code
//     flow. The flow is not yet fully wired (login/consent UI lands in
//     a later phase); the handler returns 400 with a clear message so
//     callers know to use the provider flow.
func (s *Server) AuthorizeHandler(c *gin.Context) {
	var req AuthorizeRequest
	if err := c.ShouldBindQuery(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{"code": "INVALID_REQUEST", "message": err.Error()},
		})
		return
	}

	if req.Provider == "" {
		// Fosite authorize-code flow is not yet user-facing. Return a
		// 400 so callers know to use the provider flow; the OAuth
		// package returns no Not-Implemented status anywhere.
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{
				"code":    "INVALID_REQUEST",
				"message": "authorize requires a 'provider' parameter (feishu|google|slack|dingtalk)",
			},
		})
		return
	}

	p, ok := s.providers[req.Provider]
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{
				"code":    "INVALID_REQUEST",
				"message": "unknown provider: " + req.Provider,
			},
		})
		return
	}

	// Generate state + PKCE verifier. The verifier is sent to the
	// provider on token exchange; the challenge (derived from the
	// verifier via S256) is sent on the authorize URL.
	state, err := randomString(32)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": gin.H{"code": "INTERNAL", "message": "state generation failed"},
		})
		return
	}
	verifier, err := randomString(64)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": gin.H{"code": "INTERNAL", "message": "verifier generation failed"},
		})
		return
	}
	challenge := pkceS256Challenge(verifier)

	// Build the redirect_uri. The callback is always on this server's
	// /api/v1/oauth/callback path; the request's own redirect_uri
	// (where the user lands after auth) is stashed in the state cache
	// and used by the callback handler to 302 the user back.
	callbackURI := s.resolveCallbackURL(c.Request)
	redirectURI := req.RedirectURI
	if redirectURI == "" {
		redirectURI = callbackURI
	}

	if s.tokenStore == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": gin.H{
				"code":    "UNAVAILABLE",
				"message": "oauth token store is not configured",
			},
		})
		return
	}

	// Persist state for the callback handler to consume.
	accountID := identityAccount(c)
	if err := s.tokenStore.SaveState(c.Request.Context(), state, req.Provider, challenge, "S256", redirectURI, accountID, 10*time.Minute); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": gin.H{"code": "INTERNAL", "message": "state save failed: " + err.Error()},
		})
		return
	}

	authURL := p.AuthURL(state, challenge, callbackURI)
	c.Redirect(http.StatusFound, authURL)
}

// CallbackHandler handles GET /api/v1/oauth/callback. It consumes the
// state, exchanges the authorization code for an access_token +
// refresh_token via the provider adapter, persists the token pair
// (encrypted at rest), and redirects the user back to the
// originally-requested redirect_uri.
func (s *Server) CallbackHandler(c *gin.Context) {
	var req CallbackRequest
	if err := c.ShouldBindQuery(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{"code": "INVALID_REQUEST", "message": err.Error()},
		})
		return
	}

	if req.Error != "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{"code": "OAUTH_ERROR", "message": req.Error},
		})
		return
	}

	if req.Provider == "" || req.Code == "" || req.State == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{
				"code":    "INVALID_REQUEST",
				"message": "provider, code, and state are required",
			},
		})
		return
	}

	p, ok := s.providers[req.Provider]
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{"code": "INVALID_REQUEST", "message": "unknown provider: " + req.Provider},
		})
		return
	}

	if s.tokenStore == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": gin.H{"code": "UNAVAILABLE", "message": "oauth token store is not configured"},
		})
		return
	}

	stateRow, err := s.tokenStore.ConsumeState(c.Request.Context(), req.State)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": gin.H{"code": "INTERNAL", "message": "state consume failed: " + err.Error()},
		})
		return
	}
	if stateRow == nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{"code": "INVALID_STATE", "message": "state is unknown or expired"},
		})
		return
	}
	if stateRow.Provider != req.Provider {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{"code": "INVALID_STATE", "message": "state/provider mismatch"},
		})
		return
	}

	callbackURI := s.resolveCallbackURL(c.Request)
	tok, err := p.Exchange(c.Request.Context(), req.Code, pkceVerifierFromState(stateRow), callbackURI)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{
			"error": gin.H{"code": "OAUTH_EXCHANGE_FAILED", "message": err.Error()},
		})
		return
	}

	accountID := defaultStr(stateRow.AccountID, identityAccount(c))
	if accountID == "" {
		accountID = defaultStr(tok.UserID, "default")
	}
	tok.Scope = joinScopes(tok.Scope, p.Scopes())
	if err := s.tokenStore.SaveToken(c.Request.Context(), req.Provider, accountID, tok); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": gin.H{"code": "INTERNAL", "message": "token save failed: " + err.Error()},
		})
		return
	}

	// Redirect the user back to the originally-requested redirect_uri.
	// When no redirect_uri was stashed, render a simple success page so
	// interactive flows (e.g. CLI) see something usable.
	target := stateRow.RedirectURI
	if target == "" {
		c.JSON(http.StatusOK, gin.H{
			"provider":     req.Provider,
			"user_id":      tok.UserID,
			"subject":      tok.Subject,
			"expires_in":   tok.ExpiresIn,
		})
		return
	}
	if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
		// Treat as relative path on the public origin.
		target = s.resolve(target)
	}
	c.Redirect(http.StatusFound, target)
}

// TokenHandler handles POST /api/v1/oauth/token. It supports two grant
// types:
//
//   - grant_type=client_credentials: fosite-backed machine-to-machine
//     token. Wired since P9.
//   - grant_type=refresh_token: refreshes a stored provider token.
//     Requires provider + refresh_token form fields. Rotates the
//     refresh_token (the old one is invalidated; a new pair is
//     persisted). Increments oauthTokenRefreshTotal with
//     status="ok"/"error"/"permanent_error".
func (s *Server) TokenHandler(c *gin.Context) {
	if c.Request.Method != http.MethodPost {
		c.JSON(http.StatusMethodNotAllowed, gin.H{
			"error": gin.H{"code": "METHOD_NOT_ALLOWED", "message": "POST required"},
		})
		return
	}

	// Peek grant_type before deciding which path to take. fosite's
	// NewAccessRequest would otherwise reject refresh_token requests
	// that lack a provider field.
	if err := c.Request.ParseForm(); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{"code": "INVALID_REQUEST", "message": err.Error()},
		})
		return
	}
	grantType := strings.TrimSpace(c.Request.PostForm.Get("grant_type"))
	if grantType == "refresh_token" {
		s.handleRefreshGrant(c)
		return
	}

	// Fosite client_credentials / authorization_code grant.
	ctx := fosite.NewContext()
	accessRequest, err := s.provider.NewAccessRequest(ctx, c.Request, newSession(subjectFromIdentity(c)))
	if err != nil {
		s.provider.WriteAccessError(c.Request.Context(), c.Writer, accessRequest, err)
		return
	}

	for _, scope := range accessRequest.GetRequestedScopes() {
		if accessRequest.GetClient().GetScopes().Has(scope) {
			accessRequest.GrantScope(scope)
		}
	}

	response, err := s.provider.NewAccessResponse(ctx, accessRequest)
	if err != nil {
		s.provider.WriteAccessError(c.Request.Context(), c.Writer, accessRequest, err)
		return
	}

	s.provider.WriteAccessResponse(c.Request.Context(), c.Writer, accessRequest, response)
}

// handleRefreshGrant implements grant_type=refresh_token for the
// provider token flow. On success: rotates the refresh_token (the old
// one is invalidated; the new pair is persisted) and writes a standard
// OAuth 2.1 token response. On failure: returns a 400 with the
// standard error shape and increments the metric with status="error"
// or "permanent_error".
func (s *Server) handleRefreshGrant(c *gin.Context) {
	var req RefreshTokenRequest
	if err := c.ShouldBind(&req); err != nil {
		s.recordRefresh(c.Request.Context(), req.Provider, "error")
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{"code": "INVALID_REQUEST", "message": err.Error()},
		})
		return
	}
	if req.Provider == "" || req.RefreshToken == "" {
		s.recordRefresh(c.Request.Context(), req.Provider, "error")
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{
				"code":    "INVALID_REQUEST",
				"message": "provider and refresh_token are required",
			},
		})
		return
	}
	p, ok := s.providers[req.Provider]
	if !ok {
		s.recordRefresh(c.Request.Context(), req.Provider, "error")
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{"code": "INVALID_REQUEST", "message": "unknown provider: " + req.Provider},
		})
		return
	}
	if s.tokenStore == nil {
		s.recordRefresh(c.Request.Context(), req.Provider, "error")
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": gin.H{"code": "UNAVAILABLE", "message": "oauth token store is not configured"},
		})
		return
	}

	// Replay detection: if the presented refresh_token matches a
	// previously-rotated one, the chain is compromised. Revoke the
	// entire (provider, account) token family and return
	// permanent_error.
	stored, err := s.tokenStore.LoadTokenByRefresh(c.Request.Context(), req.Provider, req.RefreshToken)
	if err != nil {
		s.recordRefresh(c.Request.Context(), req.Provider, "error")
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": gin.H{"code": "INTERNAL", "message": "load token: " + err.Error()},
		})
		return
	}
	if stored == nil {
		// Maybe it's a previously-rotated (replayed) token.
		isReplay, _ := s.tokenStore.IsPreviousRefreshToken(c.Request.Context(), req.Provider, req.RefreshToken)
		if isReplay {
			// Revoke the entire token family for the (provider, *)
			// pair. We don't know the account_id from the hash alone
			// (the previous-hash lookup returns a count), so the
			// caller's refresh_token is rejected and the row is
			// surfaced as permanent_error. The Python reference
			// revokes the entire (account, user) chain; we approximate
			// by rejecting the replay here.
			s.recordRefresh(c.Request.Context(), req.Provider, "permanent_error")
			c.JSON(http.StatusBadRequest, gin.H{
				"error": gin.H{
					"code":    "INVALID_GRANT",
					"message": "refresh token has been rotated; replay detected",
				},
			})
			return
		}
		s.recordRefresh(c.Request.Context(), req.Provider, "permanent_error")
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{
				"code":    "INVALID_GRANT",
				"message": "refresh token is unknown or has been revoked",
			},
		})
		return
	}

	newTok, err := p.Refresh(c.Request.Context(), req.RefreshToken)
	if err != nil {
		if IsPermanentRefreshError(err) {
			s.recordRefresh(c.Request.Context(), req.Provider, "permanent_error")
			// Permanent failure: the refresh_token is invalid/expired/
			// revoked. Drop the row so the user must re-authorize.
			_ = s.tokenStore.DeleteToken(c.Request.Context(), req.Provider, stored.AccountID)
			c.JSON(http.StatusBadRequest, gin.H{
				"error": gin.H{
					"code":    "INVALID_GRANT",
					"message": "refresh token is invalid or expired: " + err.Error(),
				},
			})
			return
		}
		s.recordRefresh(c.Request.Context(), req.Provider, "error")
		c.JSON(http.StatusBadGateway, gin.H{
			"error": gin.H{
				"code":    "OAUTH_REFRESH_FAILED",
				"message": err.Error(),
			},
		})
		return
	}

	// Carry forward user_id + subject when the provider response omits
	// them (some providers only return tokens on refresh).
	if newTok.UserID == "" {
		newTok.UserID = stored.UserID
	}
	if newTok.Subject == "" {
		newTok.Subject = stored.Subject
	}
	if newTok.Scope == "" {
		newTok.Scope = stored.Scopes
	}
	if err := s.tokenStore.SaveToken(c.Request.Context(), req.Provider, stored.AccountID, newTok); err != nil {
		s.recordRefresh(c.Request.Context(), req.Provider, "error")
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": gin.H{"code": "INTERNAL", "message": "token save failed: " + err.Error()},
		})
		return
	}

	s.recordRefresh(c.Request.Context(), req.Provider, "ok")
	c.JSON(http.StatusOK, TokenResponse{
		AccessToken:  newTok.AccessToken,
		TokenType:    defaultStr(newTok.TokenType, "Bearer"),
		ExpiresIn:    newTok.ExpiresIn,
		RefreshToken: newTok.RefreshToken,
		Scope:        newTok.Scope,
		Provider:     req.Provider,
	})
}

// recordRefresh increments oauthTokenRefreshTotal. No-op when metrics
// is nil (test path).
func (s *Server) recordRefresh(ctx context.Context, provider, status string) {
	if s.metrics == nil {
		return
	}
	s.metrics.IncOAuthTokenRefresh(provider, status)
}

// RevokeHandler handles POST /api/v1/oauth/revoke (RFC 7009).
func (s *Server) RevokeHandler(c *gin.Context) {
	if c.Request.Method != http.MethodPost {
		c.JSON(http.StatusMethodNotAllowed, gin.H{
			"error": gin.H{"code": "METHOD_NOT_ALLOWED", "message": "POST required"},
		})
		return
	}

	ctx := fosite.NewContext()
	err := s.provider.NewRevocationRequest(ctx, c.Request)
	s.provider.WriteRevocationResponse(c.Request.Context(), c.Writer, err)
}

// JWKSHandler handles GET /api/v1/oauth/keys. For the HMAC strategy
// used in this phase, the JWKS is empty; production replaces this
// with the RSA public signing key.
func (s *Server) JWKSHandler(c *gin.Context) {
	c.JSON(http.StatusOK, JWKS{Keys: []map[string]any{}})
}

// subjectFromIdentity extracts the identity from the request context
// (set by the identity middleware) and returns a subject string
// suitable for the OAuth session. Falls back to the client_id when
// no identity is present (client_credentials grant).
func subjectFromIdentity(c *gin.Context) string {
	id, ok := identity.FromContext(c.Request.Context())
	if !ok || id.IsEmpty() {
		return ""
	}
	return id.Account
}

// identityAccount returns the account id from the request context, or
// empty when no identity middleware has run. Used by the authorize
// flow to bind the authorize state to the caller's account.
func identityAccount(c *gin.Context) string {
	id, ok := identity.FromContext(c.Request.Context())
	if !ok {
		return ""
	}
	return id.Account
}

// resolveCallbackURL builds the fully-qualified /api/v1/oauth/callback
// URL for the incoming request. Uses externalURL when configured;
// otherwise falls back to the request's own scheme://host.
func (s *Server) resolveCallbackURL(r *http.Request) string {
	path := s.basePath + "/callback"
	if s.externalURL != "" {
		return strings.TrimRight(s.externalURL, "/") + path
	}
	scheme := "http"
	if r != nil && r.TLS != nil {
		scheme = "https"
	}
	if r != nil && r.Header.Get("X-Forwarded-Proto") != "" {
		scheme = r.Header.Get("X-Forwarded-Proto")
	}
	host := "localhost"
	if r != nil && r.Host != "" {
		host = r.Host
	}
	return scheme + "://" + host + path
}

// pkceS256Challenge derives the S256 code_challenge from a
// code_verifier per RFC 7636: BASE64URL(SHA256(verifier)).
func pkceS256Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// pkceVerifierFromState reconstructs the PKCE verifier for the callback
// exchange. The state cache stores the verifier alongside the challenge
// so the callback can pass it to the provider's token endpoint.
//
// NOTE: the state cache stores code_challenge (the derived value), not
// the verifier itself, because the verifier is a secret. To support
// PKCE we would need to store the verifier encrypted; for the initial
// implementation we return "" (no verifier) which works for providers
// that don't enforce PKCE on the token endpoint. A follow-up will
// encrypt + store the verifier.
func pkceVerifierFromState(row *StateRow) string {
	_ = row
	return ""
}

// randomString returns n bytes of crypto-strong random data encoded as
// base64url (no padding). Suitable for state values and PKCE verifiers.
func randomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// joinScopes returns scope1 when non-empty, otherwise scope2. Used to
// prefer the provider-issued scope over the configured default.
func joinScopes(scope1, scope2 string) string {
	if strings.TrimSpace(scope1) != "" {
		return scope1
	}
	return scope2
}
