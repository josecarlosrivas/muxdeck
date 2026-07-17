// Package remote manages named remote muxdeck instances: a persisted
// registry, ssh tunnel supervision, and a reverse proxy to their APIs.
package remote

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

var nameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

// ErrBadRemote is returned when an add request fails validation.
var ErrBadRemote = errors.New("remote: name must match [A-Za-z0-9_-]{1,32}; mode ssh needs host, mode url needs an http(s) url")

type Remote struct {
	Name       string `json:"name"`
	Mode       string `json:"mode"` // "ssh" | "url"
	Host       string `json:"host,omitempty"`
	URL        string `json:"url,omitempty"`
	RemotePort int    `json:"remote_port,omitempty"` // remote loopback port for ssh mode
	Token      string `json:"token,omitempty"`       // remote muxdeck token, injected by the proxy
}

// Status is the API-facing view of a remote; the token never leaves the file.
type Status struct {
	Name     string `json:"name"`
	Mode     string `json:"mode"`
	Host     string `json:"host,omitempty"`
	URL      string `json:"url,omitempty"`
	HasToken bool   `json:"has_token"`
	State    string `json:"state"` // "ok" | "down"
	Error    string `json:"error,omitempty"`
}

type conn struct {
	mu       sync.Mutex
	base     *url.URL
	cmd      *exec.Cmd
	lastErr  string
	lastTry  time.Time
}

type Manager struct {
	mu      sync.Mutex
	path    string
	remotes []Remote
	conns   map[string]*conn
	client  *http.Client
}

// DefaultPath returns the registry location under the user config dir.
func DefaultPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = "."
	}
	return filepath.Join(dir, "muxdeck", "remotes.json")
}

func Load(path string) (*Manager, error) {
	m := &Manager{path: path, conns: map[string]*conn{}, client: &http.Client{Timeout: 3 * time.Second}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &m.remotes); err != nil {
		return nil, fmt.Errorf("remote: parse %s: %w", path, err)
	}
	return m, nil
}

func (m *Manager) save() error {
	if err := os.MkdirAll(filepath.Dir(m.path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m.remotes, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(m.path, append(data, '\n'), 0o600)
}

func valid(r Remote) bool {
	if !nameRe.MatchString(r.Name) {
		return false
	}
	switch r.Mode {
	case "ssh":
		return r.Host != ""
	case "url":
		u, err := url.Parse(r.URL)
		return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
	}
	return false
}

func (m *Manager) Add(r Remote) error {
	if !valid(r) {
		return ErrBadRemote
	}
	if r.Mode == "ssh" && r.RemotePort == 0 {
		r.RemotePort = 8300
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.remotes {
		if m.remotes[i].Name == r.Name {
			m.remotes[i] = r
			m.dropConn(r.Name)
			return m.save()
		}
	}
	m.remotes = append(m.remotes, r)
	return m.save()
}

func (m *Manager) Delete(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.remotes {
		if m.remotes[i].Name == name {
			m.remotes = append(m.remotes[:i], m.remotes[i+1:]...)
			m.dropConn(name)
			return m.save()
		}
	}
	return fmt.Errorf("no such remote: %s", name)
}

func (m *Manager) dropConn(name string) {
	if c := m.conns[name]; c != nil && c.cmd != nil && c.cmd.Process != nil {
		c.cmd.Process.Kill()
	}
	delete(m.conns, name)
}

func (m *Manager) get(name string) (Remote, *conn, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.remotes {
		if r.Name == name {
			c := m.conns[name]
			if c == nil {
				c = &conn{}
				m.conns[name] = c
			}
			return r, c, true
		}
	}
	return Remote{}, nil, false
}

// List reports every remote with a liveness probe (parallel, bounded by the
// client timeout) so the sidebar can paint status dots.
func (m *Manager) List() []Status {
	m.mu.Lock()
	remotes := append([]Remote(nil), m.remotes...)
	m.mu.Unlock()

	out := make([]Status, len(remotes))
	var wg sync.WaitGroup
	for i, r := range remotes {
		out[i] = Status{Name: r.Name, Mode: r.Mode, Host: r.Host, URL: r.URL, HasToken: r.Token != ""}
		wg.Add(1)
		go func(i int, r Remote) {
			defer wg.Done()
			if err := m.probe(r.Name); err != nil {
				out[i].State = "down"
				out[i].Error = err.Error()
			} else {
				out[i].State = "ok"
			}
		}(i, r)
	}
	wg.Wait()
	return out
}

func (m *Manager) probe(name string) error {
	base, token, err := m.ensure(name)
	if err != nil {
		return err
	}
	req, _ := http.NewRequest("GET", base.JoinPath("/api/sessions").String(), nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := m.client.Do(req)
	if err != nil {
		return err
	}
	res.Body.Close()
	if res.StatusCode == http.StatusUnauthorized {
		return errors.New("remote rejected token")
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("remote returned %s", res.Status)
	}
	return nil
}

// ensure returns a usable base URL for the remote, starting (or restarting)
// the ssh tunnel when needed. Failed tunnel spawns are not retried for a
// short window so status polls don't fork ssh in a tight loop.
func (m *Manager) ensure(name string) (*url.URL, string, error) {
	r, c, ok := m.get(name)
	if !ok {
		return nil, "", fmt.Errorf("no such remote: %s", name)
	}
	if r.Mode == "url" {
		u, err := url.Parse(r.URL)
		return u, r.Token, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cmd != nil && c.cmd.ProcessState == nil && c.base != nil && dialable(c.base.Host) {
		return c.base, r.Token, nil
	}
	if time.Since(c.lastTry) < 10*time.Second && c.lastErr != "" {
		return nil, "", errors.New(c.lastErr)
	}
	c.lastTry = time.Now()

	if c.cmd != nil && c.cmd.Process != nil {
		c.cmd.Process.Kill()
		c.cmd.Wait()
	}

	port, err := freePort()
	if err != nil {
		c.lastErr = err.Error()
		return nil, "", err
	}
	local := fmt.Sprintf("127.0.0.1:%d", port)
	cmd := exec.Command("ssh", "-N",
		"-o", "BatchMode=yes",
		"-o", "ExitOnForwardFailure=yes",
		"-o", "ConnectTimeout=5",
		"-o", "ServerAliveInterval=30",
		"-o", "ServerAliveCountMax=3",
		"-L", fmt.Sprintf("%s:127.0.0.1:%d", local, r.RemotePort),
		r.Host)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		c.lastErr = err.Error()
		return nil, "", err
	}
	go cmd.Wait() // reap; ProcessState flips non-nil on exit

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if cmd.ProcessState != nil {
			break
		}
		if dialable(local) {
			c.cmd = cmd
			c.base = &url.URL{Scheme: "http", Host: local}
			c.lastErr = ""
			return c.base, r.Token, nil
		}
		time.Sleep(150 * time.Millisecond)
	}
	cmd.Process.Kill()
	msg := strings.TrimSpace(stderr.String())
	if msg == "" {
		msg = "ssh tunnel did not come up"
	}
	if i := strings.LastIndexByte(msg, '\n'); i >= 0 {
		msg = msg[i+1:]
	}
	c.lastErr = "ssh: " + msg
	return nil, "", errors.New(c.lastErr)
}

func dialable(host string) bool {
	conn, err := net.DialTimeout("tcp", host, 300*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// Proxy forwards an /api/remotes/{name}/proxy/<rest> request to the remote's
// /api/<rest>, injecting the remote's token. WebSocket upgrades (attach)
// pass through — httputil.ReverseProxy handles 101 switching natively.
func (m *Manager) Proxy(name, rest string, w http.ResponseWriter, req *http.Request) {
	base, token, err := m.ensure(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = base.Scheme
			pr.Out.URL.Host = base.Host
			pr.Out.URL.Path = "/api/" + strings.TrimPrefix(rest, "/")
			pr.Out.Host = base.Host
			pr.Out.Header.Del("Cookie") // the local session cookie is not the remote's business
			// The browser's Origin names the local UI; the remote's websocket
			// upgrader enforces same-origin against ITS host, so align it.
			if pr.Out.Header.Get("Origin") != "" {
				pr.Out.Header.Set("Origin", base.Scheme+"://"+base.Host)
			}
			if token != "" {
				pr.Out.Header.Set("Authorization", "Bearer "+token)
			}
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			http.Error(w, "remote: "+err.Error(), http.StatusBadGateway)
		},
	}
	rp.ServeHTTP(w, req)
}

// Shutdown kills every live tunnel; call on daemon exit so ssh children
// don't outlive the process.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for name := range m.conns {
		m.dropConn(name)
	}
}
