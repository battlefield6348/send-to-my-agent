package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/battlefield6348/send-to-my-agent/internal/agent"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testToken = "secret"

// fakeConversation answers "<turn number>:<prompt>" so tests can see whether turns share one conversation.
type fakeConversation struct {
	mu          sync.Mutex
	turns       int
	closed      bool
	inFlight    atomic.Int32
	overlapSeen atomic.Bool
}

func (c *fakeConversation) Ask(ctx context.Context, prompt string) (string, error) {
	if c.inFlight.Add(1) > 1 {
		c.overlapSeen.Store(true)
	}
	defer c.inFlight.Add(-1)

	switch prompt {
	case "fail":
		return "", errors.New("boom")
	case "hang":
		<-ctx.Done()
		return "", ctx.Err()
	case "slow":
		time.Sleep(20 * time.Millisecond)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.turns++
	return fmt.Sprintf("%d:%s", c.turns, prompt), nil
}

func (c *fakeConversation) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

func (c *fakeConversation) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

type testHarness struct {
	server        *server
	conversations []*fakeConversation
	startErr      error
	mu            sync.Mutex
}

func newHarness(idleTimeout time.Duration) *testHarness {
	h := &testHarness{}
	h.server = &server{
		token:   testToken,
		timeout: 200 * time.Millisecond,
		sessions: newSessionManager(idleTimeout, func(ctx context.Context) (agent.Conversation, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			if h.startErr != nil {
				return nil, h.startErr
			}
			conversation := &fakeConversation{}
			h.conversations = append(h.conversations, conversation)
			return conversation, nil
		}),
	}
	return h
}

func (h *testHarness) post(t *testing.T, token string, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/ask", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.server.routes().ServeHTTP(rec, req)
	return rec
}

func (h *testHarness) ask(t *testing.T, sessionID string, prompt string) (askResponse, int) {
	t.Helper()
	body, err := json.Marshal(askRequest{SessionID: sessionID, Prompt: prompt})
	require.NoError(t, err)
	rec := h.post(t, testToken, string(body))

	var resp askResponse
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	}
	return resp, rec.Code
}

func TestAskWithoutSessionStartsNewConversation(t *testing.T) {
	h := newHarness(time.Minute)

	first, firstStatus := h.ask(t, "", "hello")
	second, secondStatus := h.ask(t, "", "hello")

	require.Equal(t, http.StatusOK, firstStatus)
	require.Equal(t, http.StatusOK, secondStatus)
	assert.Equal(t, "1:hello", first.Output)
	assert.Equal(t, "1:hello", second.Output)
	assert.NotEmpty(t, first.SessionID)
	assert.NotEqual(t, first.SessionID, second.SessionID)
}

func TestAskWithSessionContinuesSameConversation(t *testing.T) {
	h := newHarness(time.Minute)

	first, _ := h.ask(t, "", "remember 7")
	second, status := h.ask(t, first.SessionID, "what number?")

	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, "2:what number?", second.Output)
	assert.Equal(t, first.SessionID, second.SessionID)
	assert.Len(t, h.conversations, 1)
}

func TestAskWithUnknownSessionReturnsNotFound(t *testing.T) {
	h := newHarness(time.Minute)

	_, status := h.ask(t, "does-not-exist", "hi")

	assert.Equal(t, http.StatusNotFound, status)
}

func TestIdleSessionIsClosedAndForgotten(t *testing.T) {
	h := newHarness(50 * time.Millisecond)

	first, _ := h.ask(t, "", "hi")
	require.Eventually(t, h.conversations[0].isClosed, time.Second, 10*time.Millisecond)

	_, status := h.ask(t, first.SessionID, "still there?")
	assert.Equal(t, http.StatusNotFound, status)
}

func TestActiveSessionStaysOpenPastIdleTimeout(t *testing.T) {
	h := newHarness(100 * time.Millisecond)

	first, _ := h.ask(t, "", "hi")
	for range 5 {
		time.Sleep(40 * time.Millisecond)
		_, status := h.ask(t, first.SessionID, "keep alive")
		require.Equal(t, http.StatusOK, status)
	}

	assert.False(t, h.conversations[0].isClosed())
}

func TestConcurrentAsksOnOneSessionRunOneAtATime(t *testing.T) {
	h := newHarness(time.Minute)
	first, _ := h.ask(t, "", "hi")

	var wg sync.WaitGroup
	for range 5 {
		wg.Go(func() {
			_, status := h.ask(t, first.SessionID, "slow")
			assert.Equal(t, http.StatusOK, status)
		})
	}
	wg.Wait()

	assert.False(t, h.conversations[0].overlapSeen.Load())
}

func TestAgentFailureClosesSession(t *testing.T) {
	h := newHarness(time.Minute)
	first, _ := h.ask(t, "", "hi")

	rec := h.post(t, testToken, `{"session_id":"`+first.SessionID+`","prompt":"fail"}`)
	assert.Equal(t, http.StatusBadGateway, rec.Code)
	assert.Contains(t, rec.Body.String(), "boom")
	assert.True(t, h.conversations[0].isClosed())

	_, status := h.ask(t, first.SessionID, "hi")
	assert.Equal(t, http.StatusNotFound, status)
}

func TestAgentStartFailureReturnsBadGateway(t *testing.T) {
	h := newHarness(time.Minute)
	h.startErr = errors.New("cli not logged in")

	rec := h.post(t, testToken, `{"prompt":"hi"}`)

	assert.Equal(t, http.StatusBadGateway, rec.Code)
	assert.Contains(t, rec.Body.String(), "cli not logged in")
}

func TestAskTimesOutAndClosesSession(t *testing.T) {
	h := newHarness(time.Minute)

	rec := h.post(t, testToken, `{"prompt":"hang"}`)

	assert.Equal(t, http.StatusGatewayTimeout, rec.Code)
	assert.True(t, h.conversations[0].isClosed())
}

func TestAskRejectsBadToken(t *testing.T) {
	h := newHarness(time.Minute)

	assert.Equal(t, http.StatusUnauthorized, h.post(t, "", `{"prompt":"hi"}`).Code)
	assert.Equal(t, http.StatusUnauthorized, h.post(t, "wrong", `{"prompt":"hi"}`).Code)
}

func TestAskRejectsInvalidInput(t *testing.T) {
	invalidBodies := map[string]string{
		"malformed json": `{"prompt":`,
		"empty prompt":   `{"prompt":"   "}`,
	}

	for name, body := range invalidBodies {
		t.Run(name, func(t *testing.T) {
			rec := newHarness(time.Minute).post(t, testToken, body)
			assert.Equal(t, http.StatusBadRequest, rec.Code)
		})
	}
}

func TestAskRejectsGet(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/ask", nil)
	rec := httptest.NewRecorder()
	newHarness(time.Minute).server.routes().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}
