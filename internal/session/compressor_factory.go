package session

import (
	"context"
	"fmt"

	"github.com/saker-ai/ctxhub/internal/domain"
)

// NewCompressor returns the compressor selected by cfg.Version.
//
//	"v2"   -> CompressorV2 (naive truncation)
//	"v3"   -> CompressorV3 (LLM summary-based, falls back to v2)
//	"auto" -> AutoCompressor (v2 below AutoV3Turns, v3 above)
//
// Any other version string returns an error.
func NewCompressor(cfg CompressorConfig) (Compressor, error) {
	cfg = cfg.withDefaults()
	switch cfg.Version {
	case "v2":
		return NewCompressorV2(cfg), nil
	case "v3":
		return NewCompressorV3(cfg), nil
	case "auto":
		return &AutoCompressor{
			v2:        NewCompressorV2(cfg),
			v3:        NewCompressorV3(cfg),
			threshold: cfg.AutoV3Turns,
		}, nil
	default:
		return nil, fmt.Errorf("session: unknown compressor version %q", cfg.Version)
	}
}

// AutoCompressor picks v2 or v3 based on the session's turn count.
type AutoCompressor struct {
	v2        Compressor
	v3        Compressor
	threshold int
}

// Compress implements Compressor.
func (a *AutoCompressor) Compress(ctx context.Context, sess *domain.Session) (*domain.Session, error) {
	if sess == nil {
		return nil, fmt.Errorf("auto_compressor: nil session")
	}
	if len(sess.Turns) >= a.threshold {
		return a.v3.Compress(ctx, sess)
	}
	return a.v2.Compress(ctx, sess)
}
