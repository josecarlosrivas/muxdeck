package relay

import (
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
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
	default:
		s.handleProxy(w, r)
	}
}

func bearer(r *http.Request) string {
	if h := r.Header.Get("Authorization"); len(h) > 7 && h[:7] == "Bearer " {
		return h[7:]
	}
	return ""
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
