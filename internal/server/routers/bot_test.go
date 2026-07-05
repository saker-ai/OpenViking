package routers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/domain"
)

// newBotTestRouter builds a gin engine with the bot router mounted at
// /bot/v1 and the in-package error middleware so *domain.AppError is
// rendered as a structured envelope (mirrors newTestRouter in
// resources_test.go without the /api/v1 group or identity middleware —
// the bot gateway sits outside the identity chain).
func newBotTestRouter(t *testing.T, deps *Deps) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(errorMiddlewareForTest())
	RegisterBot(r, deps)
	return r
}

// stubBotService is a BotService implementation that records every call
// and returns canned responses. It is safe for concurrent use because
// tests may run handlers in parallel within a single router.
type stubBotService struct {
	mu sync.Mutex

	webhookCalls      int
	sendCalls         int
	listSessionsCalls int
	getSessionCalls   int
	listChannelsCalls int
	healthCalls       int

	lastChannel   string
	lastChatID    string
	lastText      string
	lastSessionID string
	sendErr       *domain.AppError
}

func (s *stubBotService) HandleWebhook(c *gin.Context) {
	s.mu.Lock()
	s.webhookCalls++
	s.lastChannel = c.Param("channel")
	s.mu.Unlock()
	c.JSON(http.StatusOK, gin.H{
		"received": true,
		"channel":  c.Param("channel"),
	})
}

func (s *stubBotService) SendMessage(c *gin.Context) {
	s.mu.Lock()
	s.sendCalls++
	s.mu.Unlock()
	var req struct {
		Channel string `json:"channel"`
		ChatID  string `json:"chat_id"`
		Text    string `json:"text"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		abortWithError(c, domain.Wrap(domain.CodeValidationFailed, 422, err))
		return
	}
	if s.sendErr != nil {
		abortWithError(c, s.sendErr)
		return
	}
	s.mu.Lock()
	s.lastChannel = req.Channel
	s.lastChatID = req.ChatID
	s.lastText = req.Text
	s.mu.Unlock()
	c.JSON(http.StatusOK, gin.H{
		"sent":    true,
		"channel": req.Channel,
		"chat_id": req.ChatID,
	})
}

func (s *stubBotService) ListSessions(c *gin.Context) {
	s.mu.Lock()
	s.listSessionsCalls++
	s.mu.Unlock()
	c.JSON(http.StatusOK, gin.H{
		"sessions": []any{},
	})
}

func (s *stubBotService) GetSession(c *gin.Context) {
	s.mu.Lock()
	s.getSessionCalls++
	s.lastSessionID = c.Param("id")
	s.mu.Unlock()
	c.JSON(http.StatusOK, gin.H{
		"id":    c.Param("id"),
		"state": "active",
	})
}

func (s *stubBotService) ListChannels(c *gin.Context) {
	s.mu.Lock()
	s.listChannelsCalls++
	s.mu.Unlock()
	c.JSON(http.StatusOK, gin.H{
		"channels": []string{"qq", "feishu", "websocket"},
	})
}

func (s *stubBotService) Health(c *gin.Context) {
	s.mu.Lock()
	s.healthCalls++
	s.mu.Unlock()
	c.JSON(http.StatusOK, gin.H{
		"status": "ok",
	})
}

func (s *stubBotService) Chat(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"message":    "stub chat response",
		"session_id": c.Query("session_id"),
		"timestamp":  "2026-01-01T00:00:00Z",
	})
}

func (s *stubBotService) ChatStream(c *gin.Context) {
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	_, _ = c.Writer.Write([]byte("event: message\ndata: {\"message\":\"stub\"}\n\n"))
}

func (s *stubBotService) Feedback(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"received": true})
}

// TestBot_NilDepsReturns501 verifies that every /bot/v1/* route returns
// 501 UNSUPPORTED when Deps.Bot is nil (server boots without a bot).
func TestBot_NilDepsReturns501(t *testing.T) {
	r := newBotTestRouter(t, nil)

	cases := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/bot/v1/channels/qq/webhook"},
		{http.MethodPost, "/bot/v1/messages"},
		{http.MethodGet, "/bot/v1/sessions"},
		{http.MethodGet, "/bot/v1/sessions/sess-1"},
		{http.MethodGet, "/bot/v1/channels"},
		{http.MethodGet, "/bot/v1/health"},
		{http.MethodPost, "/bot/v1/chat"},
		{http.MethodPost, "/bot/v1/chat/stream"},
		{http.MethodPost, "/bot/v1/feedback"},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			assert.Equal(t, http.StatusNotImplemented, rec.Code, rec.Body.String())
			var body struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			assert.Equal(t, domain.CodeUnsupported, body.Error.Code)
		})
	}
}

// TestBot_NilBotFieldReturns501 verifies that a non-nil Deps with a nil
// Bot field still returns 501 (partial wiring doesn't crash).
func TestBot_NilBotFieldReturns501(t *testing.T) {
	r := newBotTestRouter(t, &Deps{})
	req := httptest.NewRequest(http.MethodGet, "/bot/v1/health", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
}

// TestBot_WebhookDelegates verifies the webhook route extracts the
// :channel parameter and delegates to BotService.HandleWebhook.
func TestBot_WebhookDelegates(t *testing.T) {
	svc := &stubBotService{}
	r := newBotTestRouter(t, &Deps{Bot: svc})

	body := bytes.NewBufferString(`{"event":"message","content":"hi"}`)
	req := httptest.NewRequest(http.MethodPost, "/bot/v1/channels/qq/webhook", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, 1, svc.webhookCalls)
	assert.Equal(t, "qq", svc.lastChannel)
	var resp struct {
		Received bool   `json:"received"`
		Channel  string `json:"channel"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.True(t, resp.Received)
	assert.Equal(t, "qq", resp.Channel)
}

// TestBot_SendMessageDelegates verifies the send-message route binds
// the JSON body and delegates to BotService.SendMessage.
func TestBot_SendMessageDelegates(t *testing.T) {
	svc := &stubBotService{}
	r := newBotTestRouter(t, &Deps{Bot: svc})

	body := bytes.NewBufferString(`{"channel":"feishu","chat_id":"oc_abc","text":"hello"}`)
	req := httptest.NewRequest(http.MethodPost, "/bot/v1/messages", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, 1, svc.sendCalls)
	assert.Equal(t, "feishu", svc.lastChannel)
	assert.Equal(t, "oc_abc", svc.lastChatID)
	assert.Equal(t, "hello", svc.lastText)
}

// TestBot_SendMessageValidation verifies the send-message route returns
// 422 VALIDATION_FAILED when the body is not valid JSON, and that no
// message dispatch occurs (lastText stays empty).
func TestBot_SendMessageValidation(t *testing.T) {
	svc := &stubBotService{}
	r := newBotTestRouter(t, &Deps{Bot: svc})

	req := httptest.NewRequest(http.MethodPost, "/bot/v1/messages", bytes.NewBufferString(`{bad json`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	// The handler IS invoked (it performs the validation), but no
	// dispatch should occur — lastText stays empty.
	assert.Equal(t, "", svc.lastText, "no message dispatch on validation failure")
}

// TestBot_ListSessionsDelegates verifies the sessions list route
// delegates to BotService.ListSessions.
func TestBot_ListSessionsDelegates(t *testing.T) {
	svc := &stubBotService{}
	r := newBotTestRouter(t, &Deps{Bot: svc})

	req := httptest.NewRequest(http.MethodGet, "/bot/v1/sessions", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, 1, svc.listSessionsCalls)
	var resp struct {
		Sessions []any `json:"sessions"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.NotNil(t, resp.Sessions)
}

// TestBot_GetSessionDelegates verifies the session detail route
// extracts :id and delegates to BotService.GetSession.
func TestBot_GetSessionDelegates(t *testing.T) {
	svc := &stubBotService{}
	r := newBotTestRouter(t, &Deps{Bot: svc})

	req := httptest.NewRequest(http.MethodGet, "/bot/v1/sessions/sess-42", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, 1, svc.getSessionCalls)
	assert.Equal(t, "sess-42", svc.lastSessionID)
	var resp struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "sess-42", resp.ID)
	assert.Equal(t, "active", resp.State)
}

// TestBot_ListChannelsDelegates verifies the channels list route
// delegates to BotService.ListChannels.
func TestBot_ListChannelsDelegates(t *testing.T) {
	svc := &stubBotService{}
	r := newBotTestRouter(t, &Deps{Bot: svc})

	req := httptest.NewRequest(http.MethodGet, "/bot/v1/channels", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, 1, svc.listChannelsCalls)
	var resp struct {
		Channels []string `json:"channels"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, []string{"qq", "feishu", "websocket"}, resp.Channels)
}

// TestBot_HealthDelegates verifies the health route delegates to
// BotService.Health.
func TestBot_HealthDelegates(t *testing.T) {
	svc := &stubBotService{}
	r := newBotTestRouter(t, &Deps{Bot: svc})

	req := httptest.NewRequest(http.MethodGet, "/bot/v1/health", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, 1, svc.healthCalls)
	var resp struct {
		Status string `json:"status"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "ok", resp.Status)
}

// TestBot_404UnknownRoute verifies that an unknown /bot/v1/* path
// returns 404 (gin NoRoute) rather than 501.
func TestBot_404UnknownRoute(t *testing.T) {
	r := newBotTestRouter(t, &Deps{})
	req := httptest.NewRequest(http.MethodGet, "/bot/v1/unknown", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestBot_ChatDelegates verifies POST /bot/v1/chat delegates to
// BotService.Chat and returns the stub response envelope.
func TestBot_ChatDelegates(t *testing.T) {
	svc := &stubBotService{}
	r := newBotTestRouter(t, &Deps{Bot: svc})
	body := bytes.NewBufferString(`{"message":"hi","session_id":"s1"}`)
	req := httptest.NewRequest(http.MethodPost, "/bot/v1/chat", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp struct {
		Message   string `json:"message"`
		SessionID string `json:"session_id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "stub chat response", resp.Message)
}

// TestBot_ChatStreamDelegates verifies POST /bot/v1/chat/stream delegates
// to BotService.ChatStream and emits an SSE frame.
func TestBot_ChatStreamDelegates(t *testing.T) {
	svc := &stubBotService{}
	r := newBotTestRouter(t, &Deps{Bot: svc})
	body := bytes.NewBufferString(`{"message":"hi","stream":true}`)
	req := httptest.NewRequest(http.MethodPost, "/bot/v1/chat/stream", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Header().Get("Content-Type"), "text/event-stream")
	assert.Contains(t, rec.Body.String(), "event: message")
}

// TestBot_FeedbackDelegates verifies POST /bot/v1/feedback delegates to
// BotService.Feedback.
func TestBot_FeedbackDelegates(t *testing.T) {
	svc := &stubBotService{}
	r := newBotTestRouter(t, &Deps{Bot: svc})
	body := bytes.NewBufferString(`{"rating":"up"}`)
	req := httptest.NewRequest(http.MethodPost, "/bot/v1/feedback", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp struct {
		Received bool `json:"received"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.True(t, resp.Received)
}
