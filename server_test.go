package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testToken = "secret"

// Fake agent commands stand in for the real CLIs so tests never call an AI service.
const (
	echoAgent  = "cat"
	failAgent  = "echo boom >&2; exit 1"
	sleepAgent = "sleep 5"
)

func newTestServer(agentCommand ...string) *server {
	return &server{
		token:        testToken,
		agentCommand: agentCommand,
		timeout:      200 * time.Millisecond,
	}
}

func newEchoServer() *server {
	return newTestServer(echoAgent)
}

func postAsk(t *testing.T, s *server, token string, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/ask", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	return rec
}

func TestAskReturnsAgentOutput(t *testing.T) {
	rec := postAsk(t, newEchoServer(), testToken, `{"prompt":"hello agent"}`)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp askResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "hello agent", resp.Output)
}

func TestAskPromptStartingWithDashIsNotTreatedAsFlag(t *testing.T) {
	rec := postAsk(t, newEchoServer(), testToken, `{"prompt":"--help"}`)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"output":"--help"`)
}

func TestAskRejectsBadToken(t *testing.T) {
	missingToken := postAsk(t, newEchoServer(), "", `{"prompt":"hi"}`)
	wrongToken := postAsk(t, newEchoServer(), "wrong", `{"prompt":"hi"}`)

	assert.Equal(t, http.StatusUnauthorized, missingToken.Code)
	assert.Equal(t, http.StatusUnauthorized, wrongToken.Code)
}

func TestAskRejectsInvalidInput(t *testing.T) {
	invalidBodies := map[string]string{
		"malformed json": `{"prompt":`,
		"empty prompt":   `{"prompt":"   "}`,
	}

	for name, body := range invalidBodies {
		t.Run(name, func(t *testing.T) {
			rec := postAsk(t, newEchoServer(), testToken, body)
			assert.Equal(t, http.StatusBadRequest, rec.Code)
		})
	}
}

func TestAskReportsAgentFailure(t *testing.T) {
	rec := postAsk(t, newTestServer("sh", "-c", failAgent), testToken, `{"prompt":"hi"}`)

	assert.Equal(t, http.StatusBadGateway, rec.Code)
	assert.Contains(t, rec.Body.String(), "boom")
}

func TestAskTimesOut(t *testing.T) {
	start := time.Now()
	rec := postAsk(t, newTestServer("sh", "-c", sleepAgent), testToken, `{"prompt":"hi"}`)

	assert.Equal(t, http.StatusGatewayTimeout, rec.Code)
	assert.Less(t, time.Since(start), 3*time.Second)
}

func TestAskRejectsGet(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/ask", nil)
	rec := httptest.NewRecorder()
	newEchoServer().routes().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestAgentCommandForName(t *testing.T) {
	command, err := agentCommandFor("claude")
	require.NoError(t, err)
	assert.Equal(t, []string{"claude", "-p"}, command)

	_, err = agentCommandFor("")
	assert.Error(t, err)
	_, err = agentCommandFor("nope")
	assert.Error(t, err)
}
