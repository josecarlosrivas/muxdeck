package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/josecarlosrivas/muxdeck/internal/agent"
)

// newTestServer builds a server for the status endpoint alone: it touches
// neither the static FS nor the remote and mush managers.
func newTestServer(t *testing.T) *Server {
	t.Helper()
	return New(nil, "", false, nil, nil)
}

func postStatus(t *testing.T, s *Server, body string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/agent/status", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec.Code
}

func getStatus(t *testing.T, s *Server, session string) agent.Status {
	t.Helper()
	st, ok := s.agents.Get(session)
	if !ok {
		t.Fatalf("no status stored for %q", session)
	}
	return st
}

// The two reporters for one session know different things: a statusline knows
// the model, the spend and how far along the work is; a hook knows only that
// the state changed. A hook post must not blank what the statusline said.
func TestAgentStatusCarriesForwardOmittedFields(t *testing.T) {
	s := newTestServer(t)
	if code := postStatus(t, s, `{"session":"build","agent":"demo","state":"working",
		"model":"M","cost_usd":1.5,
		"progress":{"value":0.5,"label":"tests"},
		"chips":[{"key":"tests","value":"6/12","icon":"flask"}]}`); code != http.StatusNoContent {
		t.Fatalf("first post: got %d", code)
	}
	if code := postStatus(t, s, `{"session":"build","agent":"demo","state":"waiting"}`); code != http.StatusNoContent {
		t.Fatalf("second post: got %d", code)
	}

	got := getStatus(t, s, "build")
	if got.State != "waiting" {
		t.Errorf("state: got %q, want %q", got.State, "waiting")
	}
	if got.Model != "M" || got.CostUSD != 1.5 {
		t.Errorf("model/cost: got %q %v, want M 1.5", got.Model, got.CostUSD)
	}
	if got.Progress == nil || got.Progress.Value != 0.5 || got.Progress.Label != "tests" {
		t.Errorf("progress: got %#v, want {0.5 tests}", got.Progress)
	}
	want := []agent.Chip{{Key: "tests", Value: "6/12", Icon: "flask"}}
	if !reflect.DeepEqual(got.Chips, want) {
		t.Errorf("chips: got %#v, want %#v", got.Chips, want)
	}
}

// Carrying forward is only safe within one reporter: a different agent taking
// over the session says nothing about the last one's numbers.
func TestAgentStatusDoesNotCarryAcrossAgents(t *testing.T) {
	s := newTestServer(t)
	postStatus(t, s, `{"session":"build","agent":"first","state":"working","model":"M","cost_usd":2,
		"chips":[{"key":"k","value":"v"}]}`)
	postStatus(t, s, `{"session":"build","agent":"second","state":"idle"}`)

	got := getStatus(t, s, "build")
	if got.Agent != "second" || got.Model != "" || got.CostUSD != 0 || got.Chips != nil || got.Progress != nil {
		t.Errorf("got %#v, want a status with nothing carried over", got)
	}
}

// Absent means unchanged, so saying "nothing in progress" has to be possible:
// an empty progress object and an empty chip list are how it is said.
func TestAgentStatusExplicitClear(t *testing.T) {
	s := newTestServer(t)
	postStatus(t, s, `{"session":"build","agent":"demo","state":"working",
		"progress":{"value":0.9,"label":"deploy"},"chips":[{"key":"k","value":"v"}]}`)
	postStatus(t, s, `{"session":"build","agent":"demo","state":"idle","progress":{},"chips":[]}`)

	got := getStatus(t, s, "build")
	if got.Progress == nil || got.Progress.Value != 0 || got.Progress.Label != "" {
		t.Errorf("progress: got %#v, want a zero object", got.Progress)
	}
	if len(got.Chips) != 0 {
		t.Errorf("chips: got %#v, want none", got.Chips)
	}
}

func TestAgentStatusNormalizesOnIngest(t *testing.T) {
	s := newTestServer(t)
	postStatus(t, s, `{"session":"build","agent":"demo","state":"working",
		"progress":{"value":4.2},"chips":[{"key":"k","value":"v","icon":"nope","color":"red"}]}`)

	got := getStatus(t, s, "build")
	if got.Progress.Value != 1 {
		t.Errorf("progress value: got %v, want 1", got.Progress.Value)
	}
	if got.Chips[0].Icon != "" || got.Chips[0].Color != "" {
		t.Errorf("chip: got %#v, want icon and color dropped", got.Chips[0])
	}
}

func TestAgentStatusRejects(t *testing.T) {
	s := newTestServer(t)
	for name, tc := range map[string]struct {
		body string
		want int
	}{
		"no session":   {`{"agent":"demo","state":"idle"}`, http.StatusBadRequest},
		"no agent":     {`{"session":"build","state":"idle"}`, http.StatusBadRequest},
		"bad name":     {`{"session":"has space","agent":"demo","state":"idle"}`, http.StatusBadRequest},
		"bad state":    {`{"session":"build","agent":"demo","state":"pondering"}`, http.StatusBadRequest},
		"not json":     {`{`, http.StatusBadRequest},
		"well formed":  {`{"session":"build","agent":"demo","state":"idle"}`, http.StatusNoContent},
		"unknown keys": {`{"session":"build","agent":"demo","state":"idle","future":1}`, http.StatusNoContent},
	} {
		if got := postStatus(t, s, tc.body); got != tc.want {
			t.Errorf("%s: got %d, want %d", name, got, tc.want)
		}
	}
}

// The status a session list hands back is the one the client renders, so the
// wire shape is part of the contract.
func TestStatusJSONOmitsAbsentFields(t *testing.T) {
	out, err := json.Marshal(agent.Status{Agent: "demo", State: "idle"})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"progress", "chips", "model", "cost_usd", "note"} {
		if strings.Contains(string(out), key) {
			t.Errorf("got %s, want %q omitted", out, key)
		}
	}
}
