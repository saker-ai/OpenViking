package crypto

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/saker-ai/ctxhub/internal/domain"
)

// TestLocalRoundTripMasterKeyFile exercises the local provider with a
// 32-byte master key loaded from a binary file.
func TestLocalRoundTripMasterKeyFile(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "master.key")
	master := make([]byte, 32)
	_, _ = rand.Read(master)
	require.NoError(t, os.WriteFile(keyPath, master, 0o600))

	enc, err := New(Config{
		Provider: "local",
		Local:    LocalConfig{MasterKeyPath: keyPath},
	})
	require.NoError(t, err)

	plaintext := []byte("the quick brown fox jumps over the lazy dog")
	aad := []byte("account=acme;scope=files")
	c, err := enc.Encrypt(plaintext, aad)
	require.NoError(t, err)
	assert.Equal(t, "local", c.Provider)
	assert.Equal(t, "file", c.KDF)
	assert.NotEmpty(t, c.DEK)
	assert.NotEmpty(t, c.Nonce)
	assert.NotEmpty(t, c.Ciphertext)

	got, err := enc.Decrypt(c, aad)
	require.NoError(t, err)
	assert.Equal(t, plaintext, got)
}

// TestLocalRoundTripHexMasterKey exercises the 64-char hex form.
func TestLocalRoundTripHexMasterKey(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "master.hex")
	master := make([]byte, 32)
	_, _ = rand.Read(master)
	require.NoError(t, os.WriteFile(keyPath, []byte(hex.EncodeToString(master)), 0o600))

	enc, err := New(Config{
		Provider: "local",
		Local:    LocalConfig{MasterKeyPath: keyPath},
	})
	require.NoError(t, err)

	pt := []byte("hello secret world")
	c, err := enc.Encrypt(pt, nil)
	require.NoError(t, err)
	got, err := enc.Decrypt(c, nil)
	require.NoError(t, err)
	assert.Equal(t, pt, got)
}

// TestLocalRoundTripPassphrase exercises argon2id derivation. The same
// passphrase + default salt must produce the same master key across
// processes, so a fresh encryptor can decrypt what another produced.
func TestLocalRoundTripPassphrase(t *testing.T) {
	cfg := Config{
		Provider: "local",
		Local:    LocalConfig{Passphrase: "correct horse battery staple"},
	}
	enc, err := New(cfg)
	require.NoError(t, err)
	dec, err := New(cfg) // separate instance simulates a new process
	require.NoError(t, err)

	pt := []byte("envelope encryption payload")
	aad := []byte("v=1")
	c, err := enc.Encrypt(pt, aad)
	require.NoError(t, err)
	assert.Equal(t, "argon2id", c.KDF)
	got, err := dec.Decrypt(c, aad)
	require.NoError(t, err)
	assert.Equal(t, pt, got)
}

// TestLocalAADMismatchFails proves AAD is cryptographically bound: a
// different AAD on decrypt must fail.
func TestLocalAADMismatchFails(t *testing.T) {
	enc, err := New(Config{
		Provider: "local",
		Local:    LocalConfig{Passphrase: "pw"},
	})
	require.NoError(t, err)
	c, err := enc.Encrypt([]byte("payload"), []byte("aad-1"))
	require.NoError(t, err)
	_, err = enc.Decrypt(c, []byte("aad-2"))
	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrInternal),
		"want ErrInternal, got %v", err)
}

// TestLocalWrongMasterKeyFails proves a different master key cannot
// decrypt the ciphertext.
func TestLocalWrongMasterKeyFails(t *testing.T) {
	enc1, err := New(Config{
		Provider: "local",
		Local:    LocalConfig{Passphrase: "pass-1"},
	})
	require.NoError(t, err)
	enc2, err := New(Config{
		Provider: "local",
		Local:    LocalConfig{Passphrase: "pass-2"},
	})
	require.NoError(t, err)

	c, err := enc1.Encrypt([]byte("payload"), nil)
	require.NoError(t, err)
	_, err = enc2.Decrypt(c, nil)
	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrInternal),
		"want ErrInternal, got %v", err)
}

// TestLocalMissingConfigFails proves that an empty LocalConfig is a
// validation error, not a silent zero-key encryptor.
func TestLocalMissingConfigFails(t *testing.T) {
	_, err := New(Config{Provider: "local"})
	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrValidation),
		"want ErrValidation, got %v", err)
}

// TestVolcengineStubReturnsError proves the stub fails loudly. A clear
// error is preferable to a half-working implementation that might
// silently ship unencrypted data.
func TestVolcengineStubReturnsError(t *testing.T) {
	enc, err := New(Config{
		Provider: "volcengine",
		Volcengine: VolcengineConfig{
			Region:    "cn-north-1",
			AccessKey: "ak",
			SecretKey: "sk",
			KmsKeyID:  "kp-xxx",
		},
	})
	require.NoError(t, err)

	_, err = enc.Encrypt([]byte("payload"), nil)
	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrInternal),
		"want ErrInternal, got %v", err)
	assert.Contains(t, err.Error(), "volcengine kms not yet wired")
}

// TestVolcengineMissingConfigFails proves the constructor rejects partial
// configuration before any encrypt attempt.
func TestVolcengineMissingConfigFails(t *testing.T) {
	_, err := New(Config{
		Provider: "volcengine",
		Volcengine: VolcengineConfig{Region: "cn-north-1"},
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrValidation),
		"want ErrValidation, got %v", err)
}

// TestUnknownProviderFails proves an unknown provider name is a
// validation error.
func TestUnknownProviderFails(t *testing.T) {
	_, err := New(Config{Provider: "kms-magic"})
	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrValidation),
		"want ErrValidation, got %v", err)
}
