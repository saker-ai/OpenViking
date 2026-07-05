// Copyright (c) 2026 Beijing Volcano Engine Technology Co., Ltd.
// SPDX-License-Identifier: AGPL-3.0

// Package server local_input_guard.go: blocks HTTP paths from directly
// accessing host filesystem (path traversal / SSRF protection). Mirrors
// the Python local_input_guard module in openviking/server/.
//
// The guard is applied to every resource source passed to the MCP
// add_resource handler (and any other handler that fetches a resource
// by user-supplied location) before the server touches the host
// filesystem or opens a network connection.
package server

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/saker-ai/ctxhub/internal/config"
	"github.com/saker-ai/ctxhub/internal/domain"
)

// blockedHostPathPrefixes are absolute host paths the HTTP server must
// never read from on behalf of a remote agent. Mirrors the Python
// denylist in local_input_guard.py.
var blockedHostPathPrefixes = []string{
	"/etc/",
	"/var/",
	"/root/",
	"/home/",
}

// LocalInputGuard blocks resource sources that would let an HTTP client
// read from the host filesystem or pivot to internal services. The
// guard accepts:
//
//   - Local filesystem paths (e.g. "./data/foo.txt"): allowed only when
//     they do not contain ".." traversal segments and resolve under one
//     of the configured AllowedRoots.
//   - file:// URLs: the inner path is treated as a local filesystem
//     path and run through the same checks.
//   - http:// / https:// URLs: the host is parsed; if it is a literal
//     IP, the IP is rejected when it is private (RFC1918), loopback,
//     link-local unicast, or link-local multicast. Domain hosts are
//     allowed (DNS resolution is the deployment's responsibility).
//
// The zero value is NOT safe to use; construct via NewLocalInputGuard.
type LocalInputGuard struct {
	cfg            config.LocalInputGuardConfig
	allowedRootsAbs []string // pre-resolved absolute paths
}

// NewLocalInputGuard constructs a guard from cfg. AllowedRoots are
// resolved to absolute paths at construction time so checks during
// request handling are pure string-prefix comparisons. Empty
// AllowedRoots falls back to the defaults ["./data/", "./tmp/"].
func NewLocalInputGuard(cfg config.LocalInputGuardConfig) *LocalInputGuard {
	roots := cfg.AllowedRoots
	if len(roots) == 0 {
		roots = []string{"./data/", "./tmp/"}
	}
	abs := make([]string, 0, len(roots))
	for _, r := range roots {
		if a, err := filepath.Abs(filepath.Clean(r)); err == nil {
			abs = append(abs, a)
		}
	}
	return &LocalInputGuard{cfg: cfg, allowedRootsAbs: abs}
}

// Config returns the resolved configuration. Useful for tests.
func (g *LocalInputGuard) Config() config.LocalInputGuardConfig { return g.cfg }

// Check returns domain.ErrValidation when source violates any guard.
// Nil guard is treated as always-allow so the MCP handler stays
// nil-safe in degraded deployments (mirrors the existing pattern).
func (g *LocalInputGuard) Check(source string) error {
	if g == nil {
		return nil
	}
	if source == "" {
		return fmt.Errorf("%w: source is required", domain.ErrValidation)
	}
	switch {
	case strings.HasPrefix(source, "file://"):
		return g.checkFileURL(source)
	case strings.HasPrefix(source, "http://"), strings.HasPrefix(source, "https://"):
		return g.checkHTTPURL(source)
	default:
		return g.checkLocalPath(source)
	}
}

// checkLocalPath enforces the path-traversal rule first, then lets
// explicitly-configured allowed roots override the blocked-prefix
// denylist (so an operator can whitelist e.g. /var/openviking/data),
// then falls back to the denylist + allowed-roots check.
func (g *LocalInputGuard) checkLocalPath(p string) error {
	if containsTraversal(p) {
		return fmt.Errorf("%w: path traversal sequences are not allowed", domain.ErrValidation)
	}
	if g.underAllowedRoot(p) {
		return nil
	}
	for _, prefix := range blockedHostPathPrefixes {
		if strings.HasPrefix(p, prefix) || p == strings.TrimSuffix(prefix, "/") {
			return fmt.Errorf("%w: host path %q is not allowed", domain.ErrValidation, p)
		}
	}
	return fmt.Errorf("%w: path %q is outside allowed roots", domain.ErrValidation, p)
}

// checkFileURL parses a file:// URL and applies the local-path checks
// to its interior path.
func (g *LocalInputGuard) checkFileURL(source string) error {
	u, err := url.Parse(source)
	if err != nil {
		return fmt.Errorf("%w: invalid file URL: %v", domain.ErrValidation, err)
	}
	if u.Host != "" && u.Host != "localhost" {
		return fmt.Errorf("%w: file URLs with remote hosts are not allowed", domain.ErrValidation)
	}
	if u.Path == "" {
		return fmt.Errorf("%w: file URL is missing a path", domain.ErrValidation)
	}
	return g.checkLocalPath(u.Path)
}

// checkHTTPURL parses an http(s):// URL and rejects hosts that resolve
// to private IP ranges. Literal IPs are checked directly; domain names
// are allowed (DNS-level SSRF protection is the deployment's job, same
// as the Python ensure_public_remote_target).
func (g *LocalInputGuard) checkHTTPURL(source string) error {
	u, err := url.Parse(source)
	if err != nil {
		return fmt.Errorf("%w: invalid URL: %v", domain.ErrValidation, err)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("%w: URL is missing a host", domain.ErrValidation)
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return nil // domain host; allow.
	}
	if isPrivateIP(ip) {
		return fmt.Errorf("%w: URL host %s is a private IP range", domain.ErrValidation, host)
	}
	return nil
}

// underAllowedRoot returns true when p (after cleaning and absolute
// resolution) is rooted under one of the configured AllowedRoots.
func (g *LocalInputGuard) underAllowedRoot(p string) bool {
	abs, err := filepath.Abs(filepath.Clean(p))
	if err != nil {
		return false
	}
	for _, root := range g.allowedRootsAbs {
		if abs == root {
			return true
		}
		if strings.HasPrefix(abs, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// containsTraversal returns true when any path component (separated by
// '/' or '\') equals "..". This catches "../etc", "./data/../foo",
// and "data\\..\\bar" without false-matching "foo..bar.txt".
func containsTraversal(p string) bool {
	for _, part := range strings.FieldsFunc(p, func(r rune) bool {
		return r == '/' || r == '\\'
	}) {
		if part == ".." {
			return true
		}
	}
	return false
}

// isPrivateIP reports whether ip is in a range that must never be the
// target of an HTTP fetch initiated on behalf of a remote agent:
// RFC1918 private, loopback, link-local unicast, link-local multicast,
// or unspecified. Uses stdlib netip.Addr predicates (no new dep).
func isPrivateIP(ip netip.Addr) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return true
	}
	return false
}

// IsLocalInputError reports whether err was produced by a
// LocalInputGuard check. Convenience for callers that want to map
// guard failures to a 422 instead of a generic 500.
func IsLocalInputError(err error) bool {
	return errors.Is(err, domain.ErrValidation)
}
