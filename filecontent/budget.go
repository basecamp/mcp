package filecontent

import (
	"fmt"
	"sync/atomic"

	"golang.org/x/sync/semaphore"
)

// Budget bounds the fetched file bytes a process holds at once across every
// call it serves. A size cap bounds one call, and a rate limiter a token's
// request count, but neither bounds a burst of parallel calls, each holding
// up to the cap. A product may share one budget with its other file reads
// (basecamp-mcp-server's download_file does).
type Budget interface {
	// Size is the whole budget, in bytes.
	Size() int64
	// Reserve takes n bytes, reporting false, and taking nothing, when they
	// are not free. The caller releases them with the returned func, once.
	Reserve(n int64) (release func(), ok bool)
}

// MemoryBudget is a Budget of a fixed number of bytes, refusing rather than
// queueing a reservation it can't fit.
type MemoryBudget struct {
	sem  *semaphore.Weighted
	size int64
	// held is the bytes reserved now, for tests and diagnostics; sem is
	// what bounds them.
	held atomic.Int64
}

// NewMemoryBudget returns a budget of size bytes, which must be positive.
func NewMemoryBudget(size int64) (*MemoryBudget, error) {
	if size <= 0 {
		return nil, fmt.Errorf("invalid file budget %d: must be positive", size)
	}
	return &MemoryBudget{sem: semaphore.NewWeighted(size), size: size}, nil
}

// Size is the whole budget.
func (b *MemoryBudget) Size() int64 { return b.size }

// Held is the bytes reserved now.
func (b *MemoryBudget) Held() int64 { return b.held.Load() }

// Reserve takes n bytes of the budget, reporting false when they are not
// free.
func (b *MemoryBudget) Reserve(n int64) (release func(), ok bool) {
	if !b.sem.TryAcquire(n) {
		return nil, false
	}
	b.held.Add(n)
	var once atomic.Bool
	return func() {
		if once.CompareAndSwap(false, true) {
			b.held.Add(-n)
			b.sem.Release(n)
		}
	}, true
}
