// Copyright (c) 2026 Beijing Volcano Engine Technology Co., Ltd.
// SPDX-License-Identifier: AGPL-3.0

package server

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/config"
	"github.com/saker-ai/ctxhub/internal/domain"
)

func newGuard(t *testing.T, roots []string) *LocalInputGuard {
	t.Helper()
	if roots == nil {
		roots = []string{"./data/", "./tmp/"}
	}
	return NewLocalInputGuard(config.LocalInputGuardConfig{AllowedRoots: roots})
}

func TestLocalInputGuard_BlocksTraversal(t *testing.T) {
	g := newGuard(t, nil)
	for _, src := range []string{
		"../etc/passwd",
		"./data/../foo",
		"./data/foo/../../bar",
		"data\\..\\bar",
	} {
		err := g.Check(src)
		require.Error(t, err, "expected %q to be blocked", src)
		assert.True(t, errors.Is(err, domain.ErrValidation),
			"traversal %q should be ErrValidation, got %v", src, err)
	}
}

func TestLocalInputGuard_BlocksBlockedHostPaths(t *testing.T) {
	g := newGuard(t, nil)
	for _, src := range []string{
		"/etc/passwd",
		"/etc",
		"/var/log/syslog",
		"/root/.bashrc",
		"/home/alice/secret",
	} {
		err := g.Check(src)
		require.Error(t, err, "expected %q to be blocked", src)
		assert.True(t, errors.Is(err, domain.ErrValidation),
			"blocked host path %q should be ErrValidation, got %v", src, err)
	}
}

func TestLocalInputGuard_BlocksFileURLOutsideRoots(t *testing.T) {
	g := newGuard(t, nil)
	err := g.Check("file:///etc/shadow")
	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrValidation), "file URL host path should be ErrValidation, got %v", err)
}

func TestLocalInputGuard_BlocksFileURLWithRemoteHost(t *testing.T) {
	g := newGuard(t, nil)
	err := g.Check("file://evil.host/etc/shadow")
	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrValidation), "file URL with remote host should be blocked")
}

func TestLocalInputGuard_BlocksHTTPPrivateIP(t *testing.T) {
	g := newGuard(t, nil)
	for _, src := range []string{
		"http://127.0.0.1/",
		"http://127.0.0.1:8080/",
		"https://10.0.0.1/",
		"http://192.168.1.1/",
		"http://172.16.0.1/",
		"http://169.254.169.254/latest/meta-data/", // link-local cloud metadata
		"http://[::1]/",                            // IPv6 loopback
		"http://[fc00::1]/",                        // IPv6 private
		"http://0.0.0.0/",                          // unspecified
	} {
		err := g.Check(src)
		require.Error(t, err, "expected %q to be blocked", src)
		assert.True(t, errors.Is(err, domain.ErrValidation),
			"private IP %q should be ErrValidation, got %v", src, err)
	}
}

func TestLocalInputGuard_AllowsPublicHTTP(t *testing.T) {
	g := newGuard(t, nil)
	for _, src := range []string{
		"https://example.com/",
		"http://example.com/path?q=1",
		"https://1.1.1.1/",            // public IP
		"https://8.8.8.8/dns",         // public DNS resolver
	} {
		require.NoError(t, g.Check(src), "expected %q to be allowed", src)
	}
}

func TestLocalInputGuard_AllowsDataPath(t *testing.T) {
	g := newGuard(t, nil)
	for _, src := range []string{
		"./data/foo.txt",
		"./data/nested/deep.txt",
		"./tmp/scratch.bin",
		"./data/file.with.dots.txt",
	} {
		require.NoError(t, g.Check(src), "expected %q to be allowed", src)
	}
}

func TestLocalInputGuard_BlocksOutsideRoots(t *testing.T) {
	g := newGuard(t, nil)
	for _, src := range []string{
		"./secret/creds",
		"./data-evil/file.txt", // prefix-cousin; must not match "./data/"
		"/opt/data/x",
	} {
		err := g.Check(src)
		require.Error(t, err, "expected %q to be blocked", src)
		assert.True(t, errors.Is(err, domain.ErrValidation), "outside-roots %q should be ErrValidation, got %v", src, err)
	}
}

func TestLocalInputGuard_CustomAllowedRoots(t *testing.T) {
	g := newGuard(t, []string{"/var/openviking/data"})
	require.NoError(t, g.Check("/var/openviking/data/foo.txt"))
	err := g.Check("./data/foo.txt")
	require.Error(t, err, "with custom roots, default ./data/ should be blocked")
}

func TestLocalInputGuard_NilGuard_Allows(t *testing.T) {
	// Nil guard must be always-allow so the MCP handler stays nil-safe.
	var g *LocalInputGuard
	require.NoError(t, g.Check("./data/x"))
	require.NoError(t, g.Check("https://example.com/"))
}

func TestLocalInputGuard_EmptySource(t *testing.T) {
	g := newGuard(t, nil)
	err := g.Check("")
	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrValidation))
}

func TestLocalInputGuard_IsLocalInputError(t *testing.T) {
	g := newGuard(t, nil)
	err := g.Check("/etc/passwd")
	require.Error(t, err)
	assert.True(t, IsLocalInputError(err), "IsLocalInputError should be true for guard errors")

	// A non-validation AppError must not match.
	assert.False(t, IsLocalInputError(domain.ErrNotFound))
}
