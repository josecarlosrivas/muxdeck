package relay

import (
	"context"
	"crypto/subtle"
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

// Server is the self-hosted relay: one daemon dials /tunnel with the shared
// secret, browsers hit everything else and are proxied down the tunnel.
// Single-tenant by design — your hostname, your daemon, your accounts.
type Server struct {
	secret string

	mu    sync.Mutex
	sess  *yamux.Session
	proxy *httputil.ReverseProxy
}

// NewServer returns a relay that accepts a daemon presenting secret.
func NewServer(secret string) *Server { return &Server{secret: secret} }

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
	if s.secret == "" || subtle.ConstantTimeCompare([]byte(bearer(r)), []byte(s.secret)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
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
	proxy := &httputil.ReverseProxy{
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
	if s.sess != nil {
		s.sess.Close() // a redial replaces the previous daemon
	}
	s.sess, s.proxy = sess, proxy
	s.mu.Unlock()

	// Hold until the session dies so the connection's goroutine lifetime
	// matches the tunnel's.
	<-sess.CloseChan()
	s.mu.Lock()
	if s.sess == sess {
		s.sess, s.proxy = nil, nil
	}
	s.mu.Unlock()
	ws.Close()
}

func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	proxy := s.proxy
	s.mu.Unlock()
	if proxy == nil {
		http.Error(w, "relay: no daemon connected", http.StatusServiceUnavailable)
		return
	}
	proxy.ServeHTTP(w, r)
}
