package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/josecarlosrivas/muxdeck/internal/agent"
)

type client struct {
	base  string
	token string
	http  *http.Client
}

func newClient(base, token string) *client {
	return &client{
		base:  strings.TrimSuffix(base, "/"),
		token: token,
		// Long enough for a tunnel with a sleepy hop at the far end, short
		// enough that a script does not hang on a daemon that is gone.
		http: &http.Client{Timeout: 10 * time.Second},
	}
}

func (c *client) do(method, path string, body any) ([]byte, error) {
	var payload io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		payload = bytes.NewReader(buf)
	}
	req, err := http.NewRequest(method, c.base+path, payload)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", c.base, err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, httpError(resp.StatusCode, out)
	}
	return out, nil
}

// httpError unwraps the two error shapes the API uses — a JSON {"error"}
// object and http.Error's plain text — so a caller sees the daemon's own
// words rather than a status code.
func httpError(code int, body []byte) error {
	var msg struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &msg) == nil && msg.Error != "" {
		return fmt.Errorf("%s (%d)", msg.Error, code)
	}
	if text := strings.TrimSpace(string(body)); text != "" {
		return fmt.Errorf("%s (%d)", text, code)
	}
	if code == http.StatusUnauthorized {
		return fmt.Errorf("unauthorized (%d) — set -token or MUXDECK_TOKEN", code)
	}
	return fmt.Errorf("request failed (%d)", code)
}

// listEntry is the subset of a session the CLI renders. The daemon sends
// more; -json hands the response through untouched so a script sees the API's
// shape and not this one.
type listEntry struct {
	Name     string        `json:"name"`
	Windows  int           `json:"windows"`
	Attached int           `json:"attached"`
	Command  string        `json:"command"`
	Path     string        `json:"path"`
	Branch   string        `json:"branch"`
	Dirty    bool          `json:"dirty"`
	Ports    []int         `json:"ports"`
	Agent    *agent.Status `json:"agent"`
}

func (c *client) sessions() ([]listEntry, error) {
	raw, err := c.do(http.MethodGet, "/api/sessions", nil)
	if err != nil {
		return nil, err
	}
	var out []listEntry
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("unexpected response from %s: %w", c.base, err)
	}
	return out, nil
}

func (c *client) sessionsRaw() ([]byte, error) {
	return c.do(http.MethodGet, "/api/sessions", nil)
}
