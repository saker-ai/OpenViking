package doctor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/config"
)

// writeConfig writes a YAML ov.conf at dir/ov.conf and returns its path.
func writeConfig(t *testing.T, dir, yaml string) string {
	t.Helper()
	p := filepath.Join(dir, "ov.conf")
	require.NoError(t, os.WriteFile(p, []byte(yaml), 0o600))
	return p
}

// localOnlyConfig returns a YAML config that uses only local/in-memory
// backends so tests do not need network access. Paths point at dir so disk
// probes succeed.
func localOnlyConfig(dir string) string {
	upload := filepath.Join(dir, "uploads")
	vdb := filepath.Join(dir, "vectordb")
	ragfsRoot := filepath.Join(dir, "ragfs")
	return "server:\n" +
		"  upload_dir: " + upload + "\n" +
		"vectordb:\n" +
		"  backend: local\n" +
		"  local:\n" +
		"    path: " + vdb + "\n" +
		"ragfs:\n" +
		"  mounts:\n" +
		"    - name: default\n" +
		"      backend: local\n" +
		"      path: " + ragfsRoot + "\n" +
		"queue:\n" +
		"  backend: memory\n"
}

func TestProbeConfig_OK(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, localOnlyConfig(dir))
	results := probeConfig(context.Background(), path, configSections)

	// Expect parse ok and every section either ok or skip (memory queue
	// skips; embedder/rerank/vlm skip because no provider).
	byName := map[string]ProbeResult{}
	for _, r := range results {
		byName[r.Name] = r
	}
	require.Contains(t, byName, "config.parse")
	assert.Equal(t, StatusOK, byName["config.parse"].Status)
	assert.Equal(t, StatusOK, byName["config.server"].Status)
	assert.Equal(t, StatusOK, byName["config.vectordb"].Status)
	assert.Equal(t, StatusOK, byName["config.ragfs"].Status)
	assert.Equal(t, StatusSkip, byName["config.embedder"].Status)
	assert.Equal(t, StatusSkip, byName["config.rerank"].Status)
	assert.Equal(t, StatusSkip, byName["config.vlm"].Status)
	assert.Equal(t, StatusSkip, byName["config.queuefs"].Status)
}

func TestProbeConfig_NoConfigFile(t *testing.T) {
	t.Setenv("OV_CONFIG_PATH", "")
	// Use a search path with no candidates: point OV_CONFIG_PATH at a
	// nonexistent file and override with an empty path. The probe should
	// report fail and skip every section.
	results := probeConfig(context.Background(), "/nonexistent/ov.conf", configSections)
	byName := map[string]ProbeResult{}
	for _, r := range results {
		byName[r.Name] = r
	}
	assert.Equal(t, StatusFail, byName["config.parse"].Status)
	for _, s := range configSections {
		assert.Equal(t, StatusSkip, byName["config."+s].Status, "section %s", s)
	}
}

func TestProbeConfig_InvalidSection(t *testing.T) {
	dir := t.TempDir()
	// vectordb backend is invalid -> validation fails.
	yaml := "vectordb:\n  backend: not-a-backend\n"
	path := writeConfig(t, dir, yaml)
	results := probeConfig(context.Background(), path, configSections)
	byName := map[string]ProbeResult{}
	for _, r := range results {
		byName[r.Name] = r
	}
	assert.Equal(t, StatusFail, byName["config.vectordb"].Status)
}

func TestProbeConnectivity_LocalBackendsSkipOrOK(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, localOnlyConfig(dir))
	cfg, err := config.Load(path)
	require.NoError(t, err)

	// Pre-create the ragfs root so the write probe can stat+write.
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "ragfs"), 0o755))

	results := probeConnectivity(context.Background(), cfg, 2*time.Second)
	byName := map[string]ProbeResult{}
	for _, r := range results {
		byName[r.Name] = r
	}
	// vectordb local -> skip (no remote).
	assert.Equal(t, StatusSkip, byName["connectivity.vectordb"].Status)
	// embedder/rerank/vlm unconfigured -> skip.
	assert.Equal(t, StatusSkip, byName["connectivity.embedder"].Status)
	assert.Equal(t, StatusSkip, byName["connectivity.rerank"].Status)
	assert.Equal(t, StatusSkip, byName["connectivity.vlm"].Status)
	// queuefs memory -> skip.
	assert.Equal(t, StatusSkip, byName["connectivity.queuefs"].Status)
	// ragfs local mount exists and writable -> ok.
	assert.Equal(t, StatusOK, byName["connectivity.ragfs"].Status)
}

func TestProbeConnectivity_RAGFSMissingFails(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, localOnlyConfig(dir))
	cfg, err := config.Load(path)
	require.NoError(t, err)
	// Do not create the ragfs root: stat fails -> fail.
	results := probeConnectivity(context.Background(), cfg, 2*time.Second)
	byName := map[string]ProbeResult{}
	for _, r := range results {
		byName[r.Name] = r
	}
	assert.Equal(t, StatusFail, byName["connectivity.ragfs"].Status)
}

func TestProbeDisk_TempDirPathsOK(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, localOnlyConfig(dir))
	cfg, err := config.Load(path)
	require.NoError(t, err)
	// Create the configured paths so disk reports ok.
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "uploads"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "vectordb"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "ragfs"), 0o755))

	results := probeDisk(context.Background(), cfg)
	// Every path must exist and be writable.
	for _, r := range results {
		assert.Equal(t, StatusOK, r.Status, "path %s detail=%s", r.Name, r.Detail)
		assert.NotZero(t, r.Extra["free_bytes"])
		assert.NotZero(t, r.Extra["total_bytes"])
	}
}

func TestProbeDisk_MissingPathFails(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, localOnlyConfig(dir))
	cfg, err := config.Load(path)
	require.NoError(t, err)
	// Do not create the configured paths; expect fails.
	results := probeDisk(context.Background(), cfg)
	byName := map[string]ProbeResult{}
	for _, r := range results {
		byName[r.Name] = r
	}
	// /tmp must exist (ok); uploads/vectordb/ragfs missing (fail).
	gotOK, gotFail := false, false
	for _, r := range results {
		switch r.Status {
		case StatusOK:
			gotOK = true
		case StatusFail:
			gotFail = true
		}
	}
	assert.True(t, gotOK, "expected at least one ok path")
	assert.True(t, gotFail, "expected at least one fail path")
	// The disk probe for ragfs root must be a fail.
	var ragfsResult *ProbeResult
	for i := range results {
		if strings.Contains(results[i].Name, "ragfs") {
			ragfsResult = &results[i]
			break
		}
	}
	require.NotNil(t, ragfsResult, "expected a ragfs disk result")
	assert.Equal(t, StatusFail, ragfsResult.Status)
}

func TestReportJSON_RoundTrip(t *testing.T) {
	r := &Report{
		Command:   "all",
		StartedAt: time.Date(2026, 7, 5, 1, 2, 3, 0, time.UTC),
		Results: []ProbeResult{
			{Name: "x", Status: StatusOK, LatencyMS: 5, Detail: "ok"},
			{Name: "y", Status: StatusSkip, Detail: "no provider"},
		},
	}
	var buf strings.Builder
	require.NoError(t, r.PrintJSON(&buf))
	var dec Report
	require.NoError(t, json.Unmarshal([]byte(buf.String()), &dec))
	assert.Equal(t, "all", dec.Command)
	require.Len(t, dec.Results, 2)
	assert.Equal(t, StatusOK, dec.Results[0].Status)
	assert.Equal(t, StatusSkip, dec.Results[1].Status)
	assert.True(t, r.Failed() == false)
}

func TestReport_FailedDetectsFail(t *testing.T) {
	r := &Report{
		Command: "all",
		Results: []ProbeResult{
			{Name: "ok", Status: StatusOK},
			{Name: "bad", Status: StatusFail},
			{Name: "skip", Status: StatusSkip},
		},
	}
	assert.True(t, r.Failed())
}

func TestNewRoot_DefaultRunsAll(t *testing.T) {
	// When no subcommand is given the root RunE should call runAll. We
	// verify by capturing stdout JSON for a config that passes every
	// probe.
	dir := t.TempDir()
	path := writeConfig(t, dir, localOnlyConfig(dir))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "uploads"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "vectordb"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "ragfs"), 0o755))

	root := NewRoot()
	root.SetArgs([]string{"--config", path, "--json", "--timeout", "2s"})
	var out strings.Builder
	root.SetOut(&out)
	err := root.Execute()
	require.NoError(t, err, "expected runAll to succeed for local-only config")

	var rep Report
	require.NoError(t, json.Unmarshal([]byte(out.String()), &rep), "output: %s", out.String())
	assert.Equal(t, "all", rep.Command)
	assert.NotEmpty(t, rep.Results)
	// No probe should fail for this fully-provisioned local config.
	for _, r := range rep.Results {
		assert.NotEqual(t, StatusFail, r.Status, "unexpected fail: %+v", r)
	}
}

func TestNewRoot_AllSubcommandExitsNonZeroOnFailure(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, localOnlyConfig(dir))
	// Don't create the configured paths so disk probes fail.
	root := NewRoot()
	root.SetArgs([]string{"all", "--config", path, "--json", "--timeout", "2s"})
	var out strings.Builder
	root.SetOut(&out)
	err := root.Execute()
	require.Error(t, err, "expected non-zero exit when probes fail")
	assert.ErrorIs(t, err, errProbeFailed)
}
