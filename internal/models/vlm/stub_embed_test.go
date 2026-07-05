package vlm

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/domain"
)

// TestStubEmbedUnsupportedByDefault verifies Embed returns ErrUnsupported
// when EmbedFn is nil.
func TestStubEmbedUnsupportedByDefault(t *testing.T) {
	s := &Stub{}
	_, err := s.Embed(context.Background(), []string{"hello"})
	require.ErrorIs(t, err, domain.ErrUnsupported)
}

// TestStubEmbedRecordsCalls verifies Embed records the call and delegates
// to EmbedFn when set.
func TestStubEmbedRecordsCalls(t *testing.T) {
	s := &Stub{
		EmbedFn: func(_ context.Context, texts []string) ([][]float32, error) {
			out := make([][]float32, len(texts))
			for i := range texts {
				out[i] = []float32{0.1, 0.2}
			}
			return out, nil
		},
	}
	got, err := s.Embed(context.Background(), []string{"a", "b"})
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, []float32{0.1, 0.2}, got[0])

	s.mu.Lock()
	defer s.mu.Unlock()
	require.Len(t, s.Calls, 1)
	assert.Equal(t, "embed", s.Calls[0].Kind)
	assert.Equal(t, []string{"a", "b"}, s.Calls[0].Texts)
}
