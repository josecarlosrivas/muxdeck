package server

import (
	"sync"
	"time"

	"github.com/josecarlosrivas/muxdeck/internal/ports"
	"github.com/josecarlosrivas/muxdeck/internal/tmux"
)

// portCache resolves the listening ports of every session at once.
//
// Not keyed by session, unlike repoCache: the walk reads the machine's
// process table once and attributes every listener it finds, so caching per
// session would pay that cost once per row. The same background-refresh rule
// applies for the same reason — the session list is polled every couple of
// seconds and must never wait on procfs or lsof, so a poll returns the last
// resolved answer and a stale one is recomputed behind it.
type portCache struct {
	mu sync.Mutex
	// m is replaced wholesale by a refresh, never mutated, so the map handed
	// to a request stays valid for as long as that request holds it.
	m          map[string][]int
	resolved   time.Time
	refreshing bool
}

const portTTL = 5 * time.Second

func newPortCache() *portCache { return &portCache{} }

func (c *portCache) state() map[string][]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.resolved) > portTTL && !c.refreshing {
		c.refreshing = true
		go c.refresh()
	}
	return c.m
}

func (c *portCache) refresh() {
	m := ports.Listening(tmux.PanePIDs())
	c.mu.Lock()
	c.m, c.resolved, c.refreshing = m, time.Now(), false
	c.mu.Unlock()
}
