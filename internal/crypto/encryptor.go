package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"

	"github.com/saker-ai/ctxhub/internal/domain"
)

type encryptor struct {
	provider     Provider
	providerName string
}

// New picks a Provider based on cfg.Provider and returns the Encryptor.
// Empty cfg.Provider defaults to "local" so callers can use a zero-value
// Config with only Local set.
func New(cfg Config) (Encryptor, error) {
	var (
		provider     Provider
		providerName string
	)
	switch cfg.Provider {
	case "", "local":
		p, err := NewLocal(cfg.Local)
		if err != nil {
			return nil, err
		}
		provider, providerName = p, "local"
	case "vault":
		p, err := NewVault(cfg.Vault)
		if err != nil {
			return nil, err
		}
		provider, providerName = p, "vault"
	case "volcengine":
		p, err := NewVolcengine(cfg.Volcengine)
		if err != nil {
			return nil, err
		}
		provider, providerName = p, "volcengine"
	default:
		return nil, domain.Wrap(domain.CodeValidationFailed, 422,
			errors.New("crypto: unknown provider "+cfg.Provider))
	}
	return &encryptor{provider: provider, providerName: providerName}, nil
}

// Encrypt generates a random DEK (32 bytes), AES-256-GCM encrypts
// plaintext with AAD, wraps the DEK with the provider, and returns the
// Ciphertext envelope. The DEK is wiped from memory by the GC after the
// function returns (Go has no zeroing guarantee; P2 may use a pinned
// buffer if needed).
func (e *encryptor) Encrypt(plaintext []byte, aad []byte) (*Ciphertext, error) {
	dek := make([]byte, 32)
	if _, err := rand.Read(dek); err != nil {
		return nil, domain.Wrap(domain.CodeInternalError, 500, err)
	}
	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, domain.Wrap(domain.CodeInternalError, 500, err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, domain.Wrap(domain.CodeInternalError, 500, err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, domain.Wrap(domain.CodeInternalError, 500, err)
	}
	ct := gcm.Seal(nil, nonce, plaintext, aad)
	wrapped, err := e.provider.Wrap(dek)
	if err != nil {
		return nil, err
	}
	return &Ciphertext{
		DEK:        encode(wrapped),
		Nonce:      encode(nonce),
		Ciphertext: encode(ct),
		KDF:        e.kdfName(),
		Provider:   e.providerName,
	}, nil
}

// Decrypt unwraps the DEK with the provider and AES-256-GCM decrypts the
// ciphertext. AAD must match the value passed to Encrypt; mismatches are
// detected by AES-GCM and reported as ErrInternal.
func (e *encryptor) Decrypt(c *Ciphertext, aad []byte) ([]byte, error) {
	if c == nil {
		return nil, domain.Wrap(domain.CodeValidationFailed, 422,
			errors.New("crypto: ciphertext is nil"))
	}
	wrapped, err := decode(c.DEK)
	if err != nil {
		return nil, domain.Wrap(domain.CodeValidationFailed, 422,
			fmt.Errorf("crypto: decode DEK: %w", err))
	}
	nonce, err := decode(c.Nonce)
	if err != nil {
		return nil, domain.Wrap(domain.CodeValidationFailed, 422,
			fmt.Errorf("crypto: decode nonce: %w", err))
	}
	ct, err := decode(c.Ciphertext)
	if err != nil {
		return nil, domain.Wrap(domain.CodeValidationFailed, 422,
			fmt.Errorf("crypto: decode ciphertext: %w", err))
	}
	dek, err := e.provider.Unwrap(wrapped)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, domain.Wrap(domain.CodeInternalError, 500, err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, domain.Wrap(domain.CodeInternalError, 500, err)
	}
	pt, err := gcm.Open(nil, nonce, ct, aad)
	if err != nil {
		return nil, domain.Wrap(domain.CodeInternalError, 500,
			fmt.Errorf("crypto: decrypt failed: %w", err))
	}
	return pt, nil
}

// kdfName reports the master-key KDF name. Only the local provider has a
// meaningful value; other providers report "external".
func (e *encryptor) kdfName() string {
	if lp, ok := e.provider.(*LocalProvider); ok {
		return lp.KDF()
	}
	return KDFExternal
}
