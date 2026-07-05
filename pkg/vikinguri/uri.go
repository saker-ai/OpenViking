// Package vikinguri parses and renders viking:// resource identifiers used
// throughout OpenViking to address ragfs resources, sessions, skills, and
// relations across multi-tenant namespaces.
//
// URI grammar
//
//	viking://<scope>/<kind>[/<...path>][?<query>]
//
// Where:
//
//   - scope is "agent" for the system namespace, or "user_<id>" for a
//     user-scoped namespace. Other scopes (e.g. "org_<id>") are reserved.
//   - kind is one of: resources, sessions, skills, relations.
//   - path is a POSIX-like path with leading slash, percent-decoded by the
//     caller before resource lookup.
//   - query carries optional filters (e.g. ?layer=abstract).
//
// Examples:
//
//	viking://agent/skills
//	viking://agent/resources/docs/intro.md
//	viking://user_42/sessions/sess_abc123
package vikinguri

import (
	"fmt"
	"net/url"
	"strings"
)

// Kind enumerates the resource kinds addressable by a viking URI.
type Kind string

const (
	KindResources Kind = "resources"
	KindSessions  Kind = "sessions"
	KindSkills    Kind = "skills"
	KindRelations Kind = "relations"
)

// Scope is the namespace scope of a viking URI.
type Scope struct {
	Type   string // "agent" or "user"
	UserID string // populated when Type == "user"
}

// String renders the scope as it appears in a URI.
func (s Scope) String() string {
	switch s.Type {
	case "agent":
		return "agent"
	case "user":
		if s.UserID == "" {
			return "user"
		}
		return "user_" + s.UserID
	}
	return s.Type
}

// IsAgent reports whether the scope is the system agent namespace.
func (s Scope) IsAgent() bool { return s.Type == "agent" }

// URI is a parsed viking:// identifier.
type URI struct {
	Scope Scope
	Kind  Kind
	Path  string // starts with "/" or is "" for collection root
	Query url.Values
}

// Parse parses a viking:// URI string.
func Parse(s string) (*URI, error) {
	if !strings.HasPrefix(s, "viking://") {
		return nil, fmt.Errorf("vikinguri: %q missing viking:// scheme", s)
	}
	u, err := url.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("vikinguri: parse %q: %w", s, err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("vikinguri: %q missing scope", s)
	}
	scope, err := parseScope(u.Host)
	if err != nil {
		return nil, err
	}
	kind, err := parseKind(u.Path)
	if err != nil {
		return nil, err
	}
	path := strings.TrimPrefix(u.Path, "/"+string(kind))
	if path != "" && !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return &URI{
		Scope: scope,
		Kind:  kind,
		Path:  path,
		Query: u.Query(),
	}, nil
}

// String renders the URI in canonical form.
func (u *URI) String() string {
	var b strings.Builder
	b.WriteString("viking://")
	b.WriteString(u.Scope.String())
	b.WriteByte('/')
	b.WriteString(string(u.Kind))
	if u.Path != "" {
		b.WriteString(u.Path)
	}
	if len(u.Query) > 0 {
		b.WriteByte('?')
		b.WriteString(u.Query.Encode())
	}
	return b.String()
}

// Normalize returns the canonical String form of the input.
func Normalize(s string) (string, error) {
	u, err := Parse(s)
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

func parseScope(host string) (Scope, error) {
	switch {
	case host == "agent":
		return Scope{Type: "agent"}, nil
	case strings.HasPrefix(host, "user_"):
		return Scope{Type: "user", UserID: strings.TrimPrefix(host, "user_")}, nil
	case host == "user":
		return Scope{Type: "user"}, nil
	}
	return Scope{}, fmt.Errorf("vikinguri: unknown scope %q", host)
}

func parseKind(path string) (Kind, error) {
	trimmed := strings.TrimPrefix(path, "/")
	if trimmed == "" {
		return "", fmt.Errorf("vikinguri: missing kind in path %q", path)
	}
	first := strings.SplitN(trimmed, "/", 2)[0]
	switch Kind(first) {
	case KindResources, KindSessions, KindSkills, KindRelations:
		return Kind(first), nil
	}
	return "", fmt.Errorf("vikinguri: unknown kind %q", first)
}
