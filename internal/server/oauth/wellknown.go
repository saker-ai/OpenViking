package oauth

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// AuthorizationServerMetadataHandler serves RFC 8414 metadata at
// /.well-known/oauth-authorization-server. The response advertises
// the OpenViking OAuth 2.1 endpoints and capabilities.
func (s *Server) AuthorizationServerMetadataHandler(c *gin.Context) {
	meta := AuthorizationServerMetadata{
		Issuer:                            s.issuer,
		AuthorizationEndpoint:             s.AuthorizeURL(),
		TokenEndpoint:                     s.TokenURL(),
		RegistrationEndpoint:              s.RegisterURL(),
		RevocationEndpoint:                s.RevokeURL(),
		JWKSURI:                           s.JWKSURL(),
		ResponseTypesSupported:            []string{"code", "code id_token", "none"},
		GrantTypesSupported:               []string{"authorization_code", "client_credentials", "refresh_token"},
		CodeChallengeMethodsSupported:     []string{"S256"},
		TokenEndpointAuthMethodsSupported: []string{"client_secret_basic", "client_secret_post", "none"},
		ScopesSupported:                   []string{"openid", "offline", "fosite", "openviking"},
	}
	c.JSON(http.StatusOK, meta)
}

// ProtectedResourceMetadataHandler serves RFC 9728 metadata at
// /.well-known/oauth-protected-resource. The response advertises
// the OpenViking resource server identity and the authorization
// servers it trusts.
func (s *Server) ProtectedResourceMetadataHandler(c *gin.Context) {
	meta := ProtectedResourceMetadata{
		Resource:               s.issuer,
		AuthorizationServers:   []string{s.issuer},
		BearerMethodsSupported: []string{"header"},
	}
	c.JSON(http.StatusOK, meta)
}
