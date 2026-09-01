package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Config is the persisted tunnel configuration. The credential lives in the
// config file (0600) like remote tokens do; an OS keychain remains an open
// design question and would slot in behind the same accessors.
type Config struct {
	URL string `json:"url,omitempty"`
	Key string `json:"key,omitempty"`
	Off bool   `json:"off,omitempty"`
	// Gated says the relay authenticates browsers and apps itself before
	// proxying anything down the tunnel, so a tokenless daemon may dial:
	// the relay is the gate. Set from the claim response during setup —
	// never assume it of a relay that didn't advertise it.
	Gated bool `json:"gated,omitempty"`
}

// ErrBadConfig is returned when a relay URL fails validation.
var ErrBadConfig = errors.New("relay: url must be ws:// or wss://")

// DefaultConfigPath returns the config location under the user config dir.
func DefaultConfigPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = "."
	}
	return filepath.Join(dir, "muxdeck", "relay.json")
}

// Status is the API-facing view of the tunnel.
type Status struct {
	Configured bool   `json:"configured"`
	URL        string `json:"url,omitempty"`
	Off        bool   `json:"off,omitempty"`
	Gated      bool   `json:"gated,omitempty"`
	State      string `json:"state"` // "idle" | "off" | "blocked" | "dialing" | "connected" | "down" | "rejected"
	Error      string `json:"error,omitempty"`
}

// Manager owns the tunnel: persisted config, the dial loop, and its state.
type Manager struct {
	mu         sync.Mutex
	path       string
	cfg        Config
	overridden bool // flag-provided config is session-only, never saved
	handler    http.Handler
	secured    bool // the daemon has an access token; a tunnel must never expose an authless daemon
	logf       func(string, ...any)
	cancel     context.CancelFunc
	state      string
	lastErr    string
}

// LoadManager reads the config at path; a missing file is an empty config.
func LoadManager(path string) (*Manager, error) {
	m := &Manager{path: path, state: "idle", logf: func(string, ...any) {}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &m.cfg); err != nil {
		return nil, fmt.Errorf("relay: parse %s: %w", path, err)
	}
	return m, nil
}

func validURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "ws" || u.Scheme == "wss") && u.Host != ""
}

func (m *Manager) save() error {
	if m.overridden {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(m.path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m.cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(m.path, append(data, '\n'), 0o600)
}

// Override installs flag-provided config for this process only.
func (m *Manager) Override(rawURL, key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cfg = Config{URL: rawURL, Key: key}
	m.overridden = true
}

// Start begins dialing if the config says to. handler is what the tunnel
// serves — the daemon's own mux. secured says the daemon requires an access
// token: a tunnel publishes the daemon, so an authless one is never dialed
// (state "blocked") — otherwise anyone who learns the relay name gets an
// unauthenticated terminal. The exception is a gated relay (Config.Gated),
// which authenticates clients itself before proxying.
func (m *Manager) Start(handler http.Handler, secured bool, logf func(string, ...any)) {
	m.mu.Lock()
	m.handler = handler
	m.secured = secured
	if logf != nil {
		m.logf = logf
	}
	m.mu.Unlock()
	m.restart()
}

// Set validates, persists, and applies a new config.
func (m *Manager) Set(cfg Config) error {
	if cfg.URL != "" && !validURL(cfg.URL) {
		return ErrBadConfig
	}
	m.mu.Lock()
	m.cfg = cfg
	err := m.save()
	m.mu.Unlock()
	if err != nil {
		return err
	}
	m.restart()
	return nil
}

// SetOff soft-disables (or re-enables) the tunnel, keeping the config.
// It is also the re-arm after a rejection.
func (m *Manager) SetOff(off bool) error {
	m.mu.Lock()
	m.cfg.Off = off
	err := m.save()
	m.mu.Unlock()
	if err != nil {
		return err
	}
	m.restart()
	return nil
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return Status{
		Configured: m.cfg.URL != "",
		URL:        m.cfg.URL,
		Off:        m.cfg.Off,
		Gated:      m.cfg.Gated,
		State:      m.state,
		Error:      m.lastErr,
	}
}

func (m *Manager) setState(state, errMsg string) {
	m.mu.Lock()
	m.state, m.lastErr = state, errMsg
	m.mu.Unlock()
}

// restart stops any running loop and starts one when config allows.
func (m *Manager) restart() {
	m.mu.Lock()
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	cfg, handler, logf := m.cfg, m.handler, m.logf
	switch {
	case handler == nil || cfg.URL == "":
		m.state, m.lastErr = "idle", ""
	case cfg.Off:
		m.state, m.lastErr = "off", ""
	case !m.secured && !cfg.Gated:
		m.state, m.lastErr = "blocked", "daemon has no access token; restart with -token auto to use the relay"
		logf("relay: tunnel blocked — the daemon has no access token and a tunnel would expose it publicly; restart with -token auto (or MUXDECK_TOKEN=auto)")
	default:
		ctx, cancel := context.WithCancel(context.Background())
		m.cancel = cancel
		m.state, m.lastErr = "dialing", ""
		go m.loop(ctx, cfg, handler, logf)
	}
	m.mu.Unlock()
}

func (m *Manager) loop(ctx context.Context, cfg Config, handler http.Handler, logf func(string, ...any)) {
	backoff := time.Second
	for {
		m.setState("dialing", "")
		err := runOnce(ctx, cfg.URL, cfg.Key, handler, func() {
			m.setState("connected", "")
			backoff = time.Second
		})
		if errors.Is(err, ErrRevoked) {
			logf("relay: credential rejected — tunnel stopped (re-claim, then `muxdeck relay on`)")
			m.setState("rejected", ErrRevoked.Error())
			return
		}
		if ctx.Err() != nil {
			return
		}
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		m.setState("down", msg)
		logf("relay: tunnel down (%v); redialing in %s", err, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}
