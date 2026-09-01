package relay

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// daemonHandler stands in for the muxdeck daemon: an HTTP route that
// reports the Host it saw, and a WebSocket echo behind gorilla's default
// same-origin check — the same check the real attach endpoint runs, so it
// proves the proxy's Host/Origin alignment.
func daemonHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /echo", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "host="+r.Host)
	})
	up := websocket.Upgrader{} // default CheckOrigin: Origin must match Host
	mux.HandleFunc("GET /ws", func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		mt, msg, err := c.ReadMessage()
		if err != nil {
			return
		}
		c.WriteMessage(mt, append([]byte("echo:"), msg...))
	})
	return mux
}

func startRelay(t *testing.T, secret string) (*httptest.Server, string) {
	t.Helper()
	ts := httptest.NewServer(NewServer(secret))
	t.Cleanup(ts.Close)
	return ts, "ws" + strings.TrimPrefix(ts.URL, "http") + "/tunnel"
}

func waitFor(t *testing.T, url string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		res, err := http.Get(url)
		if err == nil {
			res.Body.Close()
			if res.StatusCode == want {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s never returned %d", url, want)
}

// startManager wires a Manager at a temp config path to the relay under test.
func startManager(t *testing.T, wsURL, key string, handler http.Handler) *Manager {
	t.Helper()
	m, err := LoadManager(filepath.Join(t.TempDir(), "relay.json"))
	if err != nil {
		t.Fatal(err)
	}
	m.Start(handler, t.Logf)
	if err := m.Set(Config{URL: wsURL, Key: key}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.SetOff(true) })
	return m
}

func waitState(t *testing.T, m *Manager, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if m.Status().State == want {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("state never became %q (is %q, err %q)", want, m.Status().State, m.Status().Error)
}

func TestProxyThroughTunnel(t *testing.T) {
	ts, wsURL := startRelay(t, "sekrit")
	m := startManager(t, wsURL, "sekrit", daemonHandler())
	waitState(t, m, "connected")

	waitFor(t, ts.URL+"/echo", http.StatusOK)
	res, err := http.Get(ts.URL + "/echo")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if string(body) != "host="+tunnelHost {
		t.Fatalf("proxied Host: got %q", body)
	}

	// WebSocket through the relay, with a browser-like Origin naming the
	// relay — the daemon's same-origin check must still pass.
	hdr := http.Header{"Origin": {ts.URL}}
	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+"/ws", hdr)
	if err != nil {
		t.Fatalf("ws dial through relay: %v", err)
	}
	defer c.Close()
	if err := c.WriteMessage(websocket.TextMessage, []byte("hi")); err != nil {
		t.Fatal(err)
	}
	_, msg, err := c.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if string(msg) != "echo:hi" {
		t.Fatalf("ws echo: got %q", msg)
	}

	// Tunnel off: the relay answers 503, not a hang.
	m.SetOff(true)
	waitState(t, m, "off")
	waitFor(t, ts.URL+"/echo", http.StatusServiceUnavailable)
}

func TestDialRejectedIsPermanent(t *testing.T) {
	_, wsURL := startRelay(t, "sekrit")
	m := startManager(t, wsURL, "wrong", daemonHandler())
	waitState(t, m, "rejected")

	// "on" is the re-arm: it dials again (and is rejected again here).
	if err := m.SetOff(false); err != nil {
		t.Fatal(err)
	}
	waitState(t, m, "rejected")
}

func TestNoDaemonIs503(t *testing.T) {
	ts, _ := startRelay(t, "sekrit")
	res, err := http.Get(ts.URL + "/echo")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", res.StatusCode)
	}
}

func TestRedialReplacesDaemon(t *testing.T) {
	ts, wsURL := startRelay(t, "sekrit")
	m1 := startManager(t, wsURL, "sekrit", daemonHandler())
	waitState(t, m1, "connected")
	waitFor(t, ts.URL+"/echo", http.StatusOK)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /echo", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "second")
	})
	m2 := startManager(t, wsURL, "sekrit", mux)
	waitState(t, m2, "connected")

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		res, err := http.Get(ts.URL + "/echo")
		if err == nil {
			body, _ := io.ReadAll(res.Body)
			res.Body.Close()
			if string(body) == "second" {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("second daemon never took over")
}

func TestConfigRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.json")
	m, err := LoadManager(path)
	if err != nil {
		t.Fatal(err)
	}
	if st := m.Status(); st.Configured || st.State != "idle" {
		t.Fatalf("fresh manager: %+v", st)
	}
	if err := m.Set(Config{URL: "nope://x"}); err == nil {
		t.Fatal("bad scheme accepted")
	}
	if err := m.Set(Config{URL: "wss://relay.example/tunnel", Key: "k"}); err != nil {
		t.Fatal(err)
	}
	if err := m.SetOff(true); err != nil {
		t.Fatal(err)
	}

	again, err := LoadManager(path)
	if err != nil {
		t.Fatal(err)
	}
	st := again.Status()
	if !st.Configured || st.URL != "wss://relay.example/tunnel" || !st.Off {
		t.Fatalf("reloaded config: %+v", st)
	}
}
