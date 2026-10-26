package web

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"
)

const (
	sessionIdle     = 30 * time.Minute
	sessionAbsolute = 12 * time.Hour
)

// Session is server-side state for one login. The cookie only carries a random ID.
// Sessions live in memory: restarting the web process logs everyone out, which is
// acceptable for a single-admin panel and means nothing sensitive is written to disk.
type Session struct {
	ID          string
	User        string
	CSRF        string
	Via         string // listener the session was created on
	AccessEmail string // Cloudflare Access identity at login (tunnel only)
	Created     time.Time
	LastSeen    time.Time
	Flash       string
}

type sessions struct {
	mu  sync.Mutex
	m   map[string]*Session
	now func() time.Time
}

func newSessions(now func() time.Time) *sessions {
	return &sessions{m: map[string]*Session{}, now: now}
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func (s *sessions) create(user, via, email string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	sess := &Session{ID: randomToken(), User: user, CSRF: randomToken(), Via: via, AccessEmail: email, Created: now, LastSeen: now}
	s.m[sess.ID] = sess
	return sess
}

// get returns a copy of the session if it exists and has not expired, and refreshes its
// idle timer.
func (s *sessions) get(id string) (Session, bool) { return s.lookup(id, true) }

// peek is get without refreshing the idle timer: a page updating itself in the
// background is not the operator doing something.
func (s *sessions) peek(id string) (Session, bool) { return s.lookup(id, false) }

func (s *sessions) lookup(id string, touch bool) (Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.m[id]
	if !ok {
		return Session{}, false
	}
	now := s.now()
	if now.Sub(sess.LastSeen) > sessionIdle || now.Sub(sess.Created) > sessionAbsolute {
		delete(s.m, id)
		return Session{}, false
	}
	if touch {
		sess.LastSeen = now
	}
	return *sess, true
}

func (s *sessions) delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, id)
}

func (s *sessions) setFlash(id, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.m[id]; ok {
		sess.Flash = msg
	}
}

// takeFlash returns and clears the pending flash message.
func (s *sessions) takeFlash(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.m[id]
	if !ok {
		return ""
	}
	f := sess.Flash
	sess.Flash = ""
	return f
}

// sweep drops expired sessions.
func (s *sessions) sweep() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for id, sess := range s.m {
		if now.Sub(sess.LastSeen) > sessionIdle || now.Sub(sess.Created) > sessionAbsolute {
			delete(s.m, id)
		}
	}
}

// limiter counts events per key in a fixed window.
type limiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	now    func() time.Time
	m      map[string]*bucket
}

type bucket struct {
	start time.Time
	n     int
}

func newLimiter(max int, window time.Duration, now func() time.Time) *limiter {
	return &limiter{max: max, window: window, now: now, m: map[string]*bucket{}}
}

// allow records an attempt for key and reports whether it is within the limit.
func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b, ok := l.m[key]
	if !ok || now.Sub(b.start) > l.window {
		// Keep the map bounded: drop stale buckets when adding new ones.
		if len(l.m) > 10000 {
			for k, v := range l.m {
				if now.Sub(v.start) > l.window {
					delete(l.m, k)
				}
			}
		}
		b = &bucket{start: now}
		l.m[key] = b
	}
	b.n++
	return b.n <= l.max
}
