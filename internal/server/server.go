package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/battlefield6348/send-to-my-agent/internal/agent"
)

const maxRequestBytes = 1 << 20

type server struct {
	token    string
	sessions *sessionManager
	timeout  time.Duration
}

type askRequest struct {
	SessionID string `json:"session_id,omitempty"`
	Prompt    string `json:"prompt"`
}

type askResponse struct {
	SessionID string `json:"session_id"`
	Output    string `json:"output"`
}

// New returns the HTTP API; each new session starts its own conversation with startConversation.
func New(token string, idleTimeout, askTimeout time.Duration, startConversation func(ctx context.Context) (agent.Conversation, error)) http.Handler {
	s := &server{
		token:    token,
		sessions: newSessionManager(idleTimeout, startConversation),
		timeout:  askTimeout,
	}
	return s.routes()
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/ask", s.handleAsk)
	return mux
}

func (s *server) handleAsk(w http.ResponseWriter, r *http.Request) {
	if !s.isAuthorized(r) {
		writeError(w, http.StatusUnauthorized, "invalid or missing bearer token")
		return
	}

	var req askRequest
	err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBytes)).Decode(&req)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Prompt) == "" {
		writeError(w, http.StatusBadRequest, "prompt is required")
		return
	}

	// Deriving from the request context also stops the turn when the client disconnects.
	ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
	defer cancel()

	var agentSession *session
	if req.SessionID == "" {
		agentSession, err = s.sessions.create(ctx)
	} else {
		agentSession, err = s.sessions.find(req.SessionID)
	}
	if err != nil {
		writeAgentError(w, ctx, err)
		return
	}

	output, err := s.sessions.ask(ctx, agentSession, req.Prompt)
	if err != nil {
		writeAgentError(w, ctx, err)
		return
	}
	writeJSON(w, http.StatusOK, askResponse{SessionID: agentSession.id, Output: strings.TrimSpace(output)})
}

func writeAgentError(w http.ResponseWriter, ctx context.Context, err error) {
	switch {
	case errors.Is(err, errSessionNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		writeError(w, http.StatusGatewayTimeout, "agent timed out; the session was closed")
	default:
		writeError(w, http.StatusBadGateway, "agent failed: "+err.Error())
	}
}

func (s *server) isAuthorized(r *http.Request) bool {
	givenToken, hasBearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !hasBearer {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(givenToken), []byte(s.token)) == 1
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
