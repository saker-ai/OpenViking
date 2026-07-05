package ragfs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"time"
)

// MultiWriteFS replicates writes across a primary backend plus zero or
// more backup backends. Reads come from the primary; if the primary
// fails, callers may fall back to a backup via ReadFromBackup.
//
// Mirrors the Rust core/multibackend_wrapper.rs MultiWriteFS.
//
// Consistency model (per design doc 7.2.8): primary is strongly
// consistent; backups are eventually consistent. Writes are fanned out
// to backups synchronously when Sync=true (blocking until all backups
// acknowledge or fail) and asynchronously otherwise. The sync log
// records every replication attempt.
type MultiWriteFS struct {
	primary  FileSystem
	backups  []FileSystem
	redirect RedirectPolicy
	syncLog  *SyncLogStore
	sync     bool

	// asyncFanout: in async mode, failed/pending backups are recorded in
	// the sync log for a future reconcile pass (P5+). We still attempt
	// the fanout in a goroutine so that fast networks catch up quickly.
	asyncFanout bool

	// pendingMu guards concurrent goroutine fanout for the same path.
	pendingMu sync.Mutex
	pending   map[string]chan struct{}
}

// MultiWriteConfig is the construction-time tuning for MultiWriteFS.
type MultiWriteConfig struct {
	Primary  FileSystem
	Backups  []FileSystem
	Redirect RedirectPolicy
	SyncLog  *SyncLogStore
	// Sync=true blocks on backup replication; Sync=false fans out async.
	Sync bool
	// AsyncFanout=true (default) launches best-effort goroutines even in
	// async mode. Set to false to defer all replication to reconcile().
	AsyncFanout bool
}

// NewMultiWriteFS constructs a multi-write wrapper. primary must be
// non-nil; backups may be empty. cfg.SyncLog may be nil (no logging).
func NewMultiWriteFS(cfg MultiWriteConfig) (*MultiWriteFS, error) {
	if cfg.Primary == nil {
		return nil, wrapRAGFS(errors.New("multiwrite: primary backend is nil"))
	}
	backups := make([]FileSystem, 0, len(cfg.Backups))
	for _, b := range cfg.Backups {
		if b == nil {
			return nil, wrapRAGFS(errors.New("multiwrite: backup backend is nil"))
		}
		// copy to avoid caller aliasing
		backups = append(backups, b)
	}
	async := cfg.AsyncFanout
	if !cfg.Sync {
		// default async fanout to true unless caller opts out
		async = true
	}
	return &MultiWriteFS{
		primary:     cfg.Primary,
		backups:     backups,
		redirect:    cfg.Redirect,
		syncLog:     cfg.SyncLog,
		sync:        cfg.Sync,
		asyncFanout: async,
		pending:     make(map[string]chan struct{}),
	}, nil
}

// Primary returns the primary backend.
func (m *MultiWriteFS) Primary() FileSystem { return m.primary }

// Backups returns a snapshot of the backup backends.
func (m *MultiWriteFS) Backups() []FileSystem {
	out := make([]FileSystem, len(m.backups))
	copy(out, m.backups)
	return out
}

// --- ServicePlugin ---

func (m *MultiWriteFS) Name() string { return "multiwrite" }

func (m *MultiWriteFS) Validate(cfg *PluginConfig) error {
	if err := m.primary.Validate(cfg); err != nil {
		return err
	}
	for _, b := range m.backups {
		if err := b.Validate(cfg); err != nil {
			return err
		}
	}
	return nil
}

func (m *MultiWriteFS) Initialize(ctx context.Context, cfg *PluginConfig) error {
	if err := m.primary.Initialize(ctx, cfg); err != nil {
		return err
	}
	for _, b := range m.backups {
		if err := b.Initialize(ctx, cfg); err != nil {
			return err
		}
	}
	return nil
}

func (m *MultiWriteFS) HealthCheck(ctx context.Context) error {
	if err := m.primary.HealthCheck(ctx); err != nil {
		return err
	}
	for _, b := range m.backups {
		if err := b.HealthCheck(ctx); err != nil {
			return err
		}
	}
	return nil
}

// --- FileSystem ---

func (m *MultiWriteFS) Create(ctx context.Context, p string, isDir bool) error {
	if err := m.primary.Create(ctx, p, isDir); err != nil {
		return err
	}
	m.fanout(ctx, p, "", OpWrite, func(b FileSystem) error {
		return b.Create(ctx, p, isDir)
	})
	return nil
}

func (m *MultiWriteFS) Mkdir(ctx context.Context, p string, perm os.FileMode) error {
	if err := m.primary.Mkdir(ctx, p, perm); err != nil {
		return err
	}
	m.fanout(ctx, p, "", OpWrite, func(b FileSystem) error {
		return b.Mkdir(ctx, p, perm)
	})
	return nil
}

func (m *MultiWriteFS) Remove(ctx context.Context, p string, recursive bool) error {
	if err := m.primary.Remove(ctx, p, recursive); err != nil {
		return err
	}
	m.fanout(ctx, p, "", OpDelete, func(b FileSystem) error {
		return b.Remove(ctx, p, recursive)
	})
	return nil
}

func (m *MultiWriteFS) Read(ctx context.Context, p string, w io.Writer) error {
	// Read follows redirect pointers (large-file redirect) when present.
	if target, ok, err := FollowRedirectTarget(ctx, m.primary, p); err != nil {
		return err
	} else if ok {
		// large-object target: delegate to primary's Read of the target
		// path. Backends may override this by registering a redirect
		// resolver; the default behavior reads the sidecar bytes from
		// the target path on the primary backend.
		return m.primary.Read(ctx, target, w)
	}
	if err := m.primary.Read(ctx, p, w); err != nil {
		// fall back to first backup that has the path
		for _, b := range m.backups {
			if e2 := b.Read(ctx, p, w); e2 == nil {
				return nil
			}
		}
		return err
	}
	return nil
}

func (m *MultiWriteFS) Write(ctx context.Context, p string, r io.Reader, perm os.FileMode) error {
	// Buffer the write so that backup fanout can replay the bytes. This
	// is required because io.Reader is one-shot. The size also drives
	// redirect policy.
	buf, size, err := readAllForReplay(r)
	if err != nil {
		return wrapRAGFS(err)
	}

	// Large-file redirect: if the write exceeds FileOverSize, store the
	// bytes at a target URI and write a redirect pointer instead.
	if m.redirect.ShouldRedirect(size, p) {
		target := m.buildRedirectTarget(p, size)
		if err := m.primary.Write(ctx, target, bytes.NewReader(buf), perm); err != nil {
			return err
		}
		redir := &Redirect{
			Type:   RedirectFileOverSize,
			Target: target,
			Size:   size,
		}
		if err := WriteRedirect(ctx, m.primary, p, redir); err != nil {
			return err
		}
		m.fanout(ctx, p, "", OpWrite, func(b FileSystem) error {
			if err := b.Write(ctx, target, bytes.NewReader(buf), perm); err != nil {
				return err
			}
			return WriteRedirect(ctx, b, p, redir)
		})
		return nil
	}

	// Normal write: primary first, then backups.
	if err := m.primary.Write(ctx, p, bytes.NewReader(buf), perm); err != nil {
		return err
	}
	m.fanout(ctx, p, "", OpWrite, func(b FileSystem) error {
		return b.Write(ctx, p, bytes.NewReader(buf), perm)
	})
	return nil
}

func (m *MultiWriteFS) ReadDir(ctx context.Context, p string) ([]*TreeEntry, error) {
	return m.primary.ReadDir(ctx, p)
}

func (m *MultiWriteFS) Stat(ctx context.Context, p string) (*FileInfo, error) {
	return m.primary.Stat(ctx, p)
}

func (m *MultiWriteFS) Rename(ctx context.Context, oldP, newP string) error {
	if err := m.primary.Rename(ctx, oldP, newP); err != nil {
		return err
	}
	m.fanout(ctx, oldP, newP, OpWrite, func(b FileSystem) error {
		return b.Rename(ctx, oldP, newP)
	})
	return nil
}

func (m *MultiWriteFS) Copy(ctx context.Context, srcP, dstP string) error {
	if err := m.primary.Copy(ctx, srcP, dstP); err != nil {
		// If primary doesn't implement Copy, emulate via Read+Write.
		if errors.Is(err, ErrUnsupported) {
			return m.copyViaRead(ctx, srcP, dstP)
		}
		return err
	}
	m.fanout(ctx, srcP, dstP, OpWrite, func(b FileSystem) error {
		if err := b.Copy(ctx, srcP, dstP); err != nil {
			if errors.Is(err, ErrUnsupported) {
				return m.copyViaReadOn(ctx, b, srcP, dstP)
			}
			return err
		}
		return nil
	})
	return nil
}

func (m *MultiWriteFS) Chmod(ctx context.Context, p string, perm os.FileMode) error {
	if err := m.primary.Chmod(ctx, p, perm); err != nil {
		return err
	}
	m.fanout(ctx, p, "", OpWrite, func(b FileSystem) error {
		return b.Chmod(ctx, p, perm)
	})
	return nil
}

func (m *MultiWriteFS) Grep(ctx context.Context, pattern, p string, recursive bool) ([]GrepMatch, error) {
	return m.primary.Grep(ctx, pattern, p, recursive)
}

func (m *MultiWriteFS) TreeDirectory(ctx context.Context, p string, depth int) ([]*TreeEntry, error) {
	return m.primary.TreeDirectory(ctx, p, depth)
}

// --- internals ---

// fanout replicates an op to all backups. In sync mode it blocks until
// every backup acks or fails; in async mode it launches a goroutine and
// returns immediately, recording any pending state in the sync log.
func (m *MultiWriteFS) fanout(ctx context.Context, p, _newP string, op OpType, fn func(b FileSystem) error) {
	if len(m.backups) == 0 {
		return
	}
	if m.sync {
		for _, b := range m.backups {
			if err := fn(b); err != nil {
				m.recordSync(p, b.Name(), op, err, 0)
			} else {
				m.recordSync(p, b.Name(), OpSync, nil, 0)
			}
		}
		return
	}
	// async: best-effort goroutine
	if !m.asyncFanout {
		for _, b := range m.backups {
			m.recordSync(p, b.Name(), OpPending, errors.New("deferred"), 0)
		}
		return
	}
	go func() {
		for _, b := range m.backups {
			if err := fn(b); err != nil {
				m.recordSync(p, b.Name(), op, err, 0)
			} else {
				m.recordSync(p, b.Name(), OpSync, nil, 0)
			}
		}
	}()
}

// recordSync appends a sync log entry if a store is configured. Errors
// are swallowed: sync log failures must not break the write path.
func (m *MultiWriteFS) recordSync(p, backend string, op OpType, cause error, retry int) {
	if m.syncLog == nil {
		return
	}
	e := SyncLogEntry{
		Op:         op,
		Path:       p,
		Backend:    backend,
		Timestamp:  time.Now().UTC(),
		RetryCount: retry,
	}
	if cause != nil {
		e.Status = cause.Error()
	}
	_ = m.syncLog.Append(context.Background(), e)
}

// buildRedirectTarget returns the URI where the large object's bytes
// should live. The convention is "<path>.blob" sibling file.
func (m *MultiWriteFS) buildRedirectTarget(p string, size int64) string {
	return HiddenSidecar(p, ".blob")
}

// copyViaRead emulates Copy on the primary via Read+Write.
func (m *MultiWriteFS) copyViaRead(ctx context.Context, srcP, dstP string) error {
	return m.copyViaReadOn(ctx, m.primary, srcP, dstP)
}

func (m *MultiWriteFS) copyViaReadOn(ctx context.Context, fs FileSystem, srcP, dstP string) error {
	var buf bytes.Buffer
	if err := fs.Read(ctx, srcP, &buf); err != nil {
		return err
	}
	return fs.Write(ctx, dstP, bytes.NewReader(buf.Bytes()), 0o644)
}

// readAllForReplay reads r fully into memory, returning the bytes and
// size. Used so that backup fanout can replay the same bytes.
func readAllForReplay(r io.Reader) ([]byte, int64, error) {
	if r == nil {
		return nil, 0, nil
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		return nil, 0, err
	}
	return buf.Bytes(), int64(buf.Len()), nil
}
