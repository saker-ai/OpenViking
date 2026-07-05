package agent

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
)

// stubSubagent is a SubagentHandler used by the dispatcher tests. It
// records the task it received and returns a deterministic result.
type stubSubagent struct {
	name     string
	output   string
	err      error
	runs     atomic.Int32
	lastTask string
}

func (s *stubSubagent) Name() string { return s.name }

func (s *stubSubagent) Run(ctx context.Context, taskID, task string, opts SubagentOptions) (SubagentResult, error) {
	s.runs.Add(1)
	s.lastTask = task
	if s.err != nil {
		return SubagentResult{}, s.err
	}
	return SubagentResult{
		TaskID: taskID,
		Label:  opts.Label,
		Status: SubagentStatusOK,
		Output: s.output,
	}, nil
}

func TestSubagentDispatcher_UnknownName(t *testing.T) {
	d := NewSubagentDispatcher()
	_, err := d.Dispatch(context.Background(), "missing", "do X", SubagentOptions{})
	if err == nil {
		t.Fatalf("Dispatch should error for unknown handler")
	}
	var unknown ErrSubagentUnknown
	if !errors.As(err, &unknown) {
		t.Fatalf("err type = %T, want ErrSubagentUnknown", err)
	}
	if unknown.Name != "missing" {
		t.Errorf("name = %q, want %q", unknown.Name, "missing")
	}
}

func TestSubagentDispatcher_DispatchOK(t *testing.T) {
	d := NewSubagentDispatcher()
	h := &stubSubagent{name: "code-interpreter", output: "result=42"}
	d.Register(h)

	res, err := d.Dispatch(context.Background(), "code-interpreter", "compute 6*7", SubagentOptions{Label: "math"})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if res.Status != SubagentStatusOK {
		t.Errorf("Status = %q, want %q", res.Status, SubagentStatusOK)
	}
	if res.Output != "result=42" {
		t.Errorf("Output = %q", res.Output)
	}
	if res.Label != "math" {
		t.Errorf("Label = %q, want math", res.Label)
	}
	if res.TaskID == "" {
		t.Errorf("TaskID should be non-empty")
	}
	if h.runs.Load() != 1 {
		t.Errorf("runs = %d, want 1", h.runs.Load())
	}
	if h.lastTask != "compute 6*7" {
		t.Errorf("lastTask = %q", h.lastTask)
	}
}

func TestSubagentDispatcher_HandlerErrorBecomesErrorStatus(t *testing.T) {
	// When a handler returns an error, the dispatcher surfaces it as
	// a SubagentStatusError result rather than propagating the error.
	// This matches the Python "_announce_result with status=error"
	// path: the agent loop gets a structured result it can relay.
	d := NewSubagentDispatcher()
	d.Register(&stubSubagent{name: "broken", err: errors.New("boom")})

	res, err := d.Dispatch(context.Background(), "broken", "task", SubagentOptions{})
	if err != nil {
		t.Fatalf("Dispatch should not surface handler error directly: %v", err)
	}
	if res.Status != SubagentStatusError {
		t.Errorf("Status = %q, want %q", res.Status, SubagentStatusError)
	}
	if !strings.Contains(res.Output, "boom") {
		t.Errorf("Output = %q, want it to contain the error", res.Output)
	}
}

func TestSubagentDispatcher_AutoLabelFromTask(t *testing.T) {
	d := NewSubagentDispatcher()
	h := &stubSubagent{name: "echo", output: "ok"}
	d.Register(h)

	long := strings.Repeat("a", 50)
	res, _ := d.Dispatch(context.Background(), "echo", long, SubagentOptions{})
	if !strings.HasSuffix(res.Label, "...") {
		t.Errorf("Label = %q, want trailing ...", res.Label)
	}
	if len([]rune(res.Label)) != 30+3 {
		t.Errorf("Label length = %d, want 33", len([]rune(res.Label)))
	}
}

func TestSubagentDispatcher_NamesSorted(t *testing.T) {
	d := NewSubagentDispatcher()
	d.Register(&stubSubagent{name: "zeta", output: ""})
	d.Register(&stubSubagent{name: "alpha", output: ""})
	d.Register(&stubSubagent{name: "mid", output: ""})

	names := d.Names()
	want := []string{"alpha", "mid", "zeta"}
	if len(names) != len(want) {
		t.Fatalf("Names length = %d, want %d", len(names), len(want))
	}
	for i, n := range names {
		if n != want[i] {
			t.Errorf("Names[%d] = %q, want %q", i, n, want[i])
		}
	}
}

func TestSubagentDispatcher_RegisterNil(t *testing.T) {
	d := NewSubagentDispatcher()
	d.Register(nil) // must not panic
	if len(d.Names()) != 0 {
		t.Errorf("Names should be empty after nil register")
	}
}

func TestSubagentResult_Announce(t *testing.T) {
	r := SubagentResult{
		Label:  "research",
		Status: SubagentStatusOK,
		Output: "found 3 pages",
	}
	got := r.Announce("find docs")
	if !strings.Contains(got, "[Subagent 'research' completed successfully]") {
		t.Errorf("Announce missing header: %q", got)
	}
	if !strings.Contains(got, "Task: find docs") {
		t.Errorf("Announce missing task: %q", got)
	}
	if !strings.Contains(got, "found 3 pages") {
		t.Errorf("Announce missing output: %q", got)
	}
}

// TestWebcrawlerSubagent_InvalidURL exercises the handler with a
// non-URL task. The handler must return a structured error result,
// not panic. We do NOT hit the network here.
func TestWebcrawlerSubagent_InvalidURL(t *testing.T) {
	w := &WebcrawlerSubagent{}
	res, err := w.Run(context.Background(), "id-1", "not a url", SubagentOptions{Label: "bad"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != SubagentStatusError {
		t.Errorf("Status = %q, want %q", res.Status, SubagentStatusError)
	}
	if !strings.Contains(res.Output, "not a crawlable") {
		t.Errorf("Output = %q", res.Output)
	}
}

func TestNewSubagentTaskID_Unique(t *testing.T) {
	a := newSubagentTaskID(1)
	b := newSubagentTaskID(2)
	if a == b {
		t.Errorf("IDs should differ: %q == %q", a, b)
	}
	if len(a) != 8 {
		t.Errorf("ID length = %d, want 8", len(a))
	}
}

func TestDeriveSubagentLabel(t *testing.T) {
	cases := []struct {
		task string
		want string
	}{
		{"short", "short"},
		{strings.Repeat("x", 30), strings.Repeat("x", 30)},
		{strings.Repeat("x", 31), strings.Repeat("x", 30) + "..."},
	}
	for _, c := range cases {
		got := deriveSubagentLabel(c.task)
		if got != c.want {
			t.Errorf("deriveSubagentLabel(%q) = %q, want %q", c.task, got, c.want)
		}
	}
}
