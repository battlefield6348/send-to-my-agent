package server

import (
	"context"
	"crypto/rand"
	"errors"
	"sync"
	"time"

	"github.com/battlefield6348/send-to-my-agent/internal/agent"
)

var errSessionNotFound = errors.New("session not found or expired")

// sessionManager keeps one live agent conversation per session and closes it after idleTimeout without asks.
type sessionManager struct {
	idleTimeout       time.Duration
	startConversation func(ctx context.Context) (agent.Conversation, error)

	mu       sync.Mutex
	sessions map[string]*session
}

type session struct {
	id string

	// mu serializes turns because an agent process answers one prompt at a time.
	mu           sync.Mutex
	conversation agent.Conversation
	lastUsed     time.Time
	closed       bool
	idleTimer    *time.Timer
}

func newSessionManager(idleTimeout time.Duration, startConversation func(ctx context.Context) (agent.Conversation, error)) *sessionManager {
	return &sessionManager{
		idleTimeout:       idleTimeout,
		startConversation: startConversation,
		sessions:          map[string]*session{},
	}
}

func (m *sessionManager) create(ctx context.Context) (*session, error) {
	conversation, err := m.startConversation(ctx)
	if err != nil {
		return nil, err
	}

	s := &session{
		id:           rand.Text(),
		conversation: conversation,
		lastUsed:     time.Now(),
	}
	s.idleTimer = time.AfterFunc(m.idleTimeout, func() { m.closeIfIdle(s) })

	m.mu.Lock()
	m.sessions[s.id] = s
	m.mu.Unlock()
	return s, nil
}

func (m *sessionManager) find(sessionID string) (*session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, exists := m.sessions[sessionID]
	if !exists {
		return nil, errSessionNotFound
	}
	return s, nil
}

// ask runs one turn. A failed turn closes the session because the agent's state is then unknown.
func (m *sessionManager) ask(ctx context.Context, s *session, prompt string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return "", errSessionNotFound
	}

	output, err := s.conversation.Ask(ctx, prompt)
	if err != nil {
		m.closeLocked(s)
		return "", err
	}
	s.lastUsed = time.Now()
	s.idleTimer.Reset(m.idleTimeout)
	return output, nil
}

func (m *sessionManager) closeIfIdle(s *session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// The timer may fire while a turn holds the lock; that turn refreshed lastUsed, so the session is not idle.
	isStillActive := time.Since(s.lastUsed) < m.idleTimeout
	if s.closed || isStillActive {
		return
	}
	m.closeLocked(s)
}

func (m *sessionManager) closeLocked(s *session) {
	s.closed = true
	s.idleTimer.Stop()

	m.mu.Lock()
	delete(m.sessions, s.id)
	m.mu.Unlock()

	_ = s.conversation.Close()
}
