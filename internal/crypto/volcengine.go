package crypto

import (
	"errors"

	"github.com/saker-ai/ctxhub/internal/domain"
)

// VolcengineProvider wraps DEKs with Volcengine KMS. P1 scope: stubbed.
//
// The full implementation requires the Volcengine V4 HMAC-SHA256 signer
// (see internal/vectordb/vikingdb.go sign()) and the KMS Encrypt/Decrypt
// REST endpoints. The signer's credential-scope service component for KMS
// differs from VikingDB's ("air"), so the vikingdb signer cannot be
// imported directly — it must be parameterized or copied. P2 will wire
// the real implementation; until then Wrap/Unwrap return ErrInternal so
// callers fail loudly rather than silently shipping unencrypted data.
type VolcengineProvider struct {
	cfg VolcengineConfig
}

// NewVolcengine validates cfg and returns the stubbed provider. All four
// fields are required; missing values are a configuration error.
func NewVolcengine(cfg VolcengineConfig) (*VolcengineProvider, error) {
	if cfg.Region == "" || cfg.AccessKey == "" || cfg.SecretKey == "" || cfg.KmsKeyID == "" {
		return nil, domain.Wrap(domain.CodeValidationFailed, 422,
			errors.New("volcengine crypto: Region, AccessKey, SecretKey, KmsKeyID are required"))
	}
	return &VolcengineProvider{cfg: cfg}, nil
}

// Wrap returns ErrInternal. P2 will POST the DEK to the KMS Encrypt
// endpoint and return the base64 ciphertext blob.
func (p *VolcengineProvider) Wrap(dek []byte) ([]byte, error) {
	_ = dek
	return nil, domain.Wrap(domain.CodeInternalError, 500,
		errors.New("volcengine kms not yet wired"))
}

// Unwrap returns ErrInternal. See Wrap.
func (p *VolcengineProvider) Unwrap(wrapped []byte) ([]byte, error) {
	_ = wrapped
	return nil, domain.Wrap(domain.CodeInternalError, 500,
		errors.New("volcengine kms not yet wired"))
}
