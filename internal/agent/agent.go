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
	Progress  *Progress `json:"progress,omitempty"`
	Chips     []Chip    `json:"chips,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Progress is how far along whatever the agent is doing is, as a fraction of
// one, with a label naming the thing. A pointer because absent and zero are
// different answers: absent leaves the previous progress alone, and an empty
// object is how an agent says it is no longer working through anything.
type Progress struct {
	Value float64 `json:"value"`
	Label string  `json:"label,omitempty"`
}

// Chip is one extra fact an agent wants on its row — tests passing, context
// left, the queue behind it. muxdeck renders chips; it never interprets them,
// so what a chip means is entirely between the agent and the person reading
// the sidebar.
type Chip struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	Icon  string `json:"icon,omitempty"`
	Color string `json:"color,omitempty"`
}

func ValidState(s string) bool {
	return s == StateWorking || s == StateWaiting || s == StateIdle
}

// What one status may occupy of a sidebar row. A row is a few hundred pixels
// on a phone, and the reporter is an unprivileged process pushing display
// data, so the bounds are this package's rather than the reporter's.
const (
	maxNote     = 200
	maxLabel    = 60
	maxChips    = 6
	maxChipText = 24
)

// Icon and color are closed sets because a chip is drawn into muxdeck's own
// sidebar: an unknown icon name would render nothing, and an arbitrary CSS
// color would fight whatever theme it lands in — a status push should not get
// a say in how the UI looks. An unrecognised value is dropped rather than
// rejected, since a cosmetic field must not cost an agent its whole status.
var (
	chipIcons = map[string]bool{
		"check": true, "alert": true, "clock": true,
		"flask": true, "coins": true, "file": true,
		"branch": true, "plug": true,
	}
	chipColors = map[string]bool{
		"accent": true, "warn": true, "danger": true, "dim": true,
	}
)

// Normalize clamps a reported status to what the sidebar can render. Chips
// that survive nothing — no value left after trimming — are dropped, so a
// status whose chips are all unrenderable arrives as an explicit empty list
// and clears whatever was showing.
func (s *Status) Normalize() {
	s.Note = truncate(s.Note, maxNote)
	if s.Progress != nil {
		s.Progress.Value = min(max(s.Progress.Value, 0), 1)
		s.Progress.Label = truncate(s.Progress.Label, maxLabel)
	}
	if len(s.Chips) > maxChips {
		s.Chips = s.Chips[:maxChips]
	}
	kept := s.Chips[:0]
	for _, c := range s.Chips {
		c.Key = truncate(c.Key, maxChipText)
		c.Value = truncate(c.Value, maxChipText)
		if !chipIcons[c.Icon] {
			c.Icon = ""
		}
		if !chipColors[c.Color] {
			c.Color = ""
		}
		if c.Value == "" {
			continue
		}
		kept = append(kept, c)
	}
	s.Chips = kept
}

// truncate cuts to at most max runes. Byte slicing would be enough for the
// length bound and would also split a multi-byte code point down the middle,
// which renders as a replacement glyph in the one place the text is shown.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
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
