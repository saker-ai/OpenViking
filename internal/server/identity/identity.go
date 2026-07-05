// Package identity resolves the multi-tenant identity carried by request
// headers X-OpenViking-Account / User / Actor-Peer and propagates it through
// the request context.
//
// The middleware form (gin handler) is in middleware.go; the pure context
// helpers here are usable by any caller, including non-gin code paths.
package identity

import (
	"context"
	"net/http"

	"github.com/saker-ai/ctxhub/internal/domain"
)

type contextKey struct{}

var ctxKey = contextKey{}

// HeaderAccount / HeaderUser / HeaderActorPeer are the canonical request
// header names that carry identity.
const (
	HeaderAccount   = "X-OpenViking-Account"
	HeaderUser      = "X-OpenViking-User"
	HeaderActorPeer = "X-OpenViking-Actor-Peer"
)

// FromContext returns the Identifier stored in ctx, if any.
func FromContext(ctx context.Context) (domain.Identifier, bool) {
	id, ok := ctx.Value(ctxKey).(domain.Identifier)
	return id, ok
}

// WithIdentity returns a new ctx carrying id.
func WithIdentity(ctx context.Context, id domain.Identifier) context.Context {
	return context.WithValue(ctx, ctxKey, id)
}

// FromHeaders extracts an Identifier from request headers.
func FromHeaders(h http.Header) domain.Identifier {
	return domain.Identifier{
		Account:   h.Get(HeaderAccount),
		User:      h.Get(HeaderUser),
		ActorPeer: h.Get(HeaderActorPeer),
	}
}
