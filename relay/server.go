package relay

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
)

var discardLogger = log.New(io.Discard, "", 0)

// tunnelHost is the constant authority proxied requests carry. The daemon's
// WebSocket upgrader enforces same-origin against its own Host, so the
// proxy aligns Host and Origin to this one name.
const tunnelHost = "daemon.tunnel"

var tunnelUpgrader = websocket.Upgrader{
	ReadBufferSize:  32 * 1024,
	WriteBufferSize: 32 * 1024,
	// The dialer is the muxdeck daemon, not a browser; no Origin to check.
	CheckOrigin: func(*http.Request) bool { return true },
}

// ErrReject tells the server a dialing daemon's credential is *permanently*
// bad: answer 401, which the client treats as revocation and stops. Any
// other Authenticator error is transient (401 vs "couldn't check right
// now") and answers 503, which the client retries — so a control plane
// blip never permanently disconnects a legitimate daemon.
var ErrReject = errors.New("relay: daemon rejected")

// Authenticator verifies a dialing daemon's bearer and returns the route
// key it is reachable under — a relay name for hosted, "" for the single
// self-hosted tunnel.
type Authenticator interface {
	Auth(ctx context.Context, bearer string) (key string, err error)
}

// Router maps an incoming browser request to the daemon key that serves it.
type Router func(r *http.Request) string

// ClientAuthenticator authorizes a proxied client request for the daemon
// key it routes to — the gate that lets a tokenless daemon sit behind the
// relay. nil error admits the request; ErrReject answers 401 (bad or
// missing credential); any other error answers 503 (couldn't check right
// now — the client may retry). Callers present the bearer from the
// request's Authorization header; per-request caching is the
// implementation's concern.
type ClientAuthenticator interface {
	ClientAuth(ctx context.Context, bearer, key string) error
}

// StaticAuth admits one shared secret and routes every request to the one
// daemon it accepts: the self-hosted, single-tenant case.
type StaticAuth struct{ Secret string }

func (a StaticAuth) Auth(_ context.Context, bearer string) (string, error) {
	if a.Secret == "" || subtle.ConstantTimeCompare([]byte(bearer), []byte(a.Secret)) != 1 {
		return "", ErrReject
	}
	return "", nil // "" is the single tunnel's key
}

type tunnel struct {
	sess  *yamux.Session
	proxy *httputil.ReverseProxy
}

// Server is the relay rendezvous. Daemons dial /tunnel and are authenticated
// into a route key; browsers hit everything else and are proxied down the
// tunnel their Host routes to. Self-host is one key (""), one daemon; hosted
// keys by relay name and serves many.
type Server struct {
	auth       Authenticator
	router     Router
	clientAuth ClientAuthenticator

	mu      sync.Mutex
	tunnels map[string]*tunnel
}

// New builds a relay with an explicit authenticator and router — the hosted,
// multi-tenant constructor.
func New(auth Authenticator, router Router) *Server {
	return &Server{auth: auth, router: router, tunnels: map[string]*tunnel{}}
}

// NewServer returns the single-tenant self-hosted relay: one shared secret,
// every request routed to the one connected daemon.
func NewServer(secret string) *Server {
	return New(StaticAuth{Secret: secret}, func(*http.Request) string { return "" })
}

// SetClientAuth makes the relay a gate: every proxied request (WebSocket
// upgrades included) must carry a bearer the authenticator admits for its
// route key. Without it the relay proxies unauthenticated and the daemon's
// own token is the only gate — the self-hosted default.
func (s *Server) SetClientAuth(a ClientAuthenticator) { s.clientAuth = a }

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/relay/healthz":
		w.Write([]byte("ok\n"))
	case r.URL.Path == "/tunnel":
		s.handleTunnel(w, r)
	case s.clientAuth != nil && r.URL.Path == "/relay/login":
		s.handleLogin(w, r)
	case s.clientAuth != nil && r.URL.Path == "/relay/logout":
		s.handleLogout(w, r)
	default:
		s.handleProxy(w, r)
	}
}

// clientCookie carries the client bearer for browsers that cannot set an
// Authorization header — a PWA opened straight at the relay origin. Set by
// /relay/login, read as a fallback by bearer, stripped before proxying so
// the credential never reaches the daemon.
const clientCookie = "muxdeck_relay_client"

func bearer(r *http.Request) string {
	if h := r.Header.Get("Authorization"); len(h) > 7 && h[:7] == "Bearer " {
		return h[7:]
	}
	if c, err := r.Cookie(clientCookie); err == nil {
		return c.Value
	}
	return ""
}

const loginPage = `<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>muxdeck relay</title>
<style>
  body { background: #0b0e11; color: #d7dde3; font-family: "SF Mono", Menlo, Monaco, monospace;
         display: flex; flex-direction: column; align-items: center; justify-content: center;
         min-height: 100vh; margin: 0; gap: 24px; font-size: 15px; }
  h1 { font-size: 22px; font-weight: 600; letter-spacing: 0.04em; color: #2dd4bf; margin: 0; }
  h1::before { content: "\276f "; color: #6b7681; }
  form { display: flex; flex-direction: column; gap: 10px; width: min(420px, 90vw); }
  input { background: #12161b; color: #d7dde3; border: 1px solid #232b33; border-radius: 8px;
          padding: 12px 14px; font: inherit; font-size: 14px; }
  input:focus { outline: none; border-color: #2dd4bf; }
  button { background: #2dd4bf; color: #0b0e11; font: inherit; font-weight: 600;
           border: none; border-radius: 8px; padding: 12px; cursor: pointer; }
  p { color: #f87171; font-size: 13px; min-height: 1.2em; margin: 0; }
</style></head><body>
<h1>muxdeck</h1>
<form method="post" action="/relay/login">
<input name="token" type="password" placeholder="app token" autocapitalize="none" autocorrect="off" autofocus required>
<button type="submit">sign in</button>
</form>
<p>%s</p>
</body></html>`

func serveLoginPage(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	fmt.Fprintf(w, loginPage, msg)
}

// secureRequest reports whether the client connection is https — directly
// or via a terminating proxy — so the cookie's Secure flag matches how the
// relay is actually reached (prod is always behind TLS; tests are not).
func secureRequest(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

// handleLogin verifies a pasted app token against the same ClientAuthenticator
// that gates every proxied request, then hands it back as a cookie. The
// cookie grants nothing the token itself does not: bearer() just presents it
// where the browser cannot set a header.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		serveLoginPage(w, http.StatusOK, "")
		return
	}
	tok := strings.TrimSpace(r.PostFormValue("token"))
	err := s.clientAuth.ClientAuth(r.Context(), tok, s.router(r))
	if errors.Is(err, ErrReject) {
		serveLoginPage(w, http.StatusUnauthorized, "token not accepted for this relay")
		return
	}
	if err != nil {
		serveLoginPage(w, http.StatusServiceUnavailable, "could not verify the token right now — try again")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: clientCookie, Value: tok, Path: "/",
		MaxAge:   365 * 24 * 60 * 60,
		HttpOnly: true, Secure: secureRequest(r), SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: clientCookie, Value: "", Path: "/",
		MaxAge: -1, HttpOnly: true, Secure: secureRequest(r), SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/relay/login", http.StatusSeeOther)
}

func (s *Server) handleTunnel(w http.ResponseWriter, r *http.Request) {
	key, err := s.auth.Auth(r.Context(), bearer(r))
	if errors.Is(err, ErrReject) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if err != nil {
		// Transient — the daemon retries rather than treating it as revocation.
		http.Error(w, "relay: could not verify credential", http.StatusServiceUnavailable)
		return
	}
	ws, err := tunnelUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = nil
	cfg.Logger = discardLogger
	sess, err := yamux.Client(newWSConn(ws), cfg)
	if err != nil {
		ws.Close()
		return
	}

	// One transport per session so pooled streams die with it.
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return sess.OpenStream()
		},
	}
	t := &tunnel{sess: sess}
	t.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = tunnelHost
			pr.Out.Host = tunnelHost
			// Browsers name the relay in Origin; the daemon's upgrader
			// enforces same-origin against its own Host. Cookies pass —
			// the daemon's own login rides through the relay.
			if pr.Out.Header.Get("Origin") != "" {
				pr.Out.Header.Set("Origin", "http://"+tunnelHost)
			}
			if _, err := pr.Out.Cookie(clientCookie); err == nil {
				cs := pr.Out.Cookies()
				pr.Out.Header.Del("Cookie")
				for _, c := range cs {
					if c.Name != clientCookie {
						pr.Out.AddCookie(c)
					}
				}
			}
		},
		Transport: transport,
		ErrorLog:  discardLogger,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			http.Error(w, "relay: "+err.Error(), http.StatusBadGateway)
		},
	}

	s.mu.Lock()
	if prev := s.tunnels[key]; prev != nil {
		prev.sess.Close() // a redial replaces the previous daemon on this key
	}
	s.tunnels[key] = t
	s.mu.Unlock()

	// Hold until the session dies so the goroutine's lifetime matches the
	// tunnel's.
	<-sess.CloseChan()
	s.mu.Lock()
	if s.tunnels[key] == t {
		delete(s.tunnels, key)
	}
	s.mu.Unlock()
	ws.Close()
}

func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	key := s.router(r)
	if s.clientAuth != nil {
		err := s.clientAuth.ClientAuth(r.Context(), bearer(r), key)
		if errors.Is(err, ErrReject) {
			// A person navigating (the PWA opening on a dead cookie) gets
			// the login page; API and asset fetches get the bare status,
			// marked so the daemon's UI can tell this 401 is the relay's —
			// a daemon token prompt would be the wrong ask.
			if r.Method == http.MethodGet && strings.Contains(r.Header.Get("Accept"), "text/html") {
				http.Redirect(w, r, "/relay/login", http.StatusSeeOther)
				return
			}
			w.Header().Set("X-Muxdeck-Relay-Auth", "refused")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if err != nil {
			http.Error(w, "relay: could not verify credential", http.StatusServiceUnavailable)
			return
		}
	}
	s.mu.Lock()
	t := s.tunnels[key]
	s.mu.Unlock()
	if t == nil {
		http.Error(w, "relay: no daemon connected", http.StatusServiceUnavailable)
		return
	}
	t.proxy.ServeHTTP(w, r)
}
