package confirm

import (
	"sync"
	"time"
)

// Claims claim each id (a draft's, say) once per process, for a TTL, so a
// retry in this process doesn't act twice, or ask the person again about
// something already done. A claim is never released early, even when the
// action fails, so there is no rollback to get wrong.
type Claims struct {
	ttl    time.Duration
	mu     sync.Mutex
	claims map[string]time.Time
}

// NewClaims holds each claim for ttl.
func NewClaims(ttl time.Duration) *Claims {
	return &Claims{ttl: ttl, claims: map[string]time.Time{}}
}

// Claimed reports whether id was claimed within the TTL.
func (c *Claims) Claimed(id string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	at, ok := c.claims[id]
	return ok && now.Sub(at) < c.ttl
}

// Claim takes id, or reports it already taken.
func (c *Claims) Claim(id string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, at := range c.claims {
		if now.Sub(at) >= c.ttl {
			delete(c.claims, k)
		}
	}
	if _, taken := c.claims[id]; taken {
		return false
	}
	c.claims[id] = now
	// Forget it at the TTL even if no other claim comes to sweep it, so an
	// id is held for no longer than the TTL.
	time.AfterFunc(c.ttl, func() { c.forget(id, now) })
	return true
}

// forget drops a claim taken at at, leaving a later one in place.
func (c *Claims) forget(id string, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.claims[id].Equal(at) {
		delete(c.claims, id)
	}
}
