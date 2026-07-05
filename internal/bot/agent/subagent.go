// Package agent: subagent delegation.
//
// SubagentDispatcher routes a sub-task to a named subagent (webcrawler,
// code-interpreter, etc.) and returns its result synchronously. This is
// the Go counterpart of bot/vikingbot/agent/subagent.py.
//
// Differences from Python:
//   - Python's SubagentManager.spawn is async (returns immediately, runs
//     the task in the background, and announces the result on the message
//     bus). The Go dispatcher is synchronous: Dispatch blocks until the
//     subagent returns. The caller (agent loop) is free to wrap Dispatch
//     in a goroutine when it wants fire-and-forget semantics.
//   - Python builds a fresh ToolRegistry per subagent and runs an inner
//     agent loop. The Go dispatcher delegates to a SubagentHandler that
//     owns its own execution model. The webcrawler handler wraps the
//     existing internal/parse/accessors/webcrawler accessor; other
//     handlers can be plugged in via Register.
package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/saker-ai/ctxhub/internal/parse"
	"github.com/saker-ai/ctxhub/internal/parse/accessors/webcrawler"
)

// SubagentStatus is the outcome of a subagent run: "ok" or "error".
type SubagentStatus string

const (
	SubagentStatusOK    SubagentStatus = "ok"
	SubagentStatusError SubagentStatus = "error"
)

// SubagentOptions carries per-dispatch hints. The zero value is usable.
//
// Label is a human-readable summary used in the result. When empty, the
// dispatcher derives a label from the task text (first 30 runes).
type SubagentOptions struct {
	Label   string
	WorkspaceID string
}

// SubagentResult is the synchronous return value of Dispatch.
type SubagentResult struct {
	TaskID string
	Label  string
	Status SubagentStatus
	Output string
}

// SubagentHandler is the contract each named subagent implements. The
// dispatcher looks up handlers by Name(); a handler is expected to be
// safe for concurrent use.
type SubagentHandler interface {
	// Name is the subagent identifier (e.g. "webcrawler"). It must be
	// stable across calls so the agent loop can reference it from a
	// tool-call name.
	Name() string
	// Run executes the sub-task and returns its result. The dispatcher
	// assigns TaskID; handlers should embed it in the result.
	Run(ctx context.Context, taskID, task string, opts SubagentOptions) (SubagentResult, error)
}

// SubagentDispatcher routes sub-tasks to named handlers. The zero value
// is NOT usable; use NewSubagentDispatcher.
type SubagentDispatcher struct {
	mu       sync.RWMutex
	handlers map[string]SubagentHandler
	ids      atomic.Int64
}

// NewSubagentDispatcher returns an empty dispatcher.
func NewSubagentDispatcher() *SubagentDispatcher {
	return &SubagentDispatcher{handlers: make(map[string]SubagentHandler)}
}

// Register adds (or replaces) a subagent handler. Handlers are looked up
// by handler.Name(); registering the same name twice replaces the prior
// handler.
func (d *SubagentDispatcher) Register(h SubagentHandler) {
	if h == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.handlers == nil {
		d.handlers = make(map[string]SubagentHandler)
	}
	d.handlers[h.Name()] = h
}

// Names returns the registered handler names in registration-agnostic
// alphabetical order.
func (d *SubagentDispatcher) Names() []string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]string, 0, len(d.handlers))
	for name := range d.handlers {
		out = append(out, name)
	}
	// Deterministic order for stable tool listing.
	sortStrings(out)
	return out
}

// Dispatch routes task to the named subagent. Returns
// ErrSubagentUnknown when no handler is registered under name.
//
// The dispatcher assigns a short TaskID (8 hex chars) and derives a
// label from opts.Label or the first 30 runes of task. The handler's
// returned SubagentResult is augmented with these fields when empty.
func (d *SubagentDispatcher) Dispatch(ctx context.Context, name, task string, opts SubagentOptions) (SubagentResult, error) {
	d.mu.RLock()
	h, ok := d.handlers[name]
	d.mu.RUnlock()
	if !ok {
		return SubagentResult{}, ErrSubagentUnknown{Name: name}
	}
	taskID := newSubagentTaskID(d.ids.Add(1))
	label := opts.Label
	if label == "" {
		label = deriveSubagentLabel(task)
	}
	res, err := h.Run(ctx, taskID, task, opts)
	if err != nil {
		return SubagentResult{
			TaskID: taskID,
			Label:  label,
			Status: SubagentStatusError,
			Output: "error: " + err.Error(),
		}, nil
	}
	if res.TaskID == "" {
		res.TaskID = taskID
	}
	if res.Label == "" {
		res.Label = label
	}
	if res.Status == "" {
		res.Status = SubagentStatusOK
	}
	return res, nil
}

// ErrSubagentUnknown is returned by Dispatch when no handler is
// registered for the requested name.
type ErrSubagentUnknown struct{ Name string }

func (e ErrSubagentUnknown) Error() string {
	return fmt.Sprintf("subagent: no handler registered for %q", e.Name)
}

// WebcrawlerSubagent is a SubagentHandler that drives the existing
// internal/parse/accessors/webcrawler accessor. It is the Go counterpart
// of routing a "search the web" sub-task to a webcrawler subagent.
//
// The handler does NOT run an inner agent loop; it fetches the requested
// URL and returns a short summary as the result. This matches the
// "search the web" tool shape the agent loop expects.
type WebcrawlerSubagent struct {
	// Static is the static-HTML crawler. When nil, a default
	// StaticAccessor is used. The default is the same one registered
	// under the "crawl" scheme in internal/parse/accessors/webcrawler.
	Static *webcrawler.StaticAccessor
}

// Name returns "webcrawler".
func (w *WebcrawlerSubagent) Name() string { return "webcrawler" }

// Run fetches the URL in task via the webcrawler accessor and returns
// a short summary. task is treated as a URL; when it is not a valid
// http/https URL, Run returns an error result.
func (w *WebcrawlerSubagent) Run(ctx context.Context, taskID, task string, opts SubagentOptions) (SubagentResult, error) {
	acc := w.Static
	if acc == nil {
		acc = &webcrawler.StaticAccessor{UserAgent: "OpenViking-Crawler/1.0"}
	}
	if !acc.CanHandle(task, parse.AccessorOptions{Site: true}) {
		return SubagentResult{
			TaskID: taskID,
			Label:  opts.Label,
			Status: SubagentStatusError,
			Output: "webcrawler: task is not a crawlable http/https URL: " + task,
		}, nil
	}
	res, err := acc.Fetch(ctx, task, parse.AccessorOptions{Site: true, Depth: 1, Limit: 5})
	if err != nil {
		return SubagentResult{
			TaskID: taskID,
			Label:  opts.Label,
			Status: SubagentStatusError,
			Output: "webcrawler: fetch failed: " + err.Error(),
		}, nil
	}
	defer func() { res.Cleanup() }()
	visited := 0
	if v, ok := res.Meta["visited"].(int); ok {
		visited = v
	}
	out := fmt.Sprintf("webcrawler: fetched %d page(s) from %s (stored under %s)", visited, res.OriginalSource, res.Path)
	return SubagentResult{
		TaskID: taskID,
		Label:  opts.Label,
		Status: SubagentStatusOK,
		Output: out,
	}, nil
}

// newSubagentTaskID returns a short, monotonically-increasing ID. The
// format mirrors the Python counterpart (8-char hex-ish), but uses a
// counter so IDs are unique within a process.
func newSubagentTaskID(n int64) string {
	return fmt.Sprintf("%08x", n)
}

// deriveSubagentLabel returns a short label for task: the first 30
// runes, with "..." appended when truncated. Mirrors the Python
// SubagentManager.spawn default label.
func deriveSubagentLabel(task string) string {
	r := []rune(task)
	if len(r) <= 30 {
		return task
	}
	return string(r[:30]) + "..."
}

// sortStrings is a tiny helper to avoid pulling in sort just for one
// call. It uses insertion sort — Names() returns small slices.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		j := i
		for j > 0 && s[j-1] > s[j] {
			s[j-1], s[j] = s[j], s[j-1]
			j--
		}
	}
}

// subagentNow is the time.Now used for SubagentResult timestamps when
// handlers want them. Kept as a function variable for tests.
var subagentNow = time.Now

// announceHeader is the prefix the dispatcher suggests when wrapping a
// SubagentResult into a chat message. Mirrors the Python
// _announce_result header format.
const announceHeader = "[Subagent '%s' %s]"

// Announce formats a SubagentResult as a chat-ready summary string.
// Handlers and callers can use this to surface the result to the user
// without exposing task IDs or internal labels.
func (r SubagentResult) Announce(task string) string {
	status := "completed successfully"
	if r.Status == SubagentStatusError {
		status = "failed"
	}
	header := fmt.Sprintf(announceHeader, r.Label, status)
	return strings.Join([]string{
		header,
		"",
		"Task: " + task,
		"",
		"Result:",
		r.Output,
		"",
		"Summarize this naturally for the user. Keep it brief (1-2 sentences). Do not mention technical details like \"subagent\" or task IDs.",
	}, "\n")
}
