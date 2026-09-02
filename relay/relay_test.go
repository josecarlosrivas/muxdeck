package relay

import (
	"context"
	"errors"
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
	m.Start(handler, true, t.Logf)
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

// --- multi-tenant ---

// keyAuth admits a set of bearer→key mappings; anything else is a hard
// reject. errAuth always fails transiently.
type keyAuth map[string]string

func (a keyAuth) Auth(_ context.Context, bearer string) (string, error) {
	if k, ok := a[bearer]; ok {
		return k, nil
	}
	return "", ErrReject
}

type errAuth struct{}

func (errAuth) Auth(_ context.Context, _ string) (string, error) {
	return "", errors.New("control plane unreachable")
}

func startMux(t *testing.T, auth Authenticator, router Router) (*httptest.Server, string) {
	t.Helper()
	ts := httptest.NewServer(New(auth, router))
	t.Cleanup(ts.Close)
	return ts, "ws" + strings.TrimPrefix(ts.URL, "http") + "/tunnel"
}

// hostLabelRouter routes by the first DNS label of the Host — the hosted
// relay's scheme (lynx-pinecone.muxdeck.app -> "lynx-pinecone").
func hostLabelRouter(r *http.Request) string {
	host := r.Host
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	if i := strings.IndexByte(host, '.'); i >= 0 {
		return host[:i]
	}
	return host
}

func TestMultiTenantRouting(t *testing.T) {
	ts, wsURL := startMux(t, keyAuth{"cred-a": "alpha", "cred-b": "bravo"}, hostLabelRouter)

	a := namedDaemon("A")
	b := namedDaemon("B")
	ma := startManager(t, wsURL, "cred-a", a)
	mb := startManager(t, wsURL, "cred-b", b)
	waitState(t, ma, "connected")
	waitState(t, mb, "connected")

	// Same relay, different Host label -> different daemon.
	if got := getHost(t, ts.URL, "alpha.muxdeck.app"); got != "A" {
		t.Fatalf("alpha routed to %q, want A", got)
	}
	if got := getHost(t, ts.URL, "bravo.muxdeck.app"); got != "B" {
		t.Fatalf("bravo routed to %q, want B", got)
	}
	// An unknown relay name has no tunnel: 503.
	res, err := doHost(t, ts.URL, "ghost.muxdeck.app")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("unknown host: got %d, want 503", res.StatusCode)
	}
}

func TestTransientAuthIsRetryable(t *testing.T) {
	// A transient authenticator error must be 503 (client retries), never
	// 401 (which the client treats as permanent revocation).
	_, wsURL := startMux(t, errAuth{}, hostLabelRouter)
	m := startManager(t, wsURL, "whatever", daemonHandler())
	// Never reaches "rejected"; sits in dialing/down and keeps retrying.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if st := m.Status().State; st == "rejected" {
			t.Fatal("transient auth error became a permanent rejection")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func namedDaemon(name string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /who", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, name)
	})
	return mux
}

func getHost(t *testing.T, base, host string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		res, err := doHost(t, base, host)
		if err == nil && res.StatusCode == http.StatusOK {
			body, _ := io.ReadAll(res.Body)
			res.Body.Close()
			return string(body)
		}
		if res != nil {
			res.Body.Close()
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s/who never returned 200", host)
	return ""
}

func doHost(t *testing.T, base, host string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequest("GET", base+"/who", nil)
	if err != nil {
		return nil, err
	}
	req.Host = host
	return http.DefaultClient.Do(req)
}

func TestUnsecuredDaemonIsBlocked(t *testing.T) {
	_, wsURL := startRelay(t, "sekrit")
	m, err := LoadManager(filepath.Join(t.TempDir(), "relay.json"))
	if err != nil {
		t.Fatal(err)
	}
	m.Start(daemonHandler(), false, t.Logf) // authless daemon
	if err := m.Set(Config{URL: wsURL, Key: "sekrit"}); err != nil {
		t.Fatal(err)
	}
	waitState(t, m, "blocked") // never dials

	// Securing the daemon (restart with a token) unblocks the same config.
	m.Start(daemonHandler(), true, t.Logf)
	waitState(t, m, "connected")
}

// A gated relay authenticates clients itself, so a tokenless daemon may
// dial when the config says so.
func TestGatedRelayAllowsTokenlessDaemon(t *testing.T) {
	_, wsURL := startRelay(t, "sekrit")
	m, err := LoadManager(filepath.Join(t.TempDir(), "relay.json"))
	if err != nil {
		t.Fatal(err)
	}
	m.Start(daemonHandler(), false, t.Logf) // authless daemon
	if err := m.Set(Config{URL: wsURL, Key: "sekrit", Gated: true}); err != nil {
		t.Fatal(err)
	}
	waitState(t, m, "connected")
	t.Cleanup(func() { m.SetOff(true) })
}

type fakeClientAuth struct{ err error }

func (a fakeClientAuth) ClientAuth(_ context.Context, bearer, key string) error {
	if a.err != nil {
		return a.err
	}
	if bearer != "good" || key != "" {
		return ErrReject
	}
	return nil
}

func TestClientAuthGatesProxy(t *testing.T) {
	srv := NewServer("sekrit")
	srv.SetClientAuth(fakeClientAuth{})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/tunnel"
	m := startManager(t, wsURL, "sekrit", daemonHandler())
	waitState(t, m, "connected")

	get := func(bearer string) int {
		req, err := http.NewRequest("GET", ts.URL+"/echo", nil)
		if err != nil {
			t.Fatal(err)
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}

	waitFor(t, ts.URL+"/relay/healthz", http.StatusOK)
	if code := get(""); code != http.StatusUnauthorized {
		t.Fatalf("no bearer: got %d, want 401", code)
	}
	if code := get("bad"); code != http.StatusUnauthorized {
		t.Fatalf("bad bearer: got %d, want 401", code)
	}
	if code := get("good"); code != http.StatusOK {
		t.Fatalf("good bearer: got %d, want 200", code)
	}

	// WebSocket upgrades pass the same gate: the app's dialer sets the
	// Authorization header.
	hdr := http.Header{"Origin": {ts.URL}, "Authorization": {"Bearer good"}}
	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+"/ws", hdr)
	if err != nil {
		t.Fatalf("ws dial with bearer: %v", err)
	}
	c.Close()
	if _, res, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+"/ws", http.Header{"Origin": {ts.URL}}); err == nil || res == nil || res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("ws dial without bearer must 401 (err %v)", err)
	}

	// A transient verifier failure is 503 — the client retries, nobody is
	// signed out by a control-plane blip.
	srv.SetClientAuth(fakeClientAuth{err: errors.New("boom")})
	if code := get("good"); code != http.StatusServiceUnavailable {
		t.Fatalf("transient auth error: got %d, want 503", code)
	}
}

func TestClientCookieLogin(t *testing.T) {
	srv := NewServer("sekrit")
	srv.SetClientAuth(fakeClientAuth{})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/tunnel"

	// The daemon reports the Cookie header it received, proving the relay
	// strips its own credential before proxying.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /cookies", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "cookies="+r.Header.Get("Cookie"))
	})
	m := startManager(t, wsURL, "sekrit", mux)
	waitState(t, m, "connected")
	waitFor(t, ts.URL+"/relay/healthz", http.StatusOK)

	// Redirects must not be followed: the cookie is on the 303 itself.
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	login := func(token string) *http.Response {
		res, err := noRedirect.Post(ts.URL+"/relay/login", "application/x-www-form-urlencoded",
			strings.NewReader("token="+token))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res
	}

	if res, err := http.Get(ts.URL + "/relay/login"); err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("login page: %v %v", res.StatusCode, err)
	}
	if res := login("bad"); res.StatusCode != http.StatusUnauthorized || len(res.Cookies()) != 0 {
		t.Fatalf("bad token login: got %d, %d cookies; want 401, none", res.StatusCode, len(res.Cookies()))
	}
	res := login("good")
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("good token login: got %d, want 303", res.StatusCode)
	}
	var cookie *http.Cookie
	for _, c := range res.Cookies() {
		if c.Name == clientCookie {
			cookie = c
		}
	}
	if cookie == nil || cookie.Value != "good" || !cookie.HttpOnly {
		t.Fatalf("login must set an HttpOnly %s cookie, got %+v", clientCookie, cookie)
	}

	get := func(cookies ...*http.Cookie) (int, string) {
		req, err := http.NewRequest("GET", ts.URL+"/cookies", nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cookies {
			req.AddCookie(c)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		return res.StatusCode, string(body)
	}

	if code, _ := get(); code != http.StatusUnauthorized {
		t.Fatalf("no cookie: got %d, want 401", code)
	}
	code, body := get(cookie, &http.Cookie{Name: "muxdeck_token", Value: "daemonCookie"})
	if code != http.StatusOK {
		t.Fatalf("cookie auth: got %d, want 200", code)
	}
	// The relay credential is stripped; the daemon's own cookies pass.
	if body != "cookies=muxdeck_token=daemonCookie" {
		t.Fatalf("daemon saw %q", body)
	}

	// WebSocket upgrades carry cookies like any request.
	hdr := http.Header{"Origin": {ts.URL}, "Cookie": {cookie.Name + "=" + cookie.Value}}
	if _, res2, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+"/cookies", hdr); err == nil || res2 == nil || res2.StatusCode == http.StatusUnauthorized {
		// The route is not a websocket, so the dial fails — but it must get
		// past the auth gate (anything but 401).
		if res2 != nil && res2.StatusCode == http.StatusUnauthorized {
			t.Fatalf("ws dial with cookie must pass the gate")
		}
	}

	res3, err := noRedirect.Get(ts.URL + "/relay/logout")
	if err != nil {
		t.Fatal(err)
	}
	res3.Body.Close()
	cleared := false
	for _, c := range res3.Cookies() {
		if c.Name == clientCookie && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatalf("logout must clear the cookie")
	}
}

func TestLoginOnlyWhenGated(t *testing.T) {
	// Without client auth there is nothing to log in to: the path falls
	// through to the proxy like any other.
	ts, wsURL := startRelay(t, "sekrit")
	m := startManager(t, wsURL, "sekrit", daemonHandler())
	waitState(t, m, "connected")
	res, err := http.Get(ts.URL + "/relay/login")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("ungated /relay/login: got %d, want daemon 404", res.StatusCode)
	}
}

func TestUnauthedNavigationRedirectsToLogin(t *testing.T) {
	srv := NewServer("sekrit")
	srv.SetClientAuth(fakeClientAuth{})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	req, _ := http.NewRequest("GET", ts.URL+"/", nil)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	res, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/relay/login" {
		t.Fatalf("html navigation without credential: got %d -> %q, want 303 -> /relay/login", res.StatusCode, res.Header.Get("Location"))
	}
	req2, _ := http.NewRequest("GET", ts.URL+"/api/sessions", nil)
	res2, err := noRedirect.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	res2.Body.Close()
	if res2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("api fetch without credential: got %d, want 401", res2.StatusCode)
	}
}
