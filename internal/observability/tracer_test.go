package observability

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/config"
)

func TestInitTracerMemoryExporter(t *testing.T) {
	cfg := config.OTELConfig{
		Exporter:    "memory",
		ServiceName: "test",
		SampleRate:  1.0,
	}
	shutdown, err := InitTracer(context.Background(), cfg)
	require.NoError(t, err)
	require.NotNil(t, shutdown)
	assert.NoError(t, shutdown(context.Background()))
}

func TestInitTracerUnsupportedExporter(t *testing.T) {
	cfg := config.OTELConfig{Exporter: "bogus"}
	_, err := InitTracer(context.Background(), cfg)
	require.Error(t, err)
}
