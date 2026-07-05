// Package oauth wires the OpenViking OAuth 2.1 server surface using
// github.com/ory/fosite. It exposes:
//
//   - /.well-known/oauth-authorization-server  (RFC 8414)
//   - /.well-known/oauth-protected-resource    (RFC 9728)
//   - /api/v1/oauth/register                    (DCR, RFC 7591)
//   - /api/v1/oauth/authorize                   (provider-based user OAuth)
//   - /api/v1/oauth/callback                    (provider OAuth callback)
//   - /api/v1/oauth/token                       (token endpoint)
//   - /api/v1/oauth/revoke                      (RFC 7009 token revocation)
//   - /api/v1/oauth/keys                        (JWKS)
//
// The fosite-backed client_credentials grant is fully wired. The
// authorize endpoint is wired to the external provider flow
// (feishu/google/slack/dingtalk) — it redirects to the provider's auth
// URL, /callback exchanges the code, /token refreshes the stored token.
// Tokens are persisted to SQLite via internal/server/oauth/store.go and
// encrypted at rest via internal/crypto envelope encryption.
package oauth

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/ory/fosite"
	"github.com/ory/fosite/compose"
	"github.com/ory/fosite/storage"

	"github.com/saker-ai/ctxhub/internal/config"
	"github.com/saker-ai/ctxhub/internal/observability"
)

// Server is the OpenViking OAuth 2.1 server. It owns the fosite
// provider, the in-memory client/token store, the persistent provider
// token store, and the per-route HTTP handlers. Methods satisfy
// routers.OAuthService.
type Server struct {
	cfg      *config.OAuthConfig
	provider fosite.OAuth2Provider
	store    *storage.MemoryStore
	hasher   fosite.Hasher
	issuer   string

	// basePath is the URL path prefix under which the OAuth endpoints
	// live. /api/v1/oauth is the canonical mount; .well-known lives at
	// the engine root. We use basePath to build fully-qualified URLs.
	basePath string

	// externalURL is the externally-reachable base URL (scheme://host)
	// used to build fully-qualified endpoint URLs in metadata responses.
	// When empty, the request's Host + scheme is used.
	externalURL string

	// tokenStore persists provider-issued access/refresh tokens,
	// encrypted at rest. nil when no DSN is configured (test-only).
	tokenStore *Store

	// providers maps provider name → adapter. Empty when no providers
	// are configured; the authorize/callback/token refresh handlers
	// return 400 in that case.
	providers map[string]Provider

	// metrics records oauthTokenRefreshTotal. nil in tests.
	metrics *observability.Metrics

	mu sync.RWMutex
}

// New constructs an OAuth server from config. The fosite provider is
// configured with PKCE enforcement, client_credentials grant,
// authorize-code grant, and token revocation. HMAC strategy is used
// for access tokens (opaque, not JWT); /oauth/keys therefore returns
// an empty JWKS — production replaces this with an RSA keypair for
// JWT-based tokens in a later phase.
func New(cfg *config.OAuthConfig, basePath, externalURL string) (*Server, error) {
	if cfg == nil {
		return nil, errors.New("oauth: nil config")
	}
	store := storage.NewMemoryStore()

	issuer := cfg.Issuer
	if issuer == "" {
		issuer = externalURL
	}

	fositeCfg := &fosite.Config{
		AccessTokenLifespan:         time.Hour,
		RefreshTokenLifespan:        30 * 24 * time.Hour,
		AuthorizeCodeLifespan:       15 * time.Minute,
		IDTokenLifespan:             time.Hour,
		AccessTokenIssuer:           issuer,
		IDTokenIssuer:               issuer,
		EnforcePKCE:                 true,
		EnforcePKCEForPublicClients: true,
		ScopeStrategy:               fosite.WildcardScopeStrategy,
		MinParameterEntropy:         fosite.MinParameterEntropy,
		GlobalSecret:                []byte("openviking-oauth-global-secret-change-me"),
		SendDebugMessagesToClients:  false,
	}

	provider := compose.Compose(
		fositeCfg,
		store,
		&compose.CommonStrategy{
			CoreStrategy:               compose.NewOAuth2HMACStrategy(fositeCfg),
			OpenIDConnectTokenStrategy: nil, // OIDC lands later
			Signer:                     nil,
		},
		compose.OAuth2ClientCredentialsGrantFactory,
		compose.OAuth2TokenRevocationFactory,
		compose.OAuth2AuthorizeExplicitFactory,
		compose.OAuth2PKCEFactory,
	)

	srv := &Server{
		cfg:         cfg,
		provider:    provider,
		store:       store,
		hasher:      &fosite.BCrypt{Config: fositeCfg},
		issuer:      issuer,
		basePath:    basePath,
		externalURL: externalURL,
		providers:   BuildProviders(providerConfigsFromConfig(cfg)),
	}

	// Seed configured clients into the in-memory store.
	for _, c := range cfg.Clients {
		if err := srv.seedClient(c); err != nil {
			return nil, err
		}
	}

	return srv, nil
}

// SetTokenStore wires the persistent provider-token store. Callers
// (e.g. app.go) construct the store with the right DSN + encryptor +
// metrics and inject it after New(). When not called, the
// authorize/callback/refresh flow returns 503 UNAVAILABLE.
func (s *Server) SetTokenStore(store *Store) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokenStore = store
}

// SetMetrics wires the Prometheus metrics recorder. Required for
// oauthTokenRefreshTotal to increment on refresh attempts.
func (s *Server) SetMetrics(m *observability.Metrics) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.metrics = m
}

// TokenStore returns the persistent store (nil when not wired). Exposed
// for tests.
func (s *Server) TokenStore() *Store {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.tokenStore
}

// Providers returns the registered provider adapters. Exposed for tests.
func (s *Server) Providers() map[string]Provider {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]Provider, len(s.providers))
	for k, v := range s.providers {
		out[k] = v
	}
	return out
}

// providerConfigsFromConfig converts the config-layer ProviderConfig
// list to the oauth package's internal ProviderConfig. Kept as a
// helper so the oauth package depends on config only for the top-level
// OAuthConfig type, not for the per-provider shape.
func providerConfigsFromConfig(cfg *config.OAuthConfig) []ProviderConfig {
	if cfg == nil || len(cfg.Providers) == 0 {
		return nil
	}
	out := make([]ProviderConfig, 0, len(cfg.Providers))
	for _, p := range cfg.Providers {
		out = append(out, ProviderConfig{
			Name:         p.Name,
			ClientID:     p.ClientID,
			ClientSecret: p.ClientSecret,
			RedirectURIs: p.RedirectURIs,
			Scopes:       p.Scopes,
			AuthURL:      p.AuthURL,
			TokenURL:     p.TokenURL,
			UserInfoURL:  p.UserInfoURL,
		})
	}
	return out
}

// seedClient inserts a config.ClientConfig into the memory store as a
// fosite.DefaultClient. The secret is bcrypt-hashed; public clients
// store no secret.
func (s *Server) seedClient(c config.ClientConfig) error {
	dc := &fosite.DefaultClient{
		ID:            c.ID,
		RedirectURIs:  c.Redirects,
		GrantTypes:    c.GrantTypes,
		ResponseTypes: []string{"code"},
		Scopes:        c.Scopes,
		Audience:      nil,
		Public:        c.Public,
	}
	if !c.Public && c.Secret != "" {
		hashed, err := s.hasher.Hash(nil, []byte(c.Secret))
		if err != nil {
			return err
		}
		dc.Secret = hashed
	}
	s.store.Clients[c.ID] = dc
	return nil
}

// CreateClient registers a new client (DCR). It is safe for concurrent
// use. The returned ClientResponse carries the plaintext secret; the
// caller must surface it to the client exactly once.
func (s *Server) CreateClient(req RegisterRequest) (*ClientResponse, error) {
	if req.ID != "" {
		s.mu.RLock()
		_, exists := s.store.Clients[req.ID]
		s.mu.RUnlock()
		if exists {
			return nil, ErrClientIDConflict
		}
	}
	if req.ID == "" {
		id, err := generateClientID()
		if err != nil {
			return nil, err
		}
		req.ID = id
	}

	public := req.TokenEndpointAuthMethod == "none"
	plaintext := ""
	if !public {
		var err error
		plaintext, err = generateClientSecret()
		if err != nil {
			return nil, err
		}
	}

	dc := &fosite.DefaultClient{
		ID:            req.ID,
		RedirectURIs:  req.RedirectURIs,
		GrantTypes:    defaultGrantTypes(req.GrantTypes, public),
		ResponseTypes: []string{"code"},
		Scopes:        req.Scopes,
		Audience:      req.Audience,
		Public:        public,
	}

	if !public {
		hashed, err := s.hasher.Hash(nil, []byte(plaintext))
		if err != nil {
			return nil, err
		}
		dc.Secret = hashed
	}

	s.mu.Lock()
	s.store.Clients[req.ID] = dc
	s.mu.Unlock()

	resp := &ClientResponse{
		ID:                      dc.ID,
		Secret:                  plaintext,
		RedirectURIs:            dc.RedirectURIs,
		GrantTypes:              dc.GrantTypes,
		ResponseTypes:           dc.ResponseTypes,
		Scopes:                  dc.Scopes,
		Audience:                dc.Audience,
		TokenEndpointAuthMethod: req.TokenEndpointAuthMethod,
		ClientName:              req.ClientName,
		ClientURI:               req.ClientURI,
		IssuedAt:                time.Now().UTC(),
	}
	if !public {
		resp.SecretIssuedAt = resp.IssuedAt
	}
	return resp, nil
}

// Provider returns the underlying fosite OAuth2Provider. Exposed for
// tests and for routers that need to call introspection directly.
func (s *Server) Provider() fosite.OAuth2Provider { return s.provider }

// Store returns the underlying memory store. Exposed for tests.
func (s *Server) Store() *storage.MemoryStore { return s.store }

// Issuer returns the configured issuer URL.
func (s *Server) Issuer() string { return s.issuer }

// TokenURL returns the fully-qualified token endpoint URL.
func (s *Server) TokenURL() string {
	return s.resolve(s.basePath + "/token")
}

// AuthorizeURL returns the fully-qualified authorize endpoint URL.
func (s *Server) AuthorizeURL() string {
	return s.resolve(s.basePath + "/authorize")
}

// RegisterURL returns the fully-qualified DCR endpoint URL.
func (s *Server) RegisterURL() string {
	return s.resolve(s.basePath + "/register")
}

// RevokeURL returns the fully-qualified revocation endpoint URL.
func (s *Server) RevokeURL() string {
	return s.resolve(s.basePath + "/revoke")
}

// JWKSURL returns the fully-qualified JWKS endpoint URL.
func (s *Server) JWKSURL() string {
	return s.resolve(s.basePath + "/keys")
}

// resolve builds a fully-qualified URL from a path. When externalURL
// is set, it is used as the prefix; otherwise the path is returned
// verbatim and the caller (HTTP layer) fills in the host.
func (s *Server) resolve(path string) string {
	if s.externalURL == "" {
		return path
	}
	base := strings.TrimRight(s.externalURL, "/")
	return base + path
}

// newSession returns a fosite.DefaultSession carrying the subject.
// Used by the token endpoint handler when minting client_credentials
// tokens.
func newSession(subject string) *fosite.DefaultSession {
	return &fosite.DefaultSession{
		Subject: subject,
		Extra:   map[string]interface{}{},
	}
}

// defaultGrantTypes returns the grant types to assign to a newly
// registered client. When the caller specifies none, the OAuth 2.1
// default of "authorization_code" is used for public clients and
// "client_credentials" is added for confidential clients.
func defaultGrantTypes(req []string, public bool) []string {
	if len(req) > 0 {
		return req
	}
	if public {
		return []string{"authorization_code", "refresh_token"}
	}
	return []string{"client_credentials", "authorization_code", "refresh_token"}
}

// generateClientID returns a random hex client identifier. The "ov_"
// prefix distinguishes OpenViking-issued clients from externally
// provisioned ones in logs.
func generateClientID() (string, error) {
	b, err := randomBytes(16)
	if err != nil {
		return "", err
	}
	return "ov_" + hex.EncodeToString(b), nil
}

// generateClientSecret returns a random hex secret. The caller must
// surface it to the client exactly once; only the hash is stored.
func generateClientSecret() (string, error) {
	b, err := randomBytes(32)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// randomBytes returns n crypto-strong random bytes.
func randomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return b, nil
}
