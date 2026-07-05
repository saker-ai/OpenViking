package vectordb

import (
	"errors"
	"strings"
	"unicode"

	"github.com/saker-ai/ctxhub/internal/domain"
)

// CollectionName builds the tenant-scoped collection name used by every
// backend:
//
//	{collection_prefix}{account}__{kind}
//
// e.g. prefix="ov_", account="acme", kind="file" -> "ov_acme__file".
//
// account and kind must be non-empty and must not contain "__" or any rune
// outside [A-Za-z0-9._-]; this keeps the name a valid identifier across
// qdrant collection names, file paths (local backend), and SQL identifiers
// (opengauss backend, future). An empty prefix is allowed.
func CollectionName(prefix, account, kind string) (string, error) {
	if err := validateCollectionComponent("account", account); err != nil {
		return "", err
	}
	if err := validateCollectionComponent("kind", kind); err != nil {
		return "", err
	}
	// Disallow account/kind to contain the "__" separator to keep parsing
	// unambiguous on SplitCollectionName.
	if strings.Contains(account, "__") || strings.Contains(kind, "__") {
		return "", domain.Wrap(domain.CodeValidationFailed, 422,
			errors.New("collection name components must not contain '__'"))
	}
	return prefix + account + "__" + kind, nil
}

// SplitCollectionName is the inverse of CollectionName: it returns the
// account and kind encoded into name (without the prefix). It is best-effort
// and returns an error when name does not match the expected shape.
func SplitCollectionName(name, prefix string) (account, kind string, err error) {
	if !strings.HasPrefix(name, prefix) {
		return "", "", domain.Wrap(domain.CodeValidationFailed, 422,
			errors.New("collection name does not start with configured prefix"))
	}
	body := strings.TrimPrefix(name, prefix)
	idx := strings.Index(body, "__")
	if idx < 0 {
		return "", "", domain.Wrap(domain.CodeValidationFailed, 422,
			errors.New("collection name missing '__' separator"))
	}
	return body[:idx], body[idx+2:], nil
}

// validateCollectionComponent enforces the [A-Za-z0-9._-]+ charset and
// non-empty. We keep this strict so that the resulting collection name is
// safe as a file name and as a qdrant collection identifier.
func validateCollectionComponent(label, value string) error {
	if value == "" {
		return domain.Wrap(domain.CodeValidationFailed, 422,
			errors.New(label+" must not be empty"))
	}
	for _, r := range value {
		switch {
		case r >= 'A' && r <= 'Z':
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == '-':
		default:
			if !unicode.IsPrint(r) {
				return domain.Wrap(domain.CodeValidationFailed, 422,
					errors.New(label+" contains non-printable rune"))
			}
			return domain.Wrap(domain.CodeValidationFailed, 422,
				errors.New(label+" contains illegal rune '"+string(r)+"'"))
		}
	}
	return nil
}
