// Package auth provides shared authentication primitives used by API key
// storage and the auth middleware. The argon2id parameters and salt
// approach here mirror the Python reference (ARGON2_TIME_COST=3,
// ARGON2_MEMORY_COST=65536, ARGON2_PARALLELISM=2, ARGON2_HASH_LENGTH=32)
// at openviking/server/api_keys/legacy.py and the crypto/rand 16-byte
// salt fix in internal/server/middleware/auth.go.
//
// internal/server/middleware/auth.go retains its own argon2id
// implementation (with time=1/threads=1) for now; future work can
// refactor it to call these helpers so both codepaths share a single
// parameter source. Until then, new code should use these helpers.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Params holds argon2id parameters. The zero value is unusable; construct
// via DefaultParams or explicitly.
type Params struct {
	Time    uint32
	Memory  uint32
	Threads uint8
	KeyLen  uint32
	SaltLen int
}

// DefaultParams matches the Python reference at
// openviking/server/api_keys/legacy.py (time=3, memory=64*1024,
// threads=2, keyLen=32, saltLen=16). The 16-byte crypto/rand salt
// matches the fix in internal/server/middleware/auth.go.
var DefaultParams = Params{
	Time:    3,
	Memory:  64 * 1024,
	Threads: 2,
	KeyLen:  32,
	SaltLen: 16,
}

// Hash derives an argon2id key from key with the given params and returns
// the canonical encoded form:
//
//	argon2id$v=19$m=MEM,t=T,p=P$<salt-b64>$<hash-b64>
//
// The salt is cryptographically random (SaltLen bytes from crypto/rand),
// so two hashes of the same key produce different encodings — callers
// must use Verify rather than re-hashing for comparison.
func Hash(key string, p Params) (string, error) {
	if p.SaltLen <= 0 {
		return "", errors.New("auth: salt length must be positive")
	}
	if p.KeyLen == 0 {
		return "", errors.New("auth: key length must be positive")
	}
	salt := make([]byte, p.SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: read salt: %w", err)
	}
	hash := argon2.IDKey([]byte(key), salt, p.Time, p.Memory, p.Threads, p.KeyLen)
	return encode(p, salt, hash), nil
}

// Verify parses the encoded hash produced by Hash and re-derives the key
// to compare in constant time. Returns false (not error) when the key
// does not match. Returns an error only when the encoding is malformed.
func Verify(key, encoded string) (bool, error) {
	p, salt, want, err := decode(encoded)
	if err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(key), salt, p.Time, p.Memory, p.Threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// encode renders the canonical argon2id encoding.
func encode(p Params, salt, hash []byte) string {
	return "argon2id$v=19$m=" + itoa(int(p.Memory)) +
		",t=" + itoa(int(p.Time)) +
		",p=" + itoa(int(p.Threads)) +
		"$" + base64.RawStdEncoding.EncodeToString(salt) +
		"$" + base64.RawStdEncoding.EncodeToString(hash)
}

// decode parses the canonical argon2id encoding produced by encode.
// The encoded form is `argon2id$v=19$m=MEM,t=T,p=P$<salt-b64>$<hash-b64>`
// so the salt and hash are the last two `$`-separated segments; the
// params segment is the one immediately before them. The salt length is
// implicit in the decoded salt segment.
func decode(encoded string) (Params, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) < 4 {
		return Params{}, nil, nil, errors.New("auth: invalid argon2id encoding")
	}
	// Last two segments are salt and hash; the segment before them is
	// the params block (m=MEM,t=T,p=P).
	p, err := parseParams(parts[len(parts)-3])
	if err != nil {
		return Params{}, nil, nil, err
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[len(parts)-2])
	if err != nil {
		return Params{}, nil, nil, fmt.Errorf("auth: decode salt: %w", err)
	}
	hash, err := base64.RawStdEncoding.DecodeString(parts[len(parts)-1])
	if err != nil {
		return Params{}, nil, nil, fmt.Errorf("auth: decode hash: %w", err)
	}
	return p, salt, hash, nil
}

// parseParams parses "m=MEM,t=T,p=P" into a Params struct. The SaltLen
// field is not encoded; callers set it explicitly when re-hashing.
func parseParams(s string) (Params, error) {
	var p Params
	for _, kv := range strings.Split(s, ",") {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			return Params{}, fmt.Errorf("auth: invalid param %q", kv)
		}
		k, v := kv[:eq], kv[eq+1:]
		n, err := atoi(v)
		if err != nil {
			return Params{}, fmt.Errorf("auth: invalid param %q: %w", kv, err)
		}
		switch k {
		case "m":
			p.Memory = uint32(n)
		case "t":
			p.Time = uint32(n)
		case "p":
			p.Threads = uint8(n)
		}
	}
	return p, nil
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

func atoi(s string) (int, error) {
	if s == "" {
		return 0, errors.New("empty number")
	}
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, fmt.Errorf("invalid digit %q", rune(s[i]))
		}
		n = n*10 + int(s[i]-'0')
	}
	return n, nil
}
