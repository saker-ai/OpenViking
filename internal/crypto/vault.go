package crypto

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/saker-ai/ctxhub/internal/domain"
)

// VaultProvider wraps DEKs using Vault's sys/wrapping endpoints. The
// wrapped DEK returned by Wrap is the response-wrapping token (a string),
// which Unwrap consumes to recover the DEK. This avoids managing transit
// key versions and works against any Vault deployment that exposes the
// sys/wrapping endpoints.
//
// P1 scope: the token is stored as a UTF-8 string inside the wrapped
// []byte. The blob format is opaque to the Encryptor.
type VaultProvider struct {
	address string
	token   string
	client  *http.Client
}

// NewVault builds a VaultProvider. The token is read from cfg.TokenPath
// at construction time; missing or empty token files are an error.
func NewVault(cfg VaultConfig) (*VaultProvider, error) {
	if cfg.Address == "" {
		return nil, domain.Wrap(domain.CodeValidationFailed, 422,
			errors.New("vault crypto: Address is required"))
	}
	if cfg.TokenPath == "" {
		return nil, domain.Wrap(domain.CodeValidationFailed, 422,
			errors.New("vault crypto: TokenPath is required"))
	}
	tokenBytes, err := os.ReadFile(cfg.TokenPath)
	if err != nil {
		return nil, domain.Wrap(domain.CodeInternalError, 500,
			fmt.Errorf("vault crypto: read token: %w", err))
	}
	token := strings.TrimSpace(string(tokenBytes))
	if token == "" {
		return nil, domain.Wrap(domain.CodeValidationFailed, 422,
			fmt.Errorf("vault crypto: token file %s is empty", cfg.TokenPath))
	}
	return &VaultProvider{
		address: strings.TrimRight(cfg.Address, "/"),
		token:   token,
		client:  &http.Client{Timeout: 10 * time.Second},
	}, nil
}

// Wrap POSTs base64(dek) to /v1/sys/wrapping/wrap and returns the
// response-wrapping token as bytes.
func (p *VaultProvider) Wrap(dek []byte) ([]byte, error) {
	body := fmt.Sprintf(`{"input":%q}`, base64.StdEncoding.EncodeToString(dek))
	resp, err := p.post("/v1/sys/wrapping/wrap", body)
	if err != nil {
		return nil, err
	}
	tok, ok := resp["token"].(string)
	if !ok || tok == "" {
		return nil, domain.Wrap(domain.CodeInternalError, 500,
			fmt.Errorf("vault crypto: wrap response missing token: %v", resp))
	}
	return []byte(tok), nil
}

// Unwrap POSTs the token to /v1/sys/wrapping/unwrap and base64-decodes
// the returned input field back to the DEK.
func (p *VaultProvider) Unwrap(wrapped []byte) ([]byte, error) {
	tok := strings.TrimSpace(string(wrapped))
	if tok == "" {
		return nil, domain.Wrap(domain.CodeValidationFailed, 422,
			errors.New("vault crypto: empty wrapped token"))
	}
	body := fmt.Sprintf(`{"token":%q}`, tok)
	resp, err := p.post("/v1/sys/wrapping/unwrap", body)
	if err != nil {
		return nil, err
	}
	input, ok := resp["input"].(string)
	if !ok || input == "" {
		return nil, domain.Wrap(domain.CodeInternalError, 500,
			fmt.Errorf("vault crypto: unwrap response missing input: %v", resp))
	}
	dek, err := base64.StdEncoding.DecodeString(input)
	if err != nil {
		return nil, domain.Wrap(domain.CodeInternalError, 500,
			fmt.Errorf("vault crypto: decode unwrapped DEK: %w", err))
	}
	return dek, nil
}

// post sends a JSON body to path with the X-Vault-Token header and
// returns the response's top-level "data" object. Non-2xx responses are
// turned into ErrInternal-wrapped errors.
func (p *VaultProvider) post(path, body string) (map[string]any, error) {
	req, err := http.NewRequest(http.MethodPost, p.address+path, strings.NewReader(body))
	if err != nil {
		return nil, domain.Wrap(domain.CodeInternalError, 500, err)
	}
	req.Header.Set("X-Vault-Token", p.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, domain.Wrap(domain.CodeInternalError, 500,
			fmt.Errorf("vault crypto: http: %w", err))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, domain.Wrap(domain.CodeInternalError, 500,
			fmt.Errorf("vault crypto: read body: %w", err))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, domain.Wrap(domain.CodeInternalError, 500,
			fmt.Errorf("vault crypto: status=%d body=%s",
				resp.StatusCode, strings.TrimSpace(string(raw))))
	}
	data, perr := parseVaultResponse(raw)
	if perr != nil {
		return nil, domain.Wrap(domain.CodeInternalError, 500,
			fmt.Errorf("vault crypto: status=%d body=%s: %w",
				resp.StatusCode, strings.TrimSpace(string(raw)), perr))
	}
	return data, nil
}

// parseVaultResponse extracts the top-level "data" object from a Vault
// JSON response. Vault returns errors as {"errors": [...]} — those are
// surfaced as a generic error so callers can include the status code.
func parseVaultResponse(raw []byte) (map[string]any, error) {
	var top map[string]any
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, fmt.Errorf("vault crypto: parse json: %w", err)
	}
	if errs, ok := top["errors"].([]any); ok && len(errs) > 0 {
		parts := make([]string, 0, len(errs))
		for _, e := range errs {
			if s, ok := e.(string); ok {
				parts = append(parts, s)
			}
		}
		return nil, fmt.Errorf("vault crypto: vault errors: %s", strings.Join(parts, "; "))
	}
	data, _ := top["data"].(map[string]any)
	if data == nil {
		return nil, errors.New("vault crypto: response has no data object")
	}
	return data, nil
}
