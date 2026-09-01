package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/josecarlosrivas/muxdeck/relay"
)

// An authless daemon must refuse to enable the tunnel: the relay would
// publish an unauthenticated terminal.
func TestRelaySetRequiresToken(t *testing.T) {
	newSrv := func(token string) *Server {
		m, err := relay.LoadManager(filepath.Join(t.TempDir(), "relay.json"))
		if err != nil {
			t.Fatal(err)
		}
		return New(nil, token, false, nil, nil, m)
	}

	post := func(s *Server, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/api/relay", strings.NewReader(body))
		w := httptest.NewRecorder()
		s.ServeHTTP(w, req)
		return w
	}

	authless := newSrv("")
	if w := post(authless, `{"url":"wss://r.example/tunnel","key":"k"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("authless enable: got %d, want 400", w.Code)
	}
	if w := post(authless, `{"off":false}`); w.Code != http.StatusBadRequest {
		t.Fatalf("authless re-arm: got %d, want 400", w.Code)
	}
	if w := post(authless, `{"off":true}`); w.Code != http.StatusOK {
		t.Fatalf("authless off must stay allowed: got %d", w.Code)
	}

	secured := newSrv("tok")
	req := httptest.NewRequest("POST", "/api/relay", strings.NewReader(`{"url":"wss://r.example/tunnel","key":"k"}`))
	req.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	secured.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("secured enable: got %d: %s", w.Code, w.Body.String())
	}
}
