// Package crypto provides envelope encryption for sensitive data at rest.
//
// The package implements the envelope encryption pattern: a random data
// encryption key (DEK) is generated per operation, used to AES-256-GCM
// encrypt the plaintext, and then wrapped by a key encryption key (KEK)
// held by a Provider. The wrapped DEK is stored alongside the ciphertext
// so that decryption only requires the provider to unwrap the DEK.
//
// Three provider backends are supported:
//   - local: master key loaded from a file or derived from a passphrase
//     via argon2id.
//   - vault: HashiCorp Vault response-wrapping endpoints (sys/wrapping).
//   - volcengine: Volcengine KMS (P1 stub — returns ErrInternal).
//
// The package is intentionally free of SDK dependencies beyond the standard
// library and golang.org/x/crypto/argon2 (already used by middleware/auth).
package crypto

import "encoding/base64"

const (
	// KDFArgon2id marks a master key derived from a passphrase via argon2id.
	KDFArgon2id = "argon2id"
	// KDFFile marks a master key loaded directly from a key file.
	KDFFile = "file"
	// KDFExternal marks a master key held by an external provider (vault,
	// volcengine). The Ciphertext.KDF field uses this when the KEK is not
	// locally derived.
	KDFExternal = "external"
)

// Encryptor is the envelope-encryption entry point. AAD (additional
// authenticated data) is bound to the ciphertext via AES-GCM and must be
// identical on decrypt; mismatches cause Decrypt to fail.
type Encryptor interface {
	Encrypt(plaintext []byte, aad []byte) (*Ciphertext, error)
	Decrypt(c *Ciphertext, aad []byte) ([]byte, error)
}

// Ciphertext is the serialized envelope. All byte fields are base64
// (RawStdEncoding) so the struct is JSON-serializable.
type Ciphertext struct {
	// DEK is the wrapped data encryption key.
	DEK string `json:"dek"`
	// Nonce is the AES-GCM nonce used for the data encryption.
	Nonce string `json:"nonce"`
	// Ciphertext is the encrypted data.
	Ciphertext string `json:"ciphertext"`
	// KDF names the master-key derivation (e.g. "argon2id", "file",
	// "external").
	KDF string `json:"kdf"`
	// Provider names the KEK backend ("local"|"vault"|"volcengine").
	Provider string `json:"provider"`
}

// Provider wraps and unwraps the DEK with the configured KEK. Wrap
// returns an opaque blob that Unwrap can reverse without any out-of-band
// state beyond the provider's own configuration.
type Provider interface {
	Wrap(dek []byte) ([]byte, error)
	Unwrap(wrapped []byte) ([]byte, error)
}

// Config selects a provider and its parameters. Provider must be one of
// "local", "vault", "volcengine" (empty defaults to "local").
type Config struct {
	Provider   string
	Local      LocalConfig
	Vault      VaultConfig
	Volcengine VolcengineConfig
}

// LocalConfig configures the local provider. Either MasterKeyPath or
// Passphrase must be set; MasterKeyPath wins when both are populated.
type LocalConfig struct {
	// MasterKeyPath is the path to a file containing the 32-byte master
	// key (raw bytes or 64-char hex). When empty, the master key is
	// derived from Passphrase via argon2id.
	MasterKeyPath string
	// Passphrase is used to derive the master key when MasterKeyPath is
	// empty. Required when MasterKeyPath is empty.
	Passphrase string
	// Salt is the argon2id salt. When empty, a fixed app salt is used.
	Salt []byte
}

// VaultConfig configures the Vault provider. The provider uses Vault's
// sys/wrapping/wrap and sys/wrapping/unwrap endpoints via stdlib net/http
// (no Vault SDK dependency).
type VaultConfig struct {
	Address   string // e.g. "http://127.0.0.1:8200"
	TokenPath string // path to file containing the Vault token
	KeyPath   string // optional transit key path (reserved for future use)
}

// VolcengineConfig configures the Volcengine KMS provider.
type VolcengineConfig struct {
	Region    string
	AccessKey string
	SecretKey string
	KmsKeyID  string
}

// encode returns the RawStdEncoding base64 of b. RawStdEncoding is used
// (no padding) to keep the JSON form compact and URL-safe.
func encode(b []byte) string {
	return base64.RawStdEncoding.EncodeToString(b)
}

// decode parses a RawStdEncoding base64 string produced by encode.
func decode(s string) ([]byte, error) {
	return base64.RawStdEncoding.DecodeString(s)
}
