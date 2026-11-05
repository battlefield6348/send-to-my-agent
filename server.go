package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

const maxRequestBytes = 1 << 20

// Each agent reads the prompt from stdin, so prompt text can never be parsed as a CLI flag.
var defaultAgentCommands = map[string][]string{
	"claude": {"claude", "-p"},
	"codex":  {"codex", "exec", "--skip-git-repo-check", "-"},
	// gemini treats an empty -p as "no prompt"; a blank one enables headless mode and is appended to stdin.
	"gemini": {"gemini", "--skip-trust", "-p", " "},
}

type server struct {
	token         string
	agentCommands map[string][]string
	timeout       time.Duration
}

type askRequest struct {
	Agent  string `json:"agent"`
	Prompt string `json:"prompt"`
}

type askResponse struct {
	Agent  string `json:"agent"`
	Output string `json:"output"`
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
	command, isKnownAgent := s.agentCommands[req.Agent]
	if !isKnownAgent {
		writeError(w, http.StatusBadRequest, "unknown agent: "+req.Agent)
		return
	}
	if strings.TrimSpace(req.Prompt) == "" {
		writeError(w, http.StatusBadRequest, "prompt is required")
		return
	}

	// Deriving from the request context also stops the agent when the client disconnects.
	ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
	defer cancel()

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Stdin = strings.NewReader(req.Prompt)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// Agent CLIs may spawn children that keep the pipes open after the parent is killed.
	cmd.WaitDelay = 2 * time.Second

	err = cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		writeError(w, http.StatusGatewayTimeout, "agent timed out after "+s.timeout.String())
		return
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, "agent failed: "+err.Error()+": "+strings.TrimSpace(stderr.String()))
		return
	}

	writeJSON(w, http.StatusOK, askResponse{
		Agent:  req.Agent,
		Output: strings.TrimSpace(stdout.String()),
	})
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
