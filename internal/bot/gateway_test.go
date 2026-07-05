package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/domain"
)

// newGatewayTestRouter builds a gin engine with the Gateway's six routes
// mounted at /bot/v1 (mirrors routers.RegisterBot without the routers
// dependency) and a minimal error middleware that renders *domain.AppError
// as a structured envelope so tests can assert on the response shape.
func newGatewayTestRouter(t *testing.T, g *Gateway) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(renderAppErrorForTest())
	grp := r.Group("/bot/v1")
	grp.POST("/channels/:channel/webhook", g.HandleWebhook)
	grp.POST("/messages", g.SendMessage)
	grp.GET("/sessions", g.ListSessions)
	grp.GET("/sessions/:id", g.GetSession)
	grp.GET("/channels", g.ListChannels)
	grp.GET("/health", g.Health)
	return r
}

// renderAppErrorForTest mirrors server.errorMiddleware so *domain.AppError
// is rendered as { "error": { "code", "message" } }. Generic errors render
// as 500 INTERNAL_ERROR.
func renderAppErrorForTest() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()
		if len(c.Errors) == 0 {
			return
		}
		first := c.Errors[0]
		var appErr *domain.AppError
		if errors.As(first.Err, &appErr) {
			status := appErr.Status
			if status == 0 {
				status = http.StatusInternalServerError
			}
			c.JSON(status, gin.H{
				"error": gin.H{
					"code":    appErr.Code,
					"message": appErr.Error(),
				},
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": gin.H{
				"code":    domain.CodeInternalError,
				"message": "internal error",
			},
		})
	}
}

// stubChannel is a ChannelAdapter that records every call. Safe for
// concurrent use because tests may run handlers in parallel within a
// single router.
type stubChannel struct {
	mu           sync.Mutex
	name         string
	webhookCalls int
	sendCalls    int
	lastPayload  []byte
	lastChatID   string
	lastText     string
	webhookErr   error
	sendErr      error
}

func (s *stubChannel) Name() string { return s.name }

func (s *stubChannel) HandleWebhook(ctx context.Context, payload []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.webhookCalls++
	s.lastPayload = payload
	return s.webhookErr
}

func (s *stubChannel) Send(ctx context.Context, chatID, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sendCalls++
	s.lastChatID = chatID
	s.lastText = text
	return s.sendErr
}

// stubSessionStore is a SessionStore that returns canned data.
type stubSessionStore struct {
	list []SessionSummary
	get  *SessionSummary
	err  error
}

func (s *stubSessionStore) ListSessions(ctx context.Context) ([]SessionSummary, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.list, nil
}

func (s *stubSessionStore) GetSession(ctx context.Context, id string) (*SessionSummary, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.get, nil
}

// TestGateway_HandleWebhookDelegates verifies the webhook route extracts
// the :channel parameter, reads the body, and delegates to the adapter.
func TestGateway_HandleWebhookDelegates(t *testing.T) {
	ch := &stubChannel{name: "qq"}
	g := NewGateway()
	g.RegisterChannel(ch)
	r := newGatewayTestRouter(t, g)

	body := bytes.NewBufferString(`{"event":"message","content":"hi"}`)
	req := httptest.NewRequest(http.MethodPost, "/bot/v1/channels/qq/webhook", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, 1, ch.webhookCalls)
	assert.Equal(t, `{"event":"message","content":"hi"}`, string(ch.lastPayload))
	var resp struct {
		Received bool   `json:"received"`
		Channel  string `json:"channel"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.True(t, resp.Received)
	assert.Equal(t, "qq", resp.Channel)
}

// TestGateway_HandleWebhookUnknownChannel verifies an unregistered
// channel returns 404 RESOURCE_NOT_FOUND.
func TestGateway_HandleWebhookUnknownChannel(t *testing.T) {
	g := NewGateway()
	r := newGatewayTestRouter(t, g)

	req := httptest.NewRequest(http.MethodPost, "/bot/v1/channels/unknown/webhook", bytes.NewBufferString(`{}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), domain.CodeResourceNotFound)
}

// TestGateway_HandleWebhookAdapterError verifies an adapter error is
// rendered as 500 INTERNAL_ERROR.
func TestGateway_HandleWebhookAdapterError(t *testing.T) {
	ch := &stubChannel{name: "qq", webhookErr: errors.New("boom")}
	g := NewGateway()
	g.RegisterChannel(ch)
	r := newGatewayTestRouter(t, g)

	req := httptest.NewRequest(http.MethodPost, "/bot/v1/channels/qq/webhook", bytes.NewBufferString(`{}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Contains(t, rec.Body.String(), domain.CodeInternalError)
}

// TestGateway_SendMessageDelegates verifies the send route binds the
// JSON body and delegates to the adapter.
func TestGateway_SendMessageDelegates(t *testing.T) {
	ch := &stubChannel{name: "feishu"}
	g := NewGateway()
	g.RegisterChannel(ch)
	r := newGatewayTestRouter(t, g)

	body := bytes.NewBufferString(`{"channel":"feishu","chat_id":"oc_abc","text":"hello"}`)
	req := httptest.NewRequest(http.MethodPost, "/bot/v1/messages", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, 1, ch.sendCalls)
	assert.Equal(t, "oc_abc", ch.lastChatID)
	assert.Equal(t, "hello", ch.lastText)
	var resp struct {
		Sent    bool   `json:"sent"`
		Channel string `json:"channel"`
		ChatID  string `json:"chat_id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.True(t, resp.Sent)
	assert.Equal(t, "feishu", resp.Channel)
	assert.Equal(t, "oc_abc", resp.ChatID)
}

// TestGateway_SendMessageValidation verifies the send route returns 422
// VALIDATION_FAILED when the body is not valid JSON.
func TestGateway_SendMessageValidation(t *testing.T) {
	ch := &stubChannel{name: "feishu"}
	g := NewGateway()
	g.RegisterChannel(ch)
	r := newGatewayTestRouter(t, g)

	req := httptest.NewRequest(http.MethodPost, "/bot/v1/messages", bytes.NewBufferString(`{bad json`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Contains(t, rec.Body.String(), domain.CodeValidationFailed)
	assert.Equal(t, 0, ch.sendCalls, "no dispatch on validation failure")
}

// TestGateway_SendMessageUnknownChannel verifies an unknown channel in
// the request body returns 404 RESOURCE_NOT_FOUND.
func TestGateway_SendMessageUnknownChannel(t *testing.T) {
	g := NewGateway()
	r := newGatewayTestRouter(t, g)

	body := bytes.NewBufferString(`{"channel":"nope","chat_id":"c1","text":"hi"}`)
	req := httptest.NewRequest(http.MethodPost, "/bot/v1/messages", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), domain.CodeResourceNotFound)
}

// TestGateway_ListSessionsEmpty verifies ListSessions returns an empty
// list when no session store is wired.
func TestGateway_ListSessionsEmpty(t *testing.T) {
	g := NewGateway()
	r := newGatewayTestRouter(t, g)

	req := httptest.NewRequest(http.MethodGet, "/bot/v1/sessions", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp struct {
		Sessions []any `json:"sessions"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.NotNil(t, resp.Sessions)
	assert.Empty(t, resp.Sessions)
}

// TestGateway_ListSessionsDelegates verifies ListSessions delegates to
// the session store when wired.
func TestGateway_ListSessionsDelegates(t *testing.T) {
	store := &stubSessionStore{list: []SessionSummary{
		{ID: "s1", State: "active", Channel: "qq"},
		{ID: "s2", State: "archived", Channel: "feishu"},
	}}
	g := NewGateway().WithSessions(store)
	r := newGatewayTestRouter(t, g)

	req := httptest.NewRequest(http.MethodGet, "/bot/v1/sessions", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp struct {
		Sessions []SessionSummary `json:"sessions"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Sessions, 2)
	assert.Equal(t, "s1", resp.Sessions[0].ID)
	assert.Equal(t, "qq", resp.Sessions[0].Channel)
}

// TestGateway_GetSessionDelegates verifies GetSession extracts :id and
// delegates to the session store.
func TestGateway_GetSessionDelegates(t *testing.T) {
	store := &stubSessionStore{get: &SessionSummary{ID: "sess-42", State: "active", Channel: "qq"}}
	g := NewGateway().WithSessions(store)
	r := newGatewayTestRouter(t, g)

	req := httptest.NewRequest(http.MethodGet, "/bot/v1/sessions/sess-42", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp SessionSummary
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "sess-42", resp.ID)
	assert.Equal(t, "active", resp.State)
	assert.Equal(t, "qq", resp.Channel)
}

// TestGateway_GetSessionNotFound verifies GetSession returns 404 when no
// session store is wired.
func TestGateway_GetSessionNotFound(t *testing.T) {
	g := NewGateway()
	r := newGatewayTestRouter(t, g)

	req := httptest.NewRequest(http.MethodGet, "/bot/v1/sessions/sess-1", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), domain.CodeResourceNotFound)
}

// TestGateway_ListChannels verifies ListChannels returns the registered
// channel names.
func TestGateway_ListChannels(t *testing.T) {
	g := NewGateway()
	g.RegisterChannel(&stubChannel{name: "qq"})
	g.RegisterChannel(&stubChannel{name: "feishu"})
	r := newGatewayTestRouter(t, g)

	req := httptest.NewRequest(http.MethodGet, "/bot/v1/channels", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp struct {
		Channels []string `json:"channels"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.ElementsMatch(t, []string{"qq", "feishu"}, resp.Channels)
}

// TestGateway_ListChannelsEmpty verifies ListChannels returns an empty
// list when no channels are registered.
func TestGateway_ListChannelsEmpty(t *testing.T) {
	g := NewGateway()
	r := newGatewayTestRouter(t, g)

	req := httptest.NewRequest(http.MethodGet, "/bot/v1/channels", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp struct {
		Channels []any `json:"channels"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Empty(t, resp.Channels)
}

// TestGateway_Health verifies Health returns 200 with the channel count.
func TestGateway_Health(t *testing.T) {
	g := NewGateway()
	g.RegisterChannel(&stubChannel{name: "qq"})
	r := newGatewayTestRouter(t, g)

	req := httptest.NewRequest(http.MethodGet, "/bot/v1/health", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp struct {
		Status   string `json:"status"`
		Channels int    `json:"channels"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "ok", resp.Status)
	assert.Equal(t, 1, resp.Channels)
}

// TestGateway_HealthEmpty verifies Health returns 200 with channels=0
// when no channels are registered (the gateway is healthy; the runtime
// is just unconfigured).
func TestGateway_HealthEmpty(t *testing.T) {
	g := NewGateway()
	r := newGatewayTestRouter(t, g)

	req := httptest.NewRequest(http.MethodGet, "/bot/v1/health", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp struct {
		Status   string `json:"status"`
		Channels int    `json:"channels"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "ok", resp.Status)
	assert.Equal(t, 0, resp.Channels)
}

// TestGateway_RegisterChannelNil verifies RegisterChannel with nil is a
// no-op (defensive — callers shouldn't pass nil but it shouldn't panic).
func TestGateway_RegisterChannelNil(t *testing.T) {
	g := NewGateway()
	g.RegisterChannel(nil) // must not panic
	r := newGatewayTestRouter(t, g)

	req := httptest.NewRequest(http.MethodGet, "/bot/v1/channels", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// TestGateway_SendMessageAdapterError verifies an adapter Send error is
// rendered as 500 INTERNAL_ERROR.
func TestGateway_SendMessageAdapterError(t *testing.T) {
	ch := &stubChannel{name: "qq", sendErr: errors.New("boom")}
	g := NewGateway()
	g.RegisterChannel(ch)
	r := newGatewayTestRouter(t, g)

	body := bytes.NewBufferString(`{"channel":"qq","chat_id":"c1","text":"hi"}`)
	req := httptest.NewRequest(http.MethodPost, "/bot/v1/messages", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Contains(t, rec.Body.String(), domain.CodeInternalError)
	assert.Equal(t, 1, ch.sendCalls)
}
