package prompts

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watcher wraps fsnotify to hot-reload a Store when template files
// change. It debounces rapid event bursts (editors often trigger
// Write+Create+Rename in quick succession) and calls Store.Reload
// at most once per debounce window.
//
// Watcher is safe for concurrent use. Stop closes the underlying
// fsnotify watcher and waits for the event loop to exit.
type Watcher struct {
	store   *Store
	dir     string
	fsn     *fsnotify.Watcher
	closed  chan struct{}
	done    chan struct{}
	mu      sync.Mutex
	stopped bool

	// debounceNanos is the quiet period before Reload fires. Defaults
	// to 200ms; configurable for tests. Stored as nanoseconds in an
	// atomic so SetDebounce can be called concurrently with the loop.
	debounceNanos atomic.Int64

	// onReload is invoked after each successful Reload. Optional;
	// used by tests to synchronize on reload completion.
	onReload func()
}

// NewWatcher creates a Watcher for the given Store. The Store must
// have been loaded with a non-empty disk directory; watching the
// embedded archive is not supported (it is immutable).
//
// The watcher recursively adds the directory and every subdirectory
// it discovers. Hidden directories (leading ".") are skipped.
func NewWatcher(store *Store) (*Watcher, error) {
	if store == nil {
		return nil, errors.New("prompts: nil store")
	}
	dir := store.Dir()
	if dir == "" {
		return nil, errors.New("prompts: cannot watch embedded archive; configure a disk directory")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("prompts: resolve dir: %w", err)
	}
	fsn, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("prompts: fsnotify: %w", err)
	}
	w := &Watcher{
		store:  store,
		dir:    abs,
		fsn:    fsn,
		closed: make(chan struct{}),
		done:   make(chan struct{}),
	}
	w.debounceNanos.Store(int64(200 * time.Millisecond))
	if err := w.addRecursive(abs); err != nil {
		_ = fsn.Close()
		return nil, err
	}
	go w.loop()
	return w, nil
}

// addRecursive adds dir and every subdirectory to the fsnotify
// watcher. New subdirectories created at runtime are picked up by
// the event loop (Create events on directories trigger an add).
func (w *Watcher) addRecursive(root string) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return nil
		}
		base := filepath.Base(path)
		if len(base) > 1 && base[0] == '.' {
			return filepath.SkipDir
		}
		if err := w.fsn.Add(path); err != nil {
			return fmt.Errorf("prompts: watch %s: %w", path, err)
		}
		return nil
	})
}

// loop reads fsnotify events and debounces them into Reload calls.
// It exits when Closed is signaled or the fsnotify watcher dies.
//
// The debounce uses a single resettable timer: every relevant event
// resets the timer; when it finally fires, we call Reload.
func (w *Watcher) loop() {
	defer close(w.done)
	var timer *time.Timer
	var timerC <-chan time.Time
	for {
		// Drain any fired timer first so we don't accumulate stale
		// reloads when events keep arriving.
		if timer != nil && timerC == nil {
			timer.Stop()
			timer = nil
		}
		select {
		case <-w.closed:
			if timer != nil {
				timer.Stop()
			}
			return
		case ev, ok := <-w.fsn.Events:
			if !ok {
				if timer != nil {
					timer.Stop()
				}
				return
			}
			if !w.relevant(ev) {
				continue
			}
			if ev.Has(fsnotify.Create) {
				if info, statErr := os.Stat(ev.Name); statErr == nil && info.IsDir() {
					_ = w.addRecursive(ev.Name)
				}
			}
			if timer == nil {
				timer = time.NewTimer(w.currentDebounce())
				timerC = timer.C
			} else {
				timer.Reset(w.currentDebounce())
				timerC = timer.C
			}
		case <-timerC:
			timer.Stop()
			timer = nil
			timerC = nil
			if _, err := w.store.Reload(); err == nil {
				w.mu.Lock()
				cb := w.onReload
				w.mu.Unlock()
				if cb != nil {
					cb()
				}
			}
		case _, ok := <-w.fsn.Errors:
			if !ok {
				if timer != nil {
					timer.Stop()
				}
				return
			}
		}
	}
}

// relevant reports whether an fsnotify event should trigger a reload.
// We only care about Write, Create, Remove, and Rename on .yaml/.yml
// files (the rest are noise from chmod etc.).
func (w *Watcher) relevant(ev fsnotify.Event) bool {
	if !ev.Has(fsnotify.Write) && !ev.Has(fsnotify.Create) &&
		!ev.Has(fsnotify.Remove) && !ev.Has(fsnotify.Rename) {
		return false
	}
	ext := strings.ToLower(filepath.Ext(ev.Name))
	return ext == ".yaml" || ext == ".yml"
}

// Stop closes the watcher and waits for the event loop to exit. Safe
// to call multiple times; subsequent calls are no-ops.
func (w *Watcher) Stop() {
	w.mu.Lock()
	if w.stopped {
		w.mu.Unlock()
		return
	}
	w.stopped = true
	w.mu.Unlock()
	close(w.closed)
	_ = w.fsn.Close()
	<-w.done
}

// SetOnReload installs a callback invoked after each successful
// Reload. Used by tests to synchronize on reload completion. The
// callback runs in the watcher goroutine; it must not block.
func (w *Watcher) SetOnReload(fn func()) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.onReload = fn
}

// SetDebounce overrides the debounce window. Safe to call any time;
// the next event uses the new value. Intended for tests.
func (w *Watcher) SetDebounce(d time.Duration) {
	w.debounceNanos.Store(int64(d))
}

// currentDebounce returns the configured debounce window.
func (w *Watcher) currentDebounce() time.Duration {
	return time.Duration(w.debounceNanos.Load())
}
