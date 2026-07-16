// Package agent tracks self-reported status from AI agents running inside
// tmux sessions. Agents push via POST /api/agent/status; muxdeck only
// displays what it is told — it never parses terminal content.
package agent

import (
	"errors"
	"sync"
	"time"
)

// States an agent can report. "waiting" means the agent is blocked on
// human input — the one clients are expected to notify on.
const (
	StateWorking = "working"
	StateWaiting = "waiting"
	StateIdle    = "idle"
)

var ErrBadState = errors.New(`state must be "working", "waiting" or "idle"`)

type Status struct {
	Agent     string    `json:"agent"`
	State     string    `json:"state"`
	Model     string    `json:"model,omitempty"`
	CostUSD   float64   `json:"cost_usd,omitempty"`
	Note      string    `json:"note,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

func ValidState(s string) bool {
	return s == StateWorking || s == StateWaiting || s == StateIdle
}

type Store struct {
	mu sync.Mutex
	m  map[string]Status
}

func NewStore() *Store { return &Store{m: map[string]Status{}} }

func (st *Store) Set(session string, s Status) {
	s.UpdatedAt = time.Now()
	st.mu.Lock()
	st.m[session] = s
	st.mu.Unlock()
}

func (st *Store) Get(session string) (Status, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	s, ok := st.m[session]
	return s, ok
}

// Prune drops statuses for sessions that no longer exist, so a killed
// session doesn't resurrect a stale badge if the name is reused.
func (st *Store) Prune(live []string) {
	alive := make(map[string]bool, len(live))
	for _, n := range live {
		alive[n] = true
	}
	st.mu.Lock()
	for n := range st.m {
		if !alive[n] {
			delete(st.m, n)
		}
	}
	st.mu.Unlock()
}
