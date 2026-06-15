package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"sync"
	"time"
)

const sessionTTL = 24 * time.Hour

type Manager struct {
	mu       sync.Mutex
	password string
	sessions map[string]time.Time // token -> expiry
}

func NewManager(password string) *Manager {
	return &Manager{
		password: password,
		sessions: make(map[string]time.Time),
	}
}

// CheckPassword returns true if the submitted password matches.
func (m *Manager) CheckPassword(submitted string) bool {
	a := []byte(m.password)
	b := []byte(submitted)
	return subtle.ConstantTimeCompare(a, b) == 1
}

// CreateSession mints a random session token and stores it.
func (m *Manager) CreateSession() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := hex.EncodeToString(buf)
	m.mu.Lock()
	m.sessions[token] = time.Now().Add(sessionTTL)
	m.mu.Unlock()
	return token, nil
}

// ValidateSession returns true and refreshes TTL if the token is valid.
func (m *Manager) ValidateSession(token string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	exp, ok := m.sessions[token]
	if !ok || time.Now().After(exp) {
		delete(m.sessions, token)
		return false
	}
	m.sessions[token] = time.Now().Add(sessionTTL)
	return true
}

// DeleteSession removes a session token.
func (m *Manager) DeleteSession(token string) {
	m.mu.Lock()
	delete(m.sessions, token)
	m.mu.Unlock()
}
