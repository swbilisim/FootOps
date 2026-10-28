package web

import (
	"sync"
	"time"

	"github.com/swbilisim/FootOps/internal/agent"
	"github.com/swbilisim/FootOps/internal/sites"
)

// A new site is reviewed before it is created. Between the form and the "create"
// button the request waits here, in the web process's memory, so its secrets are not
// sent back to the browser in the review page. An entry belongs to one session, lives
// pendingTTL and is used once.
const (
	pendingTTL        = 15 * time.Minute
	pendingPerSession = 5
)

type pendingSite struct {
	session string
	created time.Time
	form    siteForm // as typed, to go back and correct it
	req     agent.CreateSiteRequest
	entries []sites.EnvEntry
	notes   []string // what the panel changed in the environment, and why
}

type pendingStore struct {
	mu  sync.Mutex
	m   map[string]*pendingSite
	now func() time.Time
}

func newPendingStore(now func() time.Time) *pendingStore {
	return &pendingStore{m: map[string]*pendingSite{}, now: now}
}

// put keeps a request and returns its ID; the oldest of the session's go first.
func (p *pendingStore) put(ps *pendingSite) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sweepLocked()
	ps.created = p.now()
	var oldest string
	n := 0
	for id, x := range p.m {
		if x.session != ps.session {
			continue
		}
		n++
		if oldest == "" || x.created.Before(p.m[oldest].created) {
			oldest = id
		}
	}
	if n >= pendingPerSession {
		delete(p.m, oldest)
	}
	id := randomToken()
	p.m[id] = ps
	return id
}

// get returns a request of this session that has not expired.
func (p *pendingStore) get(session, id string) (*pendingSite, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sweepLocked()
	ps, ok := p.m[id]
	if !ok || ps.session != session {
		return nil, false
	}
	return ps, true
}

// take is get, and the request is gone afterwards.
func (p *pendingStore) take(session, id string) (*pendingSite, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sweepLocked()
	ps, ok := p.m[id]
	if !ok || ps.session != session {
		return nil, false
	}
	delete(p.m, id)
	return ps, true
}

// dropSession forgets a session's requests (logout).
func (p *pendingStore) dropSession(session string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, x := range p.m {
		if x.session == session {
			delete(p.m, id)
		}
	}
}

// sweep drops expired requests; the web process calls it every minute.
func (p *pendingStore) sweep() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sweepLocked()
}

func (p *pendingStore) sweepLocked() {
	now := p.now()
	for id, x := range p.m {
		if now.Sub(x.created) > pendingTTL || now.Before(x.created) {
			delete(p.m, id)
		}
	}
}
