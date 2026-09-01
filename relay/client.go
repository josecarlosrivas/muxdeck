package relay

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
)

// ErrRevoked is returned when the relay rejects the credential.
// Revocation is a permanent disconnect, not a retry loop; the Manager owns
// the redial policy around this.
var ErrRevoked = errors.New("relay: credential rejected")

func runOnce(ctx context.Context, relayURL, key string, handler http.Handler, onUp func()) error {
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
	if onUp != nil {
		onUp()
	}

	srv := &http.Server{Handler: handler}
	err = srv.Serve(sess) // returns when the session closes
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}
