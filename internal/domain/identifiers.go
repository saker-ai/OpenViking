// Package domain defines the core domain models of OpenViking.
//
// These types are the lingua franca exchanged between HTTP routers, services,
// ragfs, vectordb, queuefs, ingest, retrieve, and session layers. They carry
// no external dependencies beyond the standard library so they can be imported
// by every internal package without cycles.
package domain

// Identifier carries the multi-tenant identity resolved from request headers
// X-OpenViking-Account / User / Actor-Peer. It is propagated through context
// by internal/server/identity and used to scope every ragfs / vectordb call.
type Identifier struct {
	Account   string `json:"account"`
	User      string `json:"user,omitempty"`
	ActorPeer string `json:"actor_peer,omitempty"`
}

// IsEmpty reports whether the identifier carries no account.
func (i Identifier) IsEmpty() bool { return i.Account == "" }

// String returns a compact "account/user/peer" representation for logs.
func (i Identifier) String() string {
	if i.User == "" && i.ActorPeer == "" {
		return i.Account
	}
	return i.Account + "/" + i.User + "/" + i.ActorPeer
}
