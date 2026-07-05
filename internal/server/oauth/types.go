package oauth

import (
	"errors"
	"time"
)

// Sentinel errors for DCR and client management.
var (
	// ErrClientIDConflict is returned when a DCR request specifies a
	// client_id that already exists in the store.
	ErrClientIDConflict = errors.New("oauth: client_id already exists")

	// ErrInvalidRedirectURI is returned when a DCR request specifies a
	// redirect_uri that is not a valid absolute URI.
	ErrInvalidRedirectURI = errors.New("oauth: invalid redirect_uri")

	// ErrClientNotFound is returned when a client lookup misses.
	ErrClientNotFound = errors.New("oauth: client not found")
)

// RegisterRequest models the JSON body of a POST /oauth/register
// request (RFC 7591). Field names mirror the RFC metadata keys so
// the JSON encoder/decoder round-trips without explicit tags.
type RegisterRequest struct {
	ID            string   `json:"client_id,omitempty"`
	Secret        string   `json:"client_secret,omitempty"`
	ClientName    string   `json:"client_name,omitempty"`
	ClientURI     string   `json:"client_uri,omitempty"`
	RedirectURIs  []string `json:"redirect_uris,omitempty"`
	GrantTypes    []string `json:"grant_types,omitempty"`
	ResponseTypes []string `json:"response_types,omitempty"`
	Scopes        []string `json:"scope,omitempty"`
	Audience      []string `json:"audience,omitempty"`
	// TokenEndpointAuthMethod mirrors RFC 7591's metadata field. When
	// set to "none" the client is treated as public (no secret).
	TokenEndpointAuthMethod string `json:"token_endpoint_auth_method,omitempty"`
}

// ClientResponse is the JSON body returned to a DCR client (RFC 7591
// §3). The plaintext client_secret is included exactly once on
// creation; subsequent reads return an empty secret.
type ClientResponse struct {
	ID                      string    `json:"client_id"`
	Secret                  string    `json:"client_secret,omitempty"`
	ClientName              string    `json:"client_name,omitempty"`
	ClientURI               string    `json:"client_uri,omitempty"`
	RedirectURIs            []string  `json:"redirect_uris,omitempty"`
	GrantTypes              []string  `json:"grant_types,omitempty"`
	ResponseTypes           []string  `json:"response_types,omitempty"`
	Scopes                  []string  `json:"scope,omitempty"`
	Audience                []string  `json:"audience,omitempty"`
	TokenEndpointAuthMethod string    `json:"token_endpoint_auth_method,omitempty"`
	IssuedAt                time.Time `json:"client_id_issued_at"`
	SecretIssuedAt          time.Time `json:"client_secret_issued_at,omitempty"`
}

// AuthorizationServerMetadata is the RFC 8414 metadata document served
// at /.well-known/oauth-authorization-server. Only the fields the
// OpenViking server populates are modelled here.
type AuthorizationServerMetadata struct {
	Issuer                            string   `json:"issuer"`
	AuthorizationEndpoint             string   `json:"authorization_endpoint"`
	TokenEndpoint                     string   `json:"token_endpoint"`
	RegistrationEndpoint              string   `json:"registration_endpoint"`
	RevocationEndpoint                string   `json:"revocation_endpoint"`
	JWKSURI                           string   `json:"jwks_uri"`
	ResponseTypesSupported            []string `json:"response_types_supported"`
	GrantTypesSupported               []string `json:"grant_types_supported"`
	CodeChallengeMethodsSupported     []string `json:"code_challenge_methods_supported"`
	TokenEndpointAuthMethodsSupported []string `json:"token_endpoint_auth_methods_supported"`
	ScopesSupported                   []string `json:"scopes_supported"`
}

// ProtectedResourceMetadata is the RFC 9728 metadata document served
// at /.well-known/oauth-protected-resource.
type ProtectedResourceMetadata struct {
	Resource               string   `json:"resource"`
	AuthorizationServers   []string `json:"authorization_servers"`
	BearerMethodsSupported []string `json:"bearer_methods_supported"`
}

// JWKS is the JSON Web Key Set served at /oauth/keys. For the HMAC
// strategy used in this phase the set is empty; production replaces
// this with the RSA public signing key.
type JWKS struct {
	Keys []map[string]any `json:"keys"`
}

// AuthorizeRequest models the query parameters accepted by
// GET /api/v1/oauth/authorize. The provider parameter selects the
// external OAuth provider (feishu / google / slack / dingtalk). When
// absent, the request is treated as a fosite authorize-code flow
// request (which is not yet implemented; the handler returns 400).
type AuthorizeRequest struct {
	Provider       string `form:"provider"`
	RedirectURI    string `form:"redirect_uri"`
	State          string `form:"state"`
	CodeChallenge  string `form:"code_challenge"`
	ResponseType   string `form:"response_type"`
	ClientID       string `form:"client_id"`
	Scope          string `form:"scope"`
}

// CallbackRequest models the query parameters accepted by
// GET /api/v1/oauth/callback. The provider is required to dispatch to
// the right adapter; state ties the response back to the original
// authorize request.
type CallbackRequest struct {
	Provider string `form:"provider"`
	Code     string `form:"code"`
	State    string `form:"state"`
	Error    string `form:"error"`
}

// RefreshTokenRequest models the form body of POST /api/v1/oauth/token
// when grant_type=refresh_token. The provider field is required; the
// refresh_token is the opaque provider-issued refresh token.
type RefreshTokenRequest struct {
	GrantType    string `form:"grant_type"`
	Provider     string `form:"provider"`
	RefreshToken string `form:"refresh_token"`
	Scope        string `form:"scope"`
}

// TokenResponse is the JSON body returned by /api/v1/oauth/token on a
// successful refresh. Mirrors the OAuth 2.1 token response shape so
// existing OAuth clients can consume it without changes.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token,omitempty"`
	Scope        string `json:"scope,omitempty"`
	Provider     string `json:"provider,omitempty"`
}
