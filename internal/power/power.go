// Package power keeps a Mac awake while someone is actually looking at it.
// Terminal and mush-stream viewers hold short presence leases; while the
// preference is on and a lease is live the daemon owns one sleep assertion
// through a platform backend, and releases it a grace period after the
// last viewer goes. Transport keepalives, session polling, relay traffic
// and detached sessions earn nothing — only a foreground viewer that keeps
// renewing does.
package power

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	// LeaseTTL is how long a presence renewal counts for; a viewer whose
	// client died without a close frame loses its claim after this.
	LeaseTTL = 60 * time.Second
	// ReleaseGrace holds the assertion briefly after the last lease so a
	// reconnecting viewer does not bounce it.
	ReleaseGrace = 15 * time.Second
	// maxFailures bounds backend retries: past this the manager stops
	// trying until the preference is toggled or viewers come back from zero.
	maxFailures = 5
	maxBackoff  = 60 * time.Second
	psCacheTTL  = 10 * time.Second
)

// Config is the persisted preference, one file per daemon.
type Config struct {
	KeepAwakeWhileViewing bool `json:"keep_awake_while_viewing"`
}

// Status is the API-facing view. State is one of off, waiting,
// keeping_awake, on_battery, unsupported, unavailable.
type Status struct {
	Supported bool   `json:"supported"`
	Enabled   bool   `json:"enabled"`
	State     string `json:"state"`
	Viewers   int    `json:"viewers"`
	Asserting bool   `json:"asserting"`
	Backend   string `json:"backend,omitempty"`
	Power     string `json:"power,omitempty"` // "ac" | "battery" | ""
	Error     string `json:"error,omitempty"`
}

// Backend owns the platform's sleep assertion.
type Backend interface {
	Name() string
	// Start acquires the assertion; Done closes when it ends on its own.
	Start() (Assertion, error)
	// PowerSource reports "ac", "battery" or "" when unknown.
	PowerSource() string
}

// Assertion is one held sleep inhibition.
type Assertion interface {
	Done() <-chan struct{}
	Stop()
}

var ErrUnsupported = errors.New("keep awake is only supported on macOS")

// DefaultConfigPath is power.json beside the daemon's other config files.
func DefaultConfigPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = "."
	}
	return filepath.Join(dir, "muxdeck", "power.json")
}

type Manager struct {
	mu      sync.Mutex
	path    string
	cfg     Config
	backend Backend
	now     func() time.Time
	logf    func(string, ...any)

	leases     map[string]time.Time // viewer id -> expiry
	assertion  Assertion
	graceUntil time.Time
	failures   int
	retryAt    time.Time
	lastErr    string
	lastLogged string

	psAt time.Time
	ps   string
}

// Load reads the preference at path (missing = off). A nil backend means
// the platform has no keep-awake; the preference then cannot be enabled.
func Load(path string, backend Backend, logf func(string, ...any)) (*Manager, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	m := &Manager{path: path, backend: backend, now: time.Now, logf: logf, leases: map[string]time.Time{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &m.cfg); err != nil {
		return nil, fmt.Errorf("power: parse %s: %w", path, err)
	}
	return m, nil
}

func (m *Manager) save() error {
	if err := os.MkdirAll(filepath.Dir(m.path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m.cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(m.path, append(data, '\n'), 0o600)
}

// Start runs the lease sweeper until stop closes.
func (m *Manager) Start(stop <-chan struct{}) {
	if m == nil {
		return
	}
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				m.Tick()
			}
		}
	}()
}

// SetEnabled persists the preference. Disabling releases the assertion at
// once; enabling takes effect for viewers already present.
func (m *Manager) SetEnabled(on bool) error {
	if m == nil || m.backend == nil {
		if on {
			return ErrUnsupported
		}
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cfg.KeepAwakeWhileViewing = on
	if err := m.save(); err != nil {
		return err
	}
	m.failures, m.retryAt, m.lastErr = 0, time.Time{}, ""
	m.reconcile()
	return nil
}

// Acquire records a viewer's presence for LeaseTTL from now. The id must
// be unique per socket; a renewal from a socket already holding a lease
// extends it. The first lease cancels a pending release.
func (m *Manager) Acquire(id string) {
	if m == nil || id == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.leases) == 0 {
		m.failures, m.retryAt = 0, time.Time{}
	}
	m.leases[id] = m.now().Add(LeaseTTL)
	m.graceUntil = time.Time{}
	m.reconcile()
}

// Release drops a viewer's lease (socket closed, or the client reported
// it is no longer looking). Idempotent.
func (m *Manager) Release(id string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.leases[id]; !ok {
		return
	}
	delete(m.leases, id)
	m.reconcile()
}

// Tick expires stale leases and applies any due release or retry. The
// sweeper calls it; tests drive it with a fake clock.
func (m *Manager) Tick() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	for id, exp := range m.leases {
		if !exp.After(now) {
			delete(m.leases, id)
		}
	}
	m.reconcile()
}

// Shutdown releases the assertion on the daemon's way out.
func (m *Manager) Shutdown() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopLocked()
}

// reconcile brings the assertion in line with the preference and the
// leases. Called with mu held.
func (m *Manager) reconcile() {
	want := m.cfg.KeepAwakeWhileViewing && m.backend != nil && len(m.leases) > 0
	now := m.now()
	switch {
	case want && m.assertion == nil:
		if m.failures >= maxFailures || now.Before(m.retryAt) {
			return
		}
		a, err := m.backend.Start()
		if err != nil {
			m.failed(err)
			return
		}
		m.assertion, m.lastErr, m.graceUntil = a, "", time.Time{}
		m.transition("keeping awake")
		go m.watch(a)
	case !want && m.assertion != nil:
		if !m.cfg.KeepAwakeWhileViewing {
			m.stopLocked()
			return
		}
		if m.graceUntil.IsZero() {
			m.graceUntil = now.Add(ReleaseGrace)
			return
		}
		if !now.Before(m.graceUntil) {
			m.stopLocked()
		}
	case want:
		m.graceUntil = time.Time{}
	}
}

// watch notices an assertion ending on its own (the child died) and lets
// reconcile retry if viewers still want it.
func (m *Manager) watch(a Assertion) {
	<-a.Done()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.assertion != a {
		return // we stopped it ourselves
	}
	m.assertion = nil
	m.failed(errors.New("assertion ended unexpectedly"))
	m.reconcile()
}

func (m *Manager) failed(err error) {
	m.failures++
	m.lastErr = err.Error()
	backoff := time.Second << uint(m.failures-1)
	if backoff > maxBackoff {
		backoff = maxBackoff
	}
	m.retryAt = m.now().Add(backoff)
	if m.failures >= maxFailures {
		m.transition("unavailable: " + m.lastErr)
	} else {
		m.transition(fmt.Sprintf("backend failed (%s); retry in %s", m.lastErr, backoff))
	}
}

func (m *Manager) stopLocked() {
	if m.assertion == nil {
		return
	}
	a := m.assertion
	m.assertion, m.graceUntil = nil, time.Time{}
	a.Stop()
	m.transition("released")
}

// transition logs a state change once, not on every tick that observes it.
func (m *Manager) transition(msg string) {
	if msg == m.lastLogged {
		return
	}
	m.lastLogged = msg
	m.logf("power: %s (viewers %d)", msg, len(m.leases))
}

// Status reports configured intent, viewers, backend and effective power
// separately: a live assertion on battery is not keeping anything awake.
func (m *Manager) Status() Status {
	if m == nil {
		return Status{State: "unsupported"}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	st := Status{
		Supported: m.backend != nil,
		Enabled:   m.cfg.KeepAwakeWhileViewing,
		Viewers:   len(m.leases),
		Asserting: m.assertion != nil,
		Error:     m.lastErr,
	}
	if m.backend != nil {
		st.Backend = m.backend.Name()
	}
	switch {
	case m.backend == nil:
		st.State = "unsupported"
	case !st.Enabled:
		st.State = "off"
	case m.failures >= maxFailures:
		st.State = "unavailable"
	case m.assertion == nil:
		st.State = "waiting"
	default:
		st.Power = m.powerSource()
		if st.Power == "battery" {
			st.State = "on_battery"
		} else {
			st.State = "keeping_awake"
		}
	}
	return st
}

func (m *Manager) powerSource() string {
	now := m.now()
	if now.Sub(m.psAt) < psCacheTTL {
		return m.ps
	}
	m.ps, m.psAt = m.backend.PowerSource(), now
	return m.ps
}
