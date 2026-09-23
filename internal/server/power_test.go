package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/josecarlosrivas/muxdeck/internal/power"
)

func powerCall(t *testing.T, s *Server, method, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, "/api/power", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestPowerAPIUnsupportedWithoutBackend(t *testing.T) {
	s := New(nil, "", false, nil, nil, nil, nil, nil)
	code, st := powerCall(t, s, http.MethodGet, "")
	if code != 200 || st["state"] != "unsupported" {
		t.Fatalf("status: %d %v", code, st)
	}
	if code, st := powerCall(t, s, http.MethodPost, `{"keep_awake_while_viewing":true}`); code != 400 || !strings.Contains(st["error"].(string), "macOS") {
		t.Fatalf("enable unsupported: %d %v", code, st)
	}
	if code, _ := powerCall(t, s, http.MethodPost, `{"keep_awake_while_viewing":false}`); code != 200 {
		t.Fatalf("disable unsupported: %d", code)
	}
}

type stubBackend struct{}

func (stubBackend) Name() string                    { return "stub" }
func (stubBackend) PowerSource() string             { return "ac" }
func (stubBackend) Start() (power.Assertion, error) { return stubAssertion{make(chan struct{})}, nil }

type stubAssertion struct{ done chan struct{} }

func (a stubAssertion) Done() <-chan struct{} { return a.done }
func (a stubAssertion) Stop()                 { close(a.done) }

func TestPowerAPISetsAndValidates(t *testing.T) {
	m, err := power.Load(filepath.Join(t.TempDir(), "power.json"), stubBackend{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := New(nil, "", false, nil, nil, nil, nil, m)
	if code, _ := powerCall(t, s, http.MethodPost, `{}`); code != 400 {
		t.Fatalf("missing field: %d", code)
	}
	if code, _ := powerCall(t, s, http.MethodPost, `{"keep_awake_while_viewing":"yes"}`); code != 400 {
		t.Fatalf("wrong type: %d", code)
	}
	code, st := powerCall(t, s, http.MethodPost, `{"keep_awake_while_viewing":true}`)
	if code != 200 || st["enabled"] != true || st["state"] != "waiting" || st["supported"] != true {
		t.Fatalf("enable: %d %v", code, st)
	}
	m.Acquire("viewer")
	if _, st := powerCall(t, s, http.MethodGet, ""); st["state"] != "keeping_awake" || st["viewers"] != float64(1) || st["backend"] != "stub" {
		t.Fatalf("with viewer: %v", st)
	}
	if code, st := powerCall(t, s, http.MethodPost, `{"keep_awake_while_viewing":false}`); code != 200 || st["state"] != "off" || st["asserting"] != false {
		t.Fatalf("disable: %d %v", code, st)
	}
}
