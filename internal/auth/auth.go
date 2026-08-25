package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
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
	csrfKey  [32]byte             // HMAC key binding CSRF tokens to sessions
	limiter  loginLimiter
}

func NewManager(password string) *Manager {
	m := &Manager{
		password: password,
		sessions: make(map[string]time.Time),
	}
	if _, err := rand.Read(m.csrfKey[:]); err != nil {
		panic("auth: crypto/rand unavailable: " + err.Error())
	}
	m.limiter.failures = make(map[string]*failState)
	return m
}

// CheckPassword returns true if the submitted password matches.
func (m *Manager) CheckPassword(submitted string) bool {
	a := []byte(m.password)
	b := []byte(submitted)
	return subtle.ConstantTimeCompare(a, b) == 1
}

// RandomToken returns a cryptographically-random session/CSRF token.
func RandomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// CSRFForSession derives the expected CSRF token for a session. Because the
// token is HMAC(sessionToken), an attacker who can plant a cookie but does
// not know the session token cannot forge a matching pair.
func (m *Manager) CSRFForSession(sessionToken string) string {
	mac := hmac.New(sha256.New, m.csrfKey[:])
	mac.Write([]byte(sessionToken))
	return hex.EncodeToString(mac.Sum(nil))
}

// CreateSession mints a random session token and stores it.
func (m *Manager) CreateSession() (string, error) {
	token, err := RandomToken()
	if err != nil {
		return "", err
	}
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

// SweepExpired removes expired sessions. Call periodically — otherwise the
// map grows with every abandoned session (entries were only evicted when
// re-presented).
func (m *Manager) SweepExpired() {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	for token, exp := range m.sessions {
		if now.After(exp) {
			delete(m.sessions, token)
		}
	}
}

// StartSweeper runs SweepExpired on an interval until ctx is cancelled.
func (m *Manager) StartSweeper(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.SweepExpired()
			}
		}
	}()
}

// ---- Login brute-force protection ----

const (
	loginMaxFailures  = 5                // failures within the window before lockout
	loginLockBase     = 30 * time.Second // initial lockout; doubles per failure past the threshold
	loginLockMax      = 15 * time.Minute
	loginWindowExpiry = 30 * time.Minute // failure counters idle this long are reset
)

type failState struct {
	count    int
	lockedTo time.Time
	last     time.Time
}

type loginLimiter struct {
	mu       sync.Mutex
	failures map[string]*failState
}

// LoginLockedOut reports whether the given client key is locked out, and for
// how much longer.
func (m *Manager) LoginLockedOut(key string) (time.Duration, bool) {
	m.limiter.mu.Lock()
	defer m.limiter.mu.Unlock()
	f, ok := m.limiter.failures[key]
	if !ok {
		return 0, false
	}
	if time.Now().Before(f.lockedTo) {
		return time.Until(f.lockedTo).Round(time.Second), true
	}
	return 0, false
}

// RecordLoginFailure increments the failure counter for key and applies an
// exponentially-growing lockout once the threshold is passed.
func (m *Manager) RecordLoginFailure(key string) {
	m.limiter.mu.Lock()
	defer m.limiter.mu.Unlock()
	f, ok := m.limiter.failures[key]
	if !ok || time.Since(f.last) > loginWindowExpiry {
		f = &failState{}
		m.limiter.failures[key] = f
	}
	f.count++
	f.last = time.Now()
	if f.count > loginMaxFailures {
		d := loginLockBase << min(f.count-loginMaxFailures-1, 5)
		if d > loginLockMax {
			d = loginLockMax
		}
		f.lockedTo = time.Now().Add(d)
	}
}

// RecordLoginSuccess clears the failure counter for key.
func (m *Manager) RecordLoginSuccess(key string) {
	m.limiter.mu.Lock()
	delete(m.limiter.failures, key)
	m.limiter.mu.Unlock()
}
