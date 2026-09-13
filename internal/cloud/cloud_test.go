package cloud

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/josecarlosrivas/muxdeck/internal/remote"
)

// fakeCloud is the slice of the control plane the sync touches: the device
// token exchange, the account, and the daemon list.
type fakeCloud struct {
	mu       sync.Mutex
	token    string
	sessions int
	daemons  []map[string]any
	revoked  []string
}

func (f *fakeCloud) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/session", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Token string }
		json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		defer f.mu.Unlock()
		if body.Token != f.token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f.sessions++
		json.NewEncoder(w).Encode(map[string]string{"session": "mds_ok"})
	})
	authed := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer mds_ok" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			next(w, r)
		}
	}
	mux.HandleFunc("GET /api/account", authed(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"username": "alice"})
	}))
	mux.HandleFunc("GET /api/daemons", authed(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		json.NewEncoder(w).Encode(f.daemons)
	}))
	mux.HandleFunc("POST /api/signout", authed(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Token string }
		json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.revoked = append(f.revoked, body.Token)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	return mux
}

func daemon(name, relay string, claimed bool) map[string]any {
	d := map[string]any{"id": relay, "name": name, "relayName": relay, "claimedAt": "2026-09-01T00:00:00Z", "lastSeenAt": "2026-09-13T00:00:00Z"}
	if !claimed {
		delete(d, "claimedAt")
		delete(d, "relayName")
	}
	return d
}

func setup(t *testing.T) (*fakeCloud, *Manager, *remote.Manager) {
	t.Helper()
	f := &fakeCloud{token: "mdd_secret", daemons: []map[string]any{
		daemon("studio", "quiet-fox", true),
		daemon("Alices-MacBook-Air.local", "brave-owl", true),
		daemon("lab", "calm-elk", true),
		daemon("pending", "", false),
	}}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	rm, err := remote.Load(filepath.Join(t.TempDir(), "remotes.json"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := Load(filepath.Join(t.TempDir(), "cloud.json"), rm, "studio")
	if err != nil {
		t.Fatal(err)
	}
	m.cfg.URL = srv.URL
	m.cfg.RelayDomain = "relay.test"
	return f, m, rm
}

func names(list []remote.Status) []string {
	var out []string
	for _, r := range list {
		out = append(out, r.Name)
	}
	return out
}

func TestSignInRegistersAccountMachines(t *testing.T) {
	f, m, rm := setup(t)
	if _, err := m.SignIn("", "wrong", ""); !errors.Is(err, ErrBadToken) {
		t.Fatalf("bad token: got %v, want ErrBadToken", err)
	}
	st, err := m.SignIn("", "mdd_secret", "")
	if err != nil {
		t.Fatal(err)
	}
	if !st.SignedIn || st.State != "ok" || st.Account != "alice" {
		t.Fatalf("status after sign-in: %+v", st)
	}
	// Self stays out; the pending (unclaimed) daemon is not a machine yet.
	got := strings.Join(names(rm.List()), ",")
	if got != "alices-macbook-air,lab" {
		t.Fatalf("remotes: got %q", got)
	}
	for _, r := range rm.List() {
		if !r.Cloud || !r.HasToken || r.Mode != "url" {
			t.Fatalf("cloud remote shape: %+v", r)
		}
		if r.Name == "lab" && r.URL != "http://calm-elk.relay.test" {
			t.Fatalf("relay url: %s", r.URL)
		}
	}
	var self, air Daemon
	for _, d := range st.Daemons {
		switch d.RelayName {
		case "quiet-fox":
			self = d
		case "brave-owl":
			air = d
		}
	}
	if !self.Self || self.Remote != "" {
		t.Fatalf("self entry: %+v", self)
	}
	if air.Remote != "alices-macbook-air" {
		t.Fatalf("air entry: %+v", air)
	}
	if f.sessions != 1 {
		t.Fatalf("sessions minted: %d, want 1 (sign-in reuses its session for the sync)", f.sessions)
	}
}

func TestSyncTracksClaimsAndKeepsOff(t *testing.T) {
	f, m, rm := setup(t)
	if _, err := m.SignIn("", "mdd_secret", ""); err != nil {
		t.Fatal(err)
	}
	if err := rm.SetOff("lab", true); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.daemons = append(f.daemons[:2], daemon("lab", "calm-elk", true), daemon("attic", "swift-ram", true))
	f.mu.Unlock()
	st := m.Sync()
	if st.State != "ok" {
		t.Fatalf("sync: %+v", st)
	}
	var lab remote.Status
	for _, r := range rm.List() {
		if r.Name == "lab" {
			lab = r
		}
	}
	if lab.State != "off" {
		t.Fatalf("lab should stay off across syncs: %+v", lab)
	}
	if got := strings.Join(names(rm.List()), ","); got != "alices-macbook-air,lab,attic" {
		t.Fatalf("remotes after claim: %q", got)
	}
	if f.sessions != 1 {
		t.Fatalf("sync should reuse the cached session; minted %d", f.sessions)
	}
}

func TestManualRemoteWinsAndIsNeverTouched(t *testing.T) {
	_, m, rm := setup(t)
	if err := rm.Add(remote.Remote{Name: "lab", Mode: "ssh", Host: "lab"}); err != nil {
		t.Fatal(err)
	}
	st, err := m.SignIn("", "mdd_secret", "")
	if err != nil {
		t.Fatal(err)
	}
	var lab Daemon
	for _, d := range st.Daemons {
		if d.RelayName == "calm-elk" {
			lab = d
		}
	}
	if lab.Skipped == "" || lab.Remote != "" {
		t.Fatalf("manual name conflict should be reported: %+v", lab)
	}
	for _, r := range rm.List() {
		if r.Name == "lab" && (r.Cloud || r.Mode != "ssh") {
			t.Fatalf("manual remote overwritten: %+v", r)
		}
	}
	if err := rm.Delete("alices-macbook-air"); !errors.Is(err, remote.ErrCloudManaged) {
		t.Fatalf("deleting a cloud remote by hand: got %v", err)
	}
	if err := rm.Add(remote.Remote{Name: "alices-macbook-air", Mode: "url", URL: "http://x"}); !errors.Is(err, remote.ErrCloudManaged) {
		t.Fatalf("overwriting a cloud remote by hand: got %v", err)
	}
}

func TestRevokedTokenSignsOutAndDropsMachines(t *testing.T) {
	f, m, rm := setup(t)
	if _, err := m.SignIn("", "mdd_secret", ""); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.token = "mdd_other"
	f.mu.Unlock()
	// The cached session is still valid at the fake, so mimic the account
	// killing it along with the device token.
	m.session = "mds_stale"
	st := m.Sync()
	if st.SignedIn || st.State != "revoked" {
		t.Fatalf("after revocation: %+v", st)
	}
	if len(rm.List()) != 0 {
		t.Fatalf("cloud remotes should be gone: %v", names(rm.List()))
	}
	if m.Status().URL == "" {
		t.Fatal("the control plane url should survive a revocation")
	}
}

func TestSignOutRevokesAndClears(t *testing.T) {
	f, m, rm := setup(t)
	if _, err := m.SignIn("", "mdd_secret", ""); err != nil {
		t.Fatal(err)
	}
	if err := m.SignOut(); err != nil {
		t.Fatal(err)
	}
	if st := m.Status(); st.SignedIn || st.State != "signed_out" || len(st.Daemons) != 0 {
		t.Fatalf("after sign-out: %+v", st)
	}
	if len(rm.List()) != 0 {
		t.Fatalf("cloud remotes should be gone: %v", names(rm.List()))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.revoked) != 1 || f.revoked[0] != "mdd_secret" {
		t.Fatalf("device token not revoked at the account: %v", f.revoked)
	}
}

func TestRelayDomainDerivation(t *testing.T) {
	if d, err := relayDomain(Config{URL: "https://cloud.muxdeck.app"}); err != nil || d != "muxdeck.app" {
		t.Fatalf("hosted: %q %v", d, err)
	}
	if _, err := relayDomain(Config{URL: "http://127.0.0.1:8341"}); !errors.Is(err, ErrBadURL) {
		t.Fatalf("ip host without relay_domain: %v", err)
	}
	if d, err := relayDomain(Config{URL: "http://127.0.0.1:8341", RelayDomain: "lvh.me"}); err != nil || d != "lvh.me" {
		t.Fatalf("explicit domain: %q %v", d, err)
	}
	if got := remoteName("Alices MacBook Air.local", "x"); got != "alices-macbook-air" {
		t.Fatalf("remoteName: %q", got)
	}
	if got := remoteName("...", "brave-owl"); got != "brave-owl" {
		t.Fatalf("remoteName fallback: %q", got)
	}
}
