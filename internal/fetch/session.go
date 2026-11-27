package fetch

import (
	"errors"
	"sync"
	"time"
)

// ErrSessionExpired marks a failure caused by the university proxy session
// having expired (EZproxy bounced us to its login page). Stages wrap it so
// the chain can mark the paper as waiting for a login instead of a dead end.
var ErrSessionExpired = errors.New("university session expired")

// SessionState is shared by the proxy and headless-browser stages: once one
// of them sees the login page, the others stop spending time on the proxy
// until a fresh cookie is applied.
type SessionState struct {
	mu      sync.Mutex
	expired bool
	msg     string
	at      time.Time
}

// Expired reports whether the proxy session is known to be invalid.
func (s *SessionState) Expired() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.expired
}

// Status returns the expiry flag with its message and time.
func (s *SessionState) Status() (expired bool, msg string, at time.Time) {
	if s == nil {
		return false, "", time.Time{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.expired, s.msg, s.at
}

// MarkExpired records that the proxy redirected to its login page.
func (s *SessionState) MarkExpired(msg string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.expired {
		s.expired, s.msg, s.at = true, msg, time.Now()
	}
}

// Reset clears the flag after a new cookie was applied or a test passed.
func (s *SessionState) Reset() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expired, s.msg, s.at = false, "", time.Time{}
}

// skipMsg is recorded on papers the proxy stages skip while the session is
// known to be expired.
const skipMsg = "university session expired; log in on the Library access page and this paper is retried automatically"
