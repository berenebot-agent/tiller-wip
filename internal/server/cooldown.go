package server

import (
	"sync"
	"time"
)

type cooldownStore struct {
	mu    sync.Mutex
	until map[string]time.Time
}

func newCooldownStore() *cooldownStore {
	return &cooldownStore{until: map[string]time.Time{}}
}

func (c *cooldownStore) cooled(id string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.until[id]
	if !ok {
		return false
	}
	return now.Before(t)
}

func (c *cooldownStore) set(id string, until time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.until[id] = until
}

func (c *cooldownStore) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.until = map[string]time.Time{}
}
