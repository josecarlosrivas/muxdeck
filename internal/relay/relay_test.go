package relay

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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

func TestProxyThroughTunnel(t *testing.T) {
	ts, wsURL := startRelay(t, "sekrit")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, wsURL, "sekrit", daemonHandler(), t.Logf)

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

	// Tunnel down: the relay answers 503, not a hang.
	cancel()
	waitFor(t, ts.URL+"/echo", http.StatusServiceUnavailable)
}

func TestDialRejectedIsPermanent(t *testing.T) {
	_, wsURL := startRelay(t, "sekrit")
	err := Run(context.Background(), wsURL, "wrong", daemonHandler(), t.Logf)
	if !errors.Is(err, ErrRevoked) {
		t.Fatalf("want ErrRevoked, got %v", err)
	}
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
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	go Run(ctx1, wsURL, "sekrit", daemonHandler(), t.Logf)
	waitFor(t, ts.URL+"/echo", http.StatusOK)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /echo", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "second")
	})
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go Run(ctx2, wsURL, "sekrit", mux, t.Logf)

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
