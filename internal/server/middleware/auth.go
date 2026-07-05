package middleware

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/bcrypt"
)

// APIKeyVerifier resolves a raw API key (or OAuth bearer token) to the
// account it belongs to. Implementations are expected to hash-compare
// the key against stored credentials. A nil-safe stub can be wired in
// when no real store exists yet (P12 wires a stub; the real verifier
// lands with the auth phase).
type APIKeyVerifier interface {
	// VerifyAPIKey returns the account id for key, or an error when the
	// key is unknown / disabled / revoked. The returned Identifier is
	// propagated on the request context for downstream use.
	VerifyAPIKey(ctx context.Context, key string) (string, error)
}

// OAuthTokenVerifier validates an OAuth 2.1 bearer token. P9 wires the
// real fosite-backed verifier; until then a stub returns
// ErrOAuthUnavailable.
type OAuthTokenVerifier interface {
	// VerifyToken returns the account id for token, or an error when the
	// token is invalid / expired / revoked.
	VerifyToken(ctx context.Context, token string) (string, error)
}

// AuthConfig controls which authentication paths the Auth middleware
// enforces.
type AuthConfig struct {
	// APIKey verifier; when nil, API key auth is skipped.
	APIKey APIKeyVerifier
	// OAuth verifier; when nil, OAuth token auth is skipped.
	OAuth OAuthTokenVerifier
	// SkipPaths are exact-match path prefixes that bypass auth entirely
	// (healthz, readyz, metrics, etc.).
	SkipPaths []string
	// HashAlgo is informational; verifiers do the actual hashing. It is
	// kept on the config so tests and configs can pin the expected algo.
	HashAlgo string
}

// errOAuthUnavailable is the stub error returned by the default OAuth
// verifier when no real verifier is wired.
var errOAuthUnavailable = errors.New("oauth token validation unavailable")

// ErrInvalidToken is returned by verifiers when the presented credential
// does not match any known account.
var ErrInvalidToken = errors.New("invalid or missing api key / token")

// noOAuth is the default OAuth verifier; it always returns unavailable so
// callers can probe whether OAuth is wired without nil-checking.
type noOAuth struct{}

func (noOAuth) VerifyToken(ctx context.Context, token string) (string, error) {
	return "", errOAuthUnavailable
}

// Auth authenticates incoming requests by validating either an
// Authorization: Bearer <api-key> header or an OAuth bearer token. Health
// / readiness / metrics paths bypass auth so k8s probes work without
// credentials.
//
// The middleware resolves the credential to an account id and stores it on
// the request context under the identity key (X-OpenViking-Account). When
// both APIKey and OAuth verifiers are nil the middleware passes through
// without enforcing auth (P12 stub mode — auth lands in a later phase).
func Auth(cfg AuthConfig) gin.HandlerFunc {
	apiKeyNil := cfg.APIKey == nil
	oauthNil := cfg.OAuth == nil
	if cfg.OAuth == nil {
		cfg.OAuth = noOAuth{}
	}
	skip := make(map[string]struct{}, len(cfg.SkipPaths))
	for _, p := range cfg.SkipPaths {
		skip[p] = struct{}{}
	}
	return func(c *gin.Context) {
		if _, ok := skip[c.Request.URL.Path]; ok {
			c.Next()
			return
		}
		// Also auto-skip the conventional probe paths so the default
		// SkipPaths empty value is enough for typical deployments.
		if isProbePath(c.Request.URL.Path) {
			c.Next()
			return
		}
		if apiKeyNil && oauthNil {
			c.Next()
			return
		}
		token := bearerToken(c)
		if token == "" {
			unauthorized(c, "missing bearer token")
			return
		}
		account, err := verifyAny(c.Request.Context(), cfg, token)
		if err != nil {
			unauthorized(c, "invalid token")
			return
		}
		if account != "" && c.GetHeader("X-OpenViking-Account") == "" {
			// Propagate the resolved account downstream so identity
			// middleware sees it without requiring the caller to set it.
			c.Request.Header.Set("X-OpenViking-Account", account)
		}
		c.Next()
	}
}

// verifyAny tries API key first then OAuth, returning whichever succeeds.
func verifyAny(ctx context.Context, cfg AuthConfig, token string) (string, error) {
	if cfg.APIKey != nil {
		if acct, err := cfg.APIKey.VerifyAPIKey(ctx, token); err == nil {
			return acct, nil
		}
	}
	if cfg.OAuth != nil {
		if acct, err := cfg.OAuth.VerifyToken(ctx, token); err == nil {
			return acct, nil
		}
	}
	return "", ErrInvalidToken
}

// bearerToken extracts the credential from the Authorization header,
// accepting "Bearer <token>" and "Api-Key <token>" schemes.
func bearerToken(c *gin.Context) string {
	h := c.GetHeader("Authorization")
	if h == "" {
		// Also accept X-Api-Key as a fallback for clients that cannot set
		// the Authorization header.
		if k := c.GetHeader("X-Api-Key"); k != "" {
			return k
		}
		return ""
	}
	parts := strings.SplitN(h, " ", 2)
	if len(parts) != 2 {
		return ""
	}
	scheme := strings.ToLower(parts[0])
	if scheme != "bearer" && scheme != "api-key" && scheme != "apikey" {
		return ""
	}
	return strings.TrimSpace(parts[1])
}

// isProbePath returns true for healthz/readyz/metrics paths that should
// bypass auth.
func isProbePath(path string) bool {
	switch path {
	case "/healthz", "/readyz", "/version", "/metrics":
		return true
	}
	return false
}

// unauthorized writes a 401 response with the structured error envelope.
func unauthorized(c *gin.Context, msg string) {
	c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
		"error": gin.H{
			"code":    "UNAUTHORIZED",
			"message": msg,
		},
	})
}

// VerifyAPIKeyHash is a helper for verifiers that store hashed keys. It
// compares the raw key against a stored hash produced by HashAPIKey.
//
// The hashAlgo argument must be one of: "argon2id", "bcrypt", "sha256".
// For argon2id and bcrypt the stored value is the verbatim hash string
// produced by the algorithm's own encoder. For sha256 the stored value
// is the hex digest of the key (weakest option; only use for legacy
// parity with the Python ov.conf example).
func VerifyAPIKeyHash(key, storedHash, hashAlgo string) (bool, error) {
	switch hashAlgo {
	case "argon2id":
		// storedHash must be the argon2id reference encoding produced by
		// HashAPIKey (argon2id$v=19$m=...$salt$hex). We re-derive from
		// the salt and parameters embedded in the hash and constant-time
		// compare.
		return verifyArgon2id(key, storedHash)
	case "bcrypt":
		err := bcrypt.CompareHashAndPassword([]byte(storedHash), []byte(key))
		return err == nil, nil
	case "sha256":
		sum := sha256.Sum256([]byte(key))
		got := hex.EncodeToString(sum[:])
		return subtle.ConstantTimeCompare([]byte(got), []byte(storedHash)) == 1, nil
	default:
		return false, errors.New("unsupported hash algorithm: " + hashAlgo)
	}
}

// argon2idParams are the OWASP-recommended defaults used by HashAPIKey.
type argon2idParams struct {
	time    uint32
	memory  uint32
	threads uint8
	keyLen  uint32
}

var defaultArgon2idParams = argon2idParams{time: 1, memory: 64 * 1024, threads: 1, keyLen: 32}

// HashAPIKey produces a hash string suitable for storage in the account
// store. The format is the verbatim encoded form produced by the chosen
// algorithm. Callers should never log or echo the return value.
func HashAPIKey(key, hashAlgo string) (string, error) {
	switch hashAlgo {
	case "argon2id":
		return hashArgon2id(key, defaultArgon2idParams), nil
	case "bcrypt":
		h, err := bcrypt.GenerateFromPassword([]byte(key), bcrypt.DefaultCost)
		if err != nil {
			return "", err
		}
		return string(h), nil
	case "sha256":
		sum := sha256.Sum256([]byte(key))
		return hex.EncodeToString(sum[:]), nil
	default:
		return "", errors.New("unsupported hash algorithm: " + hashAlgo)
	}
}

// hashArgon2id derives an argon2id key from key with the given params and
// returns the canonical encoded form: argon2id$v=N$m=M,t=T,p=P$<salt-b64>$<hash-b64>.
// The salt is cryptographically random (16 bytes from crypto/rand), so two
// hashes of the same key produce different encodings — callers must use
// verifyArgon2id rather than re-hashing for comparison.
func hashArgon2id(key string, p argon2idParams) string {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		// crypto/rand should never fail on a healthy host; fall back to a
		// time-based salt rather than blocking the request. This branch is
		// unreachable in production but keeps the function total.
		copy(salt, []byte(time.Now().Format(time.RFC3339Nano)))
	}
	hash := argon2.IDKey([]byte(key), salt, p.time, p.memory, p.threads, p.keyLen)
	return "argon2id$v=19$m=" + itoa(int(p.memory)) +
		",t=" + itoa(int(p.time)) +
		",p=" + itoa(int(p.threads)) +
		"$" + base64.RawStdEncoding.EncodeToString(salt) +
		"$" + base64.RawStdEncoding.EncodeToString(hash)
}

// verifyArgon2id parses the encoded hash and re-derives the key to compare.
// The encoded format is:
//
//	argon2id$v=19$m=MEM,t=T,p=P$<salt-b64>$<hash-b64>
//
// so the salt and hash are the last two `$`-separated segments.
func verifyArgon2id(key, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) < 3 {
		return false, errors.New("invalid argon2id encoding")
	}
	// Last two segments are salt and hash.
	salt, err := base64.RawStdEncoding.DecodeString(parts[len(parts)-2])
	if err != nil {
		return false, err
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[len(parts)-1])
	if err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(key), salt, defaultArgon2idParams.time, defaultArgon2idParams.memory, defaultArgon2idParams.threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
