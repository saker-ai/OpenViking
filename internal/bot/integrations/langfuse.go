// Package integrations wires third-party observability platforms.
// The Langfuse integration posts agent traces to the Langfuse ingest
// HTTP API.
//
// We use direct HTTP rather than the official SDK because the
// `github.com/langfuse/langfuse-go` module exists only as a placeholder
// repo (LICENSE + README, no .go files at v0.0.0-20250303). The ingest
// contract is a single POST, so hand-writing is the right call today.
// Tracked as a known gap in docs/design/go-rewrite-known-gaps.md (§1.5);
// revisit if Langfuse ships a real Go SDK.
//
// Langfuse ingest endpoint:
//
//	POST {base_url}/api/public/ingestion
//	Authorization: Basic base64(public_key:secret_key)
//	Body: JSON "batch" of events per the Langfuse ingestion schema.
package integrations

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/saker-ai/ctxhub/internal/bot/config"
)

// Langfuse is the HTTP-only Langfuse client. It batches events and
// flushes them on a timer or when the buffer is full.
type Langfuse struct {
	cfg     config.LangfuseConfig
	client  *http.Client
	auth    string
	mu      sync.Mutex
	buf     []langfuseEvent
	flushCh chan struct{}
	stopCh  chan struct{}
	wg      sync.WaitGroup
}

// New constructs a Langfuse client. The client is inactive until
// Start is called.
func New(cfg config.LangfuseConfig) *Langfuse {
	auth := ""
	if cfg.PublicKey != "" && cfg.SecretKey != "" {
		auth = "Basic " + base64.StdEncoding.EncodeToString([]byte(cfg.PublicKey+":"+cfg.SecretKey))
	}
	return &Langfuse{
		cfg:     cfg,
		client:  &http.Client{Timeout: 10 * time.Second},
		auth:    auth,
		flushCh: make(chan struct{}, 1),
		stopCh:  make(chan struct{}),
	}
}

// langfuseEvent is a single ingestion event. The body matches the
// Langfuse SDK's event schema closely enough for the v1 ingest API.
type langfuseEvent struct {
	ID   string    `json:"id"`
	Type string    `json:"type"` // "trace-create", "generation-create", etc.
	Time time.Time `json:"timestamp"`
	Body any       `json:"body"`
}

// Trace queues a trace-create event. name is the trace name; metadata
// is opaque. The event is batched and flushed asynchronously.
func (l *Langfuse) Trace(id, name string, metadata map[string]any) {
	if !l.cfg.Enabled || id == "" {
		return
	}
	l.enqueue(langfuseEvent{
		ID:   id,
		Type: "trace-create",
		Time: time.Now(),
		Body: map[string]any{
			"id":       id,
			"name":     name,
			"metadata": metadata,
		},
	})
}

// Generation queues a generation-create event linked to traceID.
func (l *Langfuse) Generation(traceID, name, prompt, completion string, metadata map[string]any) {
	if !l.cfg.Enabled || traceID == "" {
		return
	}
	l.enqueue(langfuseEvent{
		ID:   traceID + "-gen-" + name,
		Type: "generation-create",
		Time: time.Now(),
		Body: map[string]any{
			"id":         traceID + "-gen-" + name,
			"traceId":    traceID,
			"name":       name,
			"prompt":     prompt,
			"completion": completion,
			"metadata":   metadata,
		},
	})
}

func (l *Langfuse) enqueue(ev langfuseEvent) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf = append(l.buf, ev)
	if len(l.buf) >= 64 {
		select {
		case l.flushCh <- struct{}{}:
		default:
		}
	}
}

// Start launches the flush loop. It blocks until ctx is canceled or
// Stop is called. When cfg.Enabled is false it returns immediately.
func (l *Langfuse) Start(ctx context.Context) error {
	if !l.cfg.Enabled {
		return nil
	}
	flushInterval := time.Duration(l.cfg.FlushInterval) * time.Second
	if flushInterval <= 0 {
		flushInterval = 10 * time.Second
	}
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		for {
			select {
			case <-ctx.Done():
				l.flush(context.Background())
				return
			case <-l.stopCh:
				l.flush(context.Background())
				return
			case <-ticker.C:
				l.flush(ctx)
			case <-l.flushCh:
				l.flush(ctx)
			}
		}
	}()
	return nil
}

// Stop signals the flush loop to exit. It blocks until pending events
// are flushed. Idempotent.
func (l *Langfuse) Stop() {
	select {
	case <-l.stopCh:
		return
	default:
		close(l.stopCh)
	}
	l.wg.Wait()
}

// flush POSTs the buffered batch to the Langfuse ingest endpoint.
func (l *Langfuse) flush(ctx context.Context) {
	l.mu.Lock()
	batch := l.buf
	l.buf = nil
	l.mu.Unlock()
	if len(batch) == 0 {
		return
	}
	body, _ := json.Marshal(map[string]any{
		"batch": batch,
	})
	url := strings.TrimRight(l.cfg.BaseURL, "/") + "/api/public/ingestion"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if l.auth != "" {
		req.Header.Set("Authorization", l.auth)
	}
	resp, err := l.client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		// Re-enqueue on failure so we don't lose telemetry. Cap the
		// buffer at 1024 to bound memory.
		l.mu.Lock()
		if len(l.buf) < 1024 {
			l.buf = append(batch, l.buf...)
		}
		l.mu.Unlock()
	}
}

// Pending returns the number of buffered (unflushed) events. For tests.
func (l *Langfuse) Pending() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buf)
}

// String returns a compact description for logging.
func (l *Langfuse) String() string {
	return fmt.Sprintf("langfuse{enabled: %v, base: %s}", l.cfg.Enabled, l.cfg.BaseURL)
}
