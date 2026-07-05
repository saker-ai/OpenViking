package doctor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/saker-ai/ctxhub/internal/config"
)

// probeDisk inspects each configured storage path. It reports exists,
// writable, free_bytes, and total_bytes per path. Missing paths are reported
// with status fail (the server would fail to start); unwritable paths are
// reported as fail too. Paths that exist and are writable report ok.
func probeDisk(ctx context.Context, cfg *config.Config) []ProbeResult {
	if cfg == nil {
		return []ProbeResult{{Name: "disk", Status: StatusSkip, Detail: "config not loaded"}}
	}
	paths := diskPaths(cfg)
	results := make([]ProbeResult, 0, len(paths))
	for _, p := range paths {
		results = append(results, probeOneDiskPath(p))
	}
	return results
}

// diskPaths enumerates the storage paths implied by cfg. Duplicates are
// preserved so the report surfaces every configured path; the OS layer is
// the source of truth for dedup.
func diskPaths(cfg *config.Config) []string {
	var paths []string
	add := func(p string) {
		if p == "" {
			return
		}
		paths = append(paths, p)
	}
	add(cfg.Server.UploadDir)
	add(cfg.VectorDB.Local.Path)
	// ragfs local mounts.
	for _, m := range cfg.RAGFS.Mounts {
		if m.Backend == "local" {
			add(m.Path)
		}
	}
	// Logs and temp dirs: standard locations derived from env or defaults.
	add(tempDir())
	return paths
}

// tempDir returns the configured temp directory (TMPDIR or /tmp).
func tempDir() string {
	if t := os.Getenv("TMPDIR"); t != "" {
		return t
	}
	return "/tmp"
}

// probeOneDiskPath inspects one path and returns a ProbeResult carrying the
// disk usage fields as Extra.
func probeOneDiskPath(p string) ProbeResult {
	name := "disk." + filepath.Clean(p)
	abs, err := filepath.Abs(p)
	if err != nil {
		return ProbeResult{Name: name, Status: StatusFail, Error: "abs: " + err.Error()}
	}
	st, err := os.Stat(abs)
	exists := err == nil
	writable := false
	var free, total uint64
	if exists {
		writable = isWritableDir(abs, st.IsDir())
		free, total = diskUsage(abs)
	}
	status := StatusOK
	if !exists {
		status = StatusFail
	} else if !writable {
		status = StatusFail
	}
	return ProbeResult{
		Name:   name,
		Status: status,
		Detail: fmt.Sprintf("exists=%v writable=%v", exists, writable),
		Extra: map[string]any{
			"path":        abs,
			"exists":      exists,
			"writable":    writable,
			"free_bytes":  free,
			"total_bytes": total,
		},
	}
}

// isWritableDir reports whether the path is writable. For directories we
// test by attempting to create a temp file; for files we attempt to open
// the file for appending.
func isWritableDir(p string, isDir bool) bool {
	if isDir {
		f, err := os.CreateTemp(p, ".ov-doctor-disk-")
		if err != nil {
			return false
		}
		_ = f.Close()
		_ = os.Remove(f.Name())
		return true
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// diskUsage returns (free, total) bytes for the filesystem holding p.
// Returns (0, 0) when statfs fails (e.g. path does not exist on Linux).
func diskUsage(p string) (free, total uint64) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(p, &stat); err != nil {
		return 0, 0
	}
	free = stat.Bfree * uint64(stat.Bsize)
	total = (stat.Blocks) * uint64(stat.Bsize)
	return free, total
}
