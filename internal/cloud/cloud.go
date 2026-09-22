// Package cloud signs the daemon in to a muxdeck cloud account and mirrors
// the machines claimed there into the remotes registry, so every box on the
// account sits in the sidebar next to the local sessions. The account's
// device token is the only credential: the daemon exchanges it for a
// short-lived session to read the account, and the remote proxy presents it
// as the bearer the relay's client gate admits.
package cloud

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/josecarlosrivas/muxdeck/internal/remote"
)

// DefaultURL is the hosted control plane; MUXDECK_CLOUD_URL overrides it.
const DefaultURL = "https://cloud.muxdeck.app"

// SyncInterval paces the background refresh of the account's machine list.
const SyncInterval = 5 * time.Minute

// Config is the persisted account state. The device token lives in the
// config file (0600) like remote tokens do.
type Config struct {
	URL   string `json:"url,omitempty"`
	Token string `json:"token,omitempty"`
	// RelayDomain is the suffix a machine's relay name is served under
	// (<relayName>.<RelayDomain>). Derived from URL when empty: the control
	// plane's host minus its first label, so cloud.muxdeck.app implies
	// muxdeck.app.
	RelayDomain string `json:"relay_domain,omitempty"`
}

var (
	ErrBadToken = errors.New("cloud: the account refused the device token")
	ErrBadURL   = errors.New("cloud: url must be http(s) and, for an IP host, come with relay_domain")
)

// Daemon is one machine on the account as the API reports it.
type Daemon struct {
	Name       string `json:"name"`
	RelayName  string `json:"relay_name"`
	Remote     string `json:"remote,omitempty"`  // the sidebar name it was registered under
	Self       bool   `json:"self,omitempty"`    // this daemon; never registered as its own remote
	Skipped    string `json:"skipped,omitempty"` // why it was not registered
	LastSeenAt string `json:"last_seen_at,omitempty"`
}

// Status is the API-facing view of the account.
type Status struct {
	SignedIn bool     `json:"signed_in"`
	URL      string   `json:"url,omitempty"`
	Account  string   `json:"account,omitempty"`
	State    string   `json:"state"` // "signed_out" | "syncing" | "ok" | "down" | "revoked"
	Error    string   `json:"error,omitempty"`
	SyncedAt string   `json:"synced_at,omitempty"`
	Daemons  []Daemon `json:"daemons"`
}

type Manager struct {
	mu       sync.Mutex
	path     string
	cfg      Config
	remotes  *remote.Manager
	client   *http.Client
	self     string
	logf     func(string, ...any)
	session  string // cached mds_ session; re-minted on 401
	state    string
	lastErr  string
	account  string
	daemons  []Daemon
	syncedAt time.Time
	syncMu   sync.Mutex // one sync at a time; SignIn waits for an in-flight one
}

// DefaultConfigPath returns the config location under the user config dir.
func DefaultConfigPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = "."
	}
	return filepath.Join(dir, "muxdeck", "cloud.json")
}

// Load reads the config at path; a missing file is signed out. self is the
// name this daemon was claimed under (its hostname), so the sync can leave
// it out of its own sidebar.
func Load(path string, remotes *remote.Manager, self string) (*Manager, error) {
	m := &Manager{
		path:    path,
		remotes: remotes,
		client:  &http.Client{Timeout: 10 * time.Second},
		self:    self,
		logf:    func(string, ...any) {},
		state:   "signed_out",
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &m.cfg); err != nil {
		return nil, fmt.Errorf("cloud: parse %s: %w", path, err)
	}
	if m.cfg.Token != "" {
		m.state = "syncing"
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

// Start runs the background sync: once now when signed in, then every
// SyncInterval, until ctx ends.
func (m *Manager) Start(ctx context.Context, logf func(string, ...any)) {
	if logf != nil {
		m.logf = logf
	}
	go func() {
		t := time.NewTicker(SyncInterval)
		defer t.Stop()
		for {
			if m.Status().SignedIn {
				m.Sync()
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

// relayDomain resolves the suffix machines are served under.
func relayDomain(cfg Config) (string, error) {
	if cfg.RelayDomain != "" {
		return cfg.RelayDomain, nil
	}
	u, err := url.Parse(cfg.URL)
	if err != nil {
		return "", ErrBadURL
	}
	host := u.Hostname()
	labels := strings.Split(host, ".")
	if len(labels) < 3 || strings.Trim(host, "0123456789.") == "" {
		return "", ErrBadURL
	}
	return strings.Join(labels[1:], "."), nil
}

func validURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// SignIn stores the device token after the account accepts it, then syncs.
// An empty rawURL keeps the configured control plane, else the default.
func (m *Manager) SignIn(rawURL, token, domain string) (Status, error) {
	m.mu.Lock()
	cfg := m.cfg
	m.mu.Unlock()
	if rawURL != "" {
		cfg.URL = rawURL
	}
	if cfg.URL == "" {
		cfg.URL = DefaultURL
		if v := os.Getenv("MUXDECK_CLOUD_URL"); v != "" {
			cfg.URL = v
		}
	}
	if domain != "" {
		cfg.RelayDomain = domain
	}
	cfg.URL = strings.TrimRight(cfg.URL, "/")
	if !validURL(cfg.URL) {
		return m.Status(), ErrBadURL
	}
	if _, err := relayDomain(cfg); err != nil {
		return m.Status(), err
	}
	cfg.Token = strings.TrimSpace(token)
	if cfg.Token == "" {
		return m.Status(), ErrBadToken
	}
	m.syncMu.Lock()
	defer m.syncMu.Unlock()
	session, err := m.mintSession(cfg)
	if err != nil {
		return m.Status(), err
	}
	m.mu.Lock()
	m.cfg = cfg
	m.session = session
	m.state = "syncing"
	m.lastErr = ""
	err = m.save()
	m.mu.Unlock()
	if err != nil {
		return m.Status(), err
	}
	m.sync()
	return m.Status(), nil
}

// SignOut revokes the device token at the account (best effort — a
// revoked or unreachable account still signs out locally) and drops the
// machines it registered.
func (m *Manager) SignOut() error {
	m.syncMu.Lock()
	defer m.syncMu.Unlock()
	m.mu.Lock()
	cfg, session := m.cfg, m.session
	m.cfg = Config{URL: cfg.URL, RelayDomain: cfg.RelayDomain}
	m.session = ""
	m.state, m.lastErr, m.account, m.daemons = "signed_out", "", "", nil
	m.syncedAt = time.Time{}
	err := m.save()
	m.mu.Unlock()
	if cfg.Token != "" {
		if session == "" {
			session, _ = m.mintSession(cfg)
		}
		if session != "" {
			body, _ := json.Marshal(map[string]string{"token": cfg.Token})
			req, _ := http.NewRequest("POST", cfg.URL+"/api/signout", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+session)
			if res, rerr := m.client.Do(req); rerr == nil {
				res.Body.Close()
			}
		}
	}
	if _, serr := m.remotes.SetCloud(nil); serr != nil && err == nil {
		err = serr
	}
	return err
}

// Sync refreshes the machine list from the account and re-registers the
// remotes. Safe to call from any goroutine; concurrent calls serialize.
func (m *Manager) Sync() Status {
	m.syncMu.Lock()
	defer m.syncMu.Unlock()
	m.sync()
	return m.Status()
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := Status{
		SignedIn: m.cfg.Token != "",
		URL:      m.cfg.URL,
		Account:  m.account,
		State:    m.state,
		Error:    m.lastErr,
		Daemons:  append([]Daemon{}, m.daemons...),
	}
	if !m.syncedAt.IsZero() {
		st.SyncedAt = m.syncedAt.UTC().Format(time.RFC3339)
	}
	return st
}

type apiDaemon struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	RelayName  *string `json:"relayName"`
	ClaimedAt  *string `json:"claimedAt"`
	LastSeenAt *string `json:"lastSeenAt"`
}

// mintSession exchanges the device token for a session bearer.
func (m *Manager) mintSession(cfg Config) (string, error) {
	body, _ := json.Marshal(map[string]string{"token": cfg.Token})
	res, err := m.client.Post(cfg.URL+"/api/session", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusUnauthorized {
		return "", ErrBadToken
	}
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("cloud: %s answered %s", cfg.URL, res.Status)
	}
	var out struct {
		Session string `json:"session"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil || out.Session == "" {
		return "", errors.New("cloud: unexpected session response")
	}
	return out.Session, nil
}

// get performs an authenticated read; errUnauthorized signals a stale session.
var errUnauthorized = errors.New("unauthorized")

func (m *Manager) get(cfg Config, session, path string, v any) error {
	req, _ := http.NewRequest("GET", cfg.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+session)
	res, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusUnauthorized {
		return errUnauthorized
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("cloud: %s answered %s", path, res.Status)
	}
	return json.NewDecoder(res.Body).Decode(v)
}

func (m *Manager) sync() {
	m.mu.Lock()
	cfg, session := m.cfg, m.session
	m.mu.Unlock()
	if cfg.Token == "" {
		return
	}
	fetch := func(session string) (string, []apiDaemon, error) {
		var acct struct {
			Username string `json:"username"`
		}
		if err := m.get(cfg, session, "/api/account", &acct); err != nil {
			return "", nil, err
		}
		var list []apiDaemon
		if err := m.get(cfg, session, "/api/daemons", &list); err != nil {
			return "", nil, err
		}
		return acct.Username, list, nil
	}
	var account string
	var list []apiDaemon
	err := errUnauthorized
	if session != "" {
		account, list, err = fetch(session)
	}
	if errors.Is(err, errUnauthorized) {
		session, err = m.mintSession(cfg)
		if err == nil {
			account, list, err = fetch(session)
		}
	}
	if errors.Is(err, ErrBadToken) {
		// The account revoked this device: forget the token so nothing keeps
		// presenting it, and take the machines out of the sidebar.
		m.logf("cloud: device token revoked — sign in again with :cloud signin")
		m.mu.Lock()
		m.cfg.Token = ""
		m.session = ""
		m.state, m.lastErr = "revoked", "the account revoked this device token; sign in again"
		m.daemons = nil
		m.save()
		m.mu.Unlock()
		m.remotes.SetCloud(nil)
		return
	}
	if err != nil {
		m.mu.Lock()
		m.state, m.lastErr = "down", err.Error()
		m.mu.Unlock()
		return
	}
	domain, _ := relayDomain(cfg)
	scheme := "https"
	if u, perr := url.Parse(cfg.URL); perr == nil && u.Scheme == "http" {
		scheme = "http"
	}
	var want []remote.Remote
	var daemons []Daemon
	for _, d := range list {
		if d.RelayName == nil || d.ClaimedAt == nil {
			continue
		}
		entry := Daemon{Name: d.Name, RelayName: *d.RelayName}
		if d.LastSeenAt != nil {
			entry.LastSeenAt = *d.LastSeenAt
		}
		if m.self != "" && sameMachine(d.Name, m.self) {
			entry.Self = true
			daemons = append(daemons, entry)
			continue
		}
		entry.Remote = remoteName(d.Name, *d.RelayName)
		want = append(want, remote.Remote{
			Name:  entry.Remote,
			Mode:  "url",
			URL:   fmt.Sprintf("%s://%s.%s", scheme, *d.RelayName, domain),
			Token: cfg.Token,
		})
		daemons = append(daemons, entry)
	}
	skipped, serr := m.remotes.SetCloud(want)
	for i := range daemons {
		if why, ok := skipped[daemons[i].Remote]; ok {
			daemons[i].Skipped = why
			daemons[i].Remote = ""
		}
	}
	m.mu.Lock()
	m.session = session
	m.account = account
	m.daemons = daemons
	m.syncedAt = time.Now()
	m.state, m.lastErr = "ok", ""
	if serr != nil {
		m.state, m.lastErr = "down", serr.Error()
	}
	m.mu.Unlock()
}

var badChars = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

// sameMachine matches a claimed machine against this daemon's own name the
// way the registry names them: first label, case-folded. The account may
// hold the name the relay was claimed under while the daemon knows its
// hostname with a domain suffix (Alices-MacBook-Air.local vs
// alices-macbook-air), and a daemon that fails to recognize itself lists
// itself in its own sidebar.
func sameMachine(claimed, self string) bool {
	return remoteName(claimed, "") != "" && remoteName(claimed, "") == remoteName(self, "")
}

// remoteName turns a claimed machine's display name into a registry name:
// the first hostname label, lower-cased, non-name characters folded to "-".
// A name that folds to nothing falls back to the relay name.
func remoteName(name, relayName string) string {
	n := name
	if i := strings.IndexByte(n, '.'); i > 0 {
		n = n[:i]
	}
	n = strings.Trim(badChars.ReplaceAllString(strings.ToLower(n), "-"), "-")
	if n == "" {
		n = relayName
	}
	if len(n) > 32 {
		n = n[:32]
	}
	return n
}
