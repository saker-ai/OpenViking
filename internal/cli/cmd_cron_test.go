package cli

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/bot/cron"
)

func TestCronListCmd_Empty(t *testing.T) {
	rt, out, _ := newTestRuntimeNoServer(t)
	rt.CronStore = cron.NewMemoryStore()

	cmd := silence(CronCmd(rt))
	require.NoError(t, Execute(cmd, []string{"list"}))
	assert.Contains(t, out.String(), "No scheduled jobs.")
}

func TestCronListCmd_WithJobs(t *testing.T) {
	rt, out, _ := newTestRuntimeNoServer(t)
	store := cron.NewMemoryStore()
	rt.CronStore = store

	ctx := context.Background()
	id1, err := store.Add(ctx, "0 9 * * *", "reminder", []byte(`{"msg":"hi"}`))
	require.NoError(t, err)
	id2, err := store.Add(ctx, "*/5 * * * *", "tick", nil)
	require.NoError(t, err)
	require.NoError(t, store.SetEnabled(ctx, id2, false))

	cmd := silence(CronCmd(rt))
	require.NoError(t, Execute(cmd, []string{"list"}))
	output := out.String()
	assert.Contains(t, output, id1)
	assert.Contains(t, output, id2)
	assert.Contains(t, output, "0 9 * * *")
	assert.Contains(t, output, "*/5 * * * *")
	assert.Contains(t, output, "ENABLED")
}

func TestCronListCmd_JSON(t *testing.T) {
	rt, out, _ := newTestRuntimeNoServer(t)
	store := cron.NewMemoryStore()
	rt.CronStore = store

	_, err := store.Add(context.Background(), "0 9 * * *", "reminder", nil)
	require.NoError(t, err)

	cmd := silence(CronCmd(rt))
	require.NoError(t, Execute(cmd, []string{"list", "--json"}))
	assert.Contains(t, out.String(), `"schedule": "0 9 * * *"`)
}

func TestCronAddCmd(t *testing.T) {
	rt, out, _ := newTestRuntimeNoServer(t)
	store := cron.NewMemoryStore()
	rt.CronStore = store

	cmd := silence(CronCmd(rt))
	require.NoError(t, Execute(cmd, []string{"add", "--schedule", "0 9 * * *", "--type", "reminder", "--payload", `{"msg":"hi"}`}))
	assert.Contains(t, out.String(), "added job:")

	jobs, err := store.List(context.Background())
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	assert.Equal(t, "0 9 * * *", jobs[0].Schedule)
	assert.Equal(t, "reminder", jobs[0].Type)
	assert.Equal(t, `{"msg":"hi"}`, string(jobs[0].Payload))
	assert.True(t, jobs[0].Enabled)
}

func TestCronAddCmd_MissingSchedule(t *testing.T) {
	rt, _, _ := newTestRuntimeNoServer(t)
	rt.CronStore = cron.NewMemoryStore()

	cmd := silence(CronCmd(rt))
	err := Execute(cmd, []string{"add", "--type", "reminder"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--schedule")
}

func TestCronAddCmd_MissingType(t *testing.T) {
	rt, _, _ := newTestRuntimeNoServer(t)
	rt.CronStore = cron.NewMemoryStore()

	cmd := silence(CronCmd(rt))
	err := Execute(cmd, []string{"add", "--schedule", "0 9 * * *"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--type")
}

func TestCronRemoveCmd(t *testing.T) {
	rt, out, _ := newTestRuntimeNoServer(t)
	store := cron.NewMemoryStore()
	rt.CronStore = store

	id, err := store.Add(context.Background(), "0 9 * * *", "reminder", nil)
	require.NoError(t, err)

	cmd := silence(CronCmd(rt))
	require.NoError(t, Execute(cmd, []string{"remove", id}))
	assert.Contains(t, out.String(), "removed job:")

	jobs, _ := store.List(context.Background())
	assert.Empty(t, jobs)
}

func TestCronRemoveCmd_NotFound(t *testing.T) {
	rt, out, _ := newTestRuntimeNoServer(t)
	rt.CronStore = cron.NewMemoryStore()

	cmd := silence(CronCmd(rt))
	require.NoError(t, Execute(cmd, []string{"remove", "nope"}))
	assert.Contains(t, out.String(), "not found")
}

func TestCronEnableDisableCmd(t *testing.T) {
	rt, out, _ := newTestRuntimeNoServer(t)
	store := cron.NewMemoryStore()
	rt.CronStore = store

	id, err := store.Add(context.Background(), "0 9 * * *", "reminder", nil)
	require.NoError(t, err)

	cmd := silence(CronCmd(rt))
	require.NoError(t, Execute(cmd, []string{"disable", id}))
	assert.Contains(t, out.String(), "disabled job:")

	jobs, _ := store.List(context.Background())
	require.Len(t, jobs, 1)
	assert.False(t, jobs[0].Enabled)

	require.NoError(t, Execute(cmd, []string{"enable", id}))
	assert.Contains(t, out.String(), "enabled job:")

	jobs, _ = store.List(context.Background())
	require.Len(t, jobs, 1)
	assert.True(t, jobs[0].Enabled)
}

func TestCronEnableCmd_NotFound(t *testing.T) {
	rt, out, _ := newTestRuntimeNoServer(t)
	rt.CronStore = cron.NewMemoryStore()

	cmd := silence(CronCmd(rt))
	require.NoError(t, Execute(cmd, []string{"enable", "nope"}))
	assert.Contains(t, out.String(), "not found")
}

func TestCronRunCmd(t *testing.T) {
	rt, out, _ := newTestRuntimeNoServer(t)
	store := cron.NewMemoryStore()
	rt.CronStore = store

	var triggered string
	store.RunHook = func(_ context.Context, id string) error {
		triggered = id
		return nil
	}

	id, err := store.Add(context.Background(), "0 9 * * *", "reminder", nil)
	require.NoError(t, err)

	cmd := silence(CronCmd(rt))
	require.NoError(t, Execute(cmd, []string{"run", id}))
	assert.Contains(t, out.String(), "triggered job:")
	assert.Equal(t, id, triggered)
}

func TestCronRunCmd_Disabled(t *testing.T) {
	rt, out, _ := newTestRuntimeNoServer(t)
	store := cron.NewMemoryStore()
	rt.CronStore = store

	id, _ := store.Add(context.Background(), "0 9 * * *", "reminder", nil)
	require.NoError(t, store.SetEnabled(context.Background(), id, false))

	cmd := silence(CronCmd(rt))
	require.NoError(t, Execute(cmd, []string{"run", id}))
	assert.Contains(t, out.String(), "disabled")
}

func TestCronRunCmd_NotFound(t *testing.T) {
	rt, out, _ := newTestRuntimeNoServer(t)
	rt.CronStore = cron.NewMemoryStore()

	cmd := silence(CronCmd(rt))
	require.NoError(t, Execute(cmd, []string{"run", "nope"}))
	assert.Contains(t, out.String(), "not found")
}

func TestCronListCmd_DefaultFileStore(t *testing.T) {
	// When CronStore is nil, the command falls back to a FileStore at
	// $OV_DATA_DIR/cron/jobs.json. Point that at a temp dir so the test
	// doesn't touch the user's filesystem.
	dir := t.TempDir()
	t.Setenv("OV_DATA_DIR", dir)

	rt, out, _ := newTestRuntimeNoServer(t)
	// CronStore is intentionally nil; the command should construct the
	// default FileStore.

	cmd := silence(CronCmd(rt))
	require.NoError(t, Execute(cmd, []string{"list"}))
	assert.Contains(t, out.String(), "No scheduled jobs.")
}

func TestParseSince(t *testing.T) {
	cases := []struct {
		in  string
		ok  bool
		want time.Duration
	}{
		{"7d", true, 7 * 24 * time.Hour},
		{"24h", true, 24 * time.Hour},
		{"30m", true, 30 * time.Minute},
		{"", true, 0},
		{"abc", false, 0},
	}
	for _, c := range cases {
		got, err := parseSince(c.in)
		if !c.ok {
			require.Error(t, err, "in=%q", c.in)
			continue
		}
		require.NoError(t, err, "in=%q", c.in)
		assert.Equal(t, c.want, got, "in=%q", c.in)
	}
}

func TestCronListCmd_Help(t *testing.T) {
	rt, out, _ := newTestRuntimeNoServer(t)
	rt.CronStore = cron.NewMemoryStore()
	cmd := CronCmd(rt)
	cmd.SetOut(out)
	cmd.SetErr(out)
	// `cron list --help` should not error and should print usage.
	require.NoError(t, Execute(cmd, []string{"list", "--help"}))
	assert.Contains(t, out.String(), "Usage")
}
