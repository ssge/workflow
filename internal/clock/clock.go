// Package clock defines the UTC time source shared by engine and store code.
package clock

import (
	"sync"
	"time"
)

// Clock returns instants in UTC. Persist times using Time.UnixMilli.
type Clock interface {
	Now() time.Time
}

// Real reads the system clock.
type Real struct{}

func (Real) Now() time.Time { return time.Now().UTC() }

// Fake is a concurrency-safe, explicitly controlled clock for tests.
type Fake struct {
	mu  sync.RWMutex
	now time.Time
}

func NewFake(now time.Time) *Fake { return &Fake{now: now.UTC()} }

func (c *Fake) Now() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.now
}

func (c *Fake) Set(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now.UTC()
}

func (c *Fake) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}
