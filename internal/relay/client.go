package relay

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
)

// ErrRevoked is returned by Run when the relay rejects the credential.
// Revocation is a permanent disconnect, not a retry loop.
var ErrRevoked = errors.New("relay: credential rejected")

// Run dials the relay and serves handler over the tunnel until ctx ends.
// It reconnects with backoff on transport failures and returns ErrRevoked
// when the relay answers the dial with 401/403.
func Run(ctx context.Context, relayURL, key string, handler http.Handler, logf func(string, ...any)) error {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	backoff := time.Second
	for {
		err := runOnce(ctx, relayURL, key, handler)
		if err != nil && errors.Is(err, ErrRevoked) {
			logf("relay: credential rejected — tunnel stopped (re-claim to reconnect)")
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		logf("relay: tunnel down (%v); redialing in %s", err, backoff)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func runOnce(ctx context.Context, relayURL, key string, handler http.Handler) error {
	hdr := http.Header{}
	if key != "" {
		hdr.Set("Authorization", "Bearer "+key)
	}
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	ws, resp, err := dialer.DialContext(ctx, relayURL, hdr)
	if err != nil {
		if resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
			return ErrRevoked
		}
		return err
	}

	cfg := yamux.DefaultConfig()
	cfg.LogOutput = nil
	cfg.Logger = discardLogger
	// The daemon is the accepting side: the relay opens one stream per
	// proxied connection.
	sess, err := yamux.Server(newWSConn(ws), cfg)
	if err != nil {
		ws.Close()
		return err
	}
	defer sess.Close()

	stop := context.AfterFunc(ctx, func() { sess.Close() })
	defer stop()

	srv := &http.Server{Handler: handler}
	err = srv.Serve(sess) // returns when the session closes
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}
