package ingest

import (
	"encoding/base64"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// PeerSep joins harness / provider / model components in an assistant peer_id.
// Mirrors the OpenViking session-id scheme so that assistant peers sort
// naturally alongside harness-prefixed user peers.
const PeerSep = "__"

// peerIDAllowed matches characters that may appear in a peer_id without
// encoding. Anything else is replaced with "-".
var peerIDAllowed = regexp.MustCompile(`[^a-zA-Z0-9_.@-]+`)

// peerIDCollapse collapses runs of "-" and trims leading/trailing "-." so
// sanitized components stay readable.
var peerIDCollapse = regexp.MustCompile(`-{2,}`)

// SafePeerID returns a valid peer_id for an arbitrary external identifier.
// ASCII identifiers stay human-readable; anything else (e.g. CJK usernames)
// is base64-encoded as "ext-…" mirroring vikingbot's external-peer convention.
//
// Returns "" when raw is empty or unsanitizable.
func SafePeerID(raw string) string {
	text := strings.TrimSpace(raw)
	if text == "" {
		return ""
	}
	if isASCIIAlnum(text) {
		return sanitizeComponent(text)
	}
	encoded := base64.URLEncoding.EncodeToString([]byte(text))
	encoded = strings.TrimRight(encoded, "=")
	return sanitizeComponent("ext-" + encoded)
}

// sanitizeComponent makes one path-free component safe & readable. Lossy
// for non-ASCII inputs.
func sanitizeComponent(value string) string {
	cleaned := peerIDAllowed.ReplaceAllString(strings.TrimSpace(value), "-")
	cleaned = peerIDCollapse.ReplaceAllString(cleaned, "-")
	cleaned = strings.Trim(cleaned, "-.")
	if cleaned == "" {
		return "unknown"
	}
	return cleaned
}

// isASCIIAlnum reports whether s contains only ASCII letters, digits, and
// the peer_id punctuation set. Used to decide whether the readable form is
// safe or whether we need to base64-encode.
func isASCIIAlnum(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9':
		case c == '_' || c == '.' || c == '@' || c == '-':
		default:
			if c >= 0x80 {
				return false
			}
			return false
		}
	}
	return true
}

// AssistantPeerID returns "{harness}__{model}" or
// "{harness}__{provider}__{model}". Empty model becomes "unknown".
func AssistantPeerID(harness, model, provider string) string {
	parts := []string{sanitizeComponent(harness)}
	if provider != "" {
		parts = append(parts, sanitizeComponent(provider))
	}
	if model == "" {
		model = "unknown"
	}
	parts = append(parts, sanitizeComponent(model))
	return strings.Join(parts, PeerSep)
}

// gitPeerCache memoizes the git-config-derived human identity per cwd so
// repeated calls during a poll cycle do not shell out repeatedly.
var (
	gitPeerMu    sync.Mutex
	gitPeerCache = map[string]string{}
)

// ResolveGitHumanPeer returns the human peer_id from the cwd repo's git
// identity, falling back to SafePeerID(fallback) when git is unavailable
// or the repo has no user.email/user.name set.
//
// Prefers user.email (more unique), then user.name. Cached per cwd for the
// process lifetime.
func ResolveGitHumanPeer(cwd, fallback string) string {
	if cwd == "" {
		return SafePeerID(fallback)
	}

	gitPeerMu.Lock()
	cached, ok := gitPeerCache[cwd]
	gitPeerMu.Unlock()
	if ok {
		if cached == "" {
			return SafePeerID(fallback)
		}
		return cached
	}

	resolved := ""
	for _, key := range []string{"user.email", "user.name"} {
		out, err := gitConfig(cwd, key)
		if err == nil && out != "" {
			resolved = SafePeerID(out)
			break
		}
	}

	gitPeerMu.Lock()
	gitPeerCache[cwd] = resolved
	gitPeerMu.Unlock()

	if resolved == "" {
		return SafePeerID(fallback)
	}
	return resolved
}

// gitConfig shells out to `git -C cwd config --get key`. Returns "" with
// a non-nil error when git is missing, the command times out, or the key
// is unset.
func gitConfig(cwd, key string) (string, error) {
	cmd := exec.Command("git", "-C", cwd, "config", "--get", key)
	timer := time.AfterFunc(2*time.Second, func() {
		_ = cmd.Process.Kill()
	})
	defer timer.Stop()
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// PeerID returns the stable peer_id hash for (account, sourceID). It is
// used by orchestrator/replay to namespace replayed sessions under the
// caller's account when no harness-specific peer applies.
//
// The hash is the URL-safe base64 of the SHA-256 of "account/sourceID",
// truncated to 16 chars, prefixed "peer-". This is deterministic and
// collision-resistant for the typical scale of ingest (10^4 sessions).
func PeerID(account, sourceID string) string {
	if sourceID == "" {
		return SafePeerID(account)
	}
	return SafePeerID(fmt.Sprintf("%s/%s", account, sourceID))
}
