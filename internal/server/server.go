// Package server exposes the muxdeck HTTP API and the PTY<->WebSocket bridge.
package server

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"

	"github.com/josecarlosrivas/muxdeck/internal/agent"
	"github.com/josecarlosrivas/muxdeck/internal/mushrun"
	"github.com/josecarlosrivas/muxdeck/internal/remote"
	"github.com/josecarlosrivas/muxdeck/internal/tcc"
	"github.com/josecarlosrivas/muxdeck/internal/tmux"
	"github.com/josecarlosrivas/muxdeck/relay"
)

const tokenCookie = "muxdeck_token"

func init() {
	// Not in Go's built-in MIME table; browsers want the real type for PWA installability.
	mime.AddExtensionType(".webmanifest", "application/manifest+json")
}

type Server struct {
	mux      *http.ServeMux
	token    string
	foldCase bool // generated codes are matched case-insensitively, GitHub-device-auth style
	agents   *agent.Store
	repos    *repoCache
	ports    *portCache
	remotes  *remote.Manager
	mushruns *mushrun.Manager
	relaym   *relay.Manager
}

func New(static fs.FS, token string, foldCase bool, remotes *remote.Manager, mushruns *mushrun.Manager, relaym *relay.Manager) *Server {
	s := &Server{mux: http.NewServeMux(), token: token, foldCase: foldCase, agents: agent.NewStore(), repos: newRepoCache(), ports: newPortCache(), remotes: remotes, mushruns: mushruns, relaym: relaym}
	s.mux.Handle("/", http.FileServerFS(static))
	s.mux.HandleFunc("POST /api/login", s.handleLogin)
	s.mux.HandleFunc("GET /api/sessions", s.auth(s.handleList))
	s.mux.HandleFunc("POST /api/sessions", s.auth(s.handleCreate))
	s.mux.HandleFunc("DELETE /api/sessions/{name}", s.auth(s.handleKill))
	s.mux.HandleFunc("POST /api/sessions/{name}/rename", s.auth(s.handleRename))
	s.mux.HandleFunc("GET /api/sessions/{name}/mouse", s.auth(s.handleMouseGet))
	s.mux.HandleFunc("POST /api/sessions/{name}/mouse", s.auth(s.handleMouseSet))
	s.mux.HandleFunc("GET /api/sessions/{name}/attach", s.auth(s.handleAttach))
	s.mux.HandleFunc("POST /api/sessions/{name}/send", s.auth(s.handleSend))
	s.mux.HandleFunc("GET /api/sessions/{name}/diff", s.auth(s.handleDiff))
	s.mux.HandleFunc("GET /api/sessions/{name}/files", s.auth(s.handleFiles))
	s.mux.HandleFunc("GET /api/sessions/{name}/file", s.auth(s.handleFile))
	s.mux.HandleFunc("POST /api/agent/status", s.auth(s.handleAgentStatus))
	s.mux.HandleFunc("GET /api/doctor", s.auth(s.handleDoctor))
	s.mux.HandleFunc("GET /api/mush/runs", s.auth(s.handleMushList))
	s.mux.HandleFunc("POST /api/mush/runs", s.auth(s.handleMushStart))
	s.mux.HandleFunc("GET /api/mush/runs/{id}/stream", s.auth(s.handleMushStream))
	s.mux.HandleFunc("DELETE /api/mush/runs/{id}", s.auth(s.handleMushStop))
	s.mux.HandleFunc("POST /api/mush/runs/{id}/retry", s.auth(s.handleMushRetry))
	s.mux.HandleFunc("POST /api/mush/runs/{id}/resume", s.auth(s.handleMushResume))
	s.mux.HandleFunc("GET /api/relay", s.auth(s.handleRelayStatus))
	s.mux.HandleFunc("POST /api/relay", s.auth(s.handleRelaySet))
	s.mux.HandleFunc("GET /api/remotes", s.auth(s.handleRemoteList))
	s.mux.HandleFunc("POST /api/remotes", s.auth(s.handleRemoteAdd))
	s.mux.HandleFunc("DELETE /api/remotes/{name}", s.auth(s.handleRemoteDelete))
	s.mux.HandleFunc("PATCH /api/remotes/{name}", s.auth(s.handleRemotePatch))
	s.mux.HandleFunc("/api/remotes/{name}/proxy/{rest...}", s.auth(s.handleRemoteProxy))
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	s.mux.ServeHTTP(rec, r)
	remoteAddr := r.Header.Get("X-Forwarded-For")
	if remoteAddr == "" {
		remoteAddr = r.RemoteAddr
	}
	log.Printf("%s %s %d %s %s", r.Method, r.URL.Path, rec.status,
		time.Since(start).Round(time.Millisecond), remoteAddr)
}

// statusRecorder captures the response status for the access log while
// passing Hijacker/Flusher through — the WebSocket attach path needs both.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("underlying ResponseWriter is not a Hijacker")
	}
	return h.Hijack()
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// --- doctor: macOS folder-privacy (TCC) diagnostics ---

// handleDoctor reports TCC state. Passive by default — the UI polls it for
// its banner — because an unsolicited probe can raise consent prompts on a
// screen nobody is watching (see internal/tcc). ?probe is the deliberate
// form behind `muxdeck doctor`.
func (s *Server) handleDoctor(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, tcc.Status(r.URL.Query().Has("probe")))
}

// --- remotes ---

func (s *Server) handleRemoteList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.remotes.List())
}

func (s *Server) handleRemoteAdd(w http.ResponseWriter, r *http.Request) {
	var body remote.Remote
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.remotes.Add(body); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, remote.ErrBadRemote) {
			status = http.StatusBadRequest
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRemoteDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.remotes.Delete(r.PathValue("name")); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRemotePatch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Off *bool `json:"off"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Off == nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.remotes.SetOff(r.PathValue("name"), *body.Off); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRemoteProxy(w http.ResponseWriter, r *http.Request) {
	s.remotes.Proxy(r.PathValue("name"), r.PathValue("rest"), w, r)
}

// --- auth ---

func (s *Server) authorized(r *http.Request) bool {
	if s.token == "" {
		return true
	}
	candidate := ""
	if c, err := r.Cookie(tokenCookie); err == nil {
		candidate = c.Value
	} else if h := r.Header.Get("Authorization"); len(h) > 7 && h[:7] == "Bearer " {
		candidate = h[7:]
	}
	return s.tokenMatch(candidate)
}

func (s *Server) tokenMatch(candidate string) bool {
	if s.foldCase {
		candidate = strings.ToUpper(candidate)
	}
	return subtle.ConstantTimeCompare([]byte(candidate), []byte(s.token)) == 1
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.token == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !s.tokenMatch(body.Token) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	// Store the canonical token so cookie auth is unaffected by typed case.
	http.SetCookie(w, &http.Cookie{
		Name:     tokenCookie,
		Value:    s.token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	w.WriteHeader(http.StatusNoContent)
}

// --- session CRUD ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	if errors.Is(err, tmux.ErrBadName) {
		status = http.StatusBadRequest
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	if !tmux.Found {
		// A machine state, not a request error — the UI turns this into an
		// install hint instead of failing silently.
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "tmux-missing"})
		return
	}
	sessions, err := tmux.List()
	if err != nil {
		writeErr(w, err)
		return
	}
	names := make([]string, len(sessions))
	cwds := make([]string, 0, len(sessions))
	for i, sess := range sessions {
		names[i] = sess.Name
		if sess.Path != "" {
			cwds = append(cwds, sess.Path)
		}
	}
	s.agents.Prune(names)
	s.repos.prune(cwds)
	listening := s.ports.state()

	type listEntry struct {
		tmux.Session
		repoState
		// Ports the session's process tree is listening on, resolved out of
		// band; absent until the first refresh lands.
		Ports []int         `json:"ports,omitempty"`
		Agent *agent.Status `json:"agent,omitempty"`
	}
	out := make([]listEntry, len(sessions))
	for i, sess := range sessions {
		out[i] = listEntry{Session: sess, repoState: s.repos.state(sess.Path), Ports: listening[sess.Name]}
		if st, ok := s.agents.Get(sess.Name); ok {
			out[i].Agent = &st
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleAgentStatus ingests self-reported agent state. The payload is
// display data, not commands — unknown sessions are accepted so a status
// posted during session startup isn't lost, and Prune reaps them later.
//
// The status is decoded by embedding rather than copied field by field, so a
// field added to agent.Status reaches the API by existing.
func (s *Server) handleAgentStatus(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Session string `json:"session"`
		agent.Status
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Session == "" || body.Agent == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !tmux.ValidName(body.Session) {
		writeErr(w, tmux.ErrBadName)
		return
	}
	if !agent.ValidState(body.State) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": agent.ErrBadState.Error()})
		return
	}
	body.Status.Normalize()
	// One session is reported by more than one caller — a statusline that
	// knows the model and the spend, hooks that know only the transition —
	// so a field the caller left out keeps what the last one said rather than
	// blanking it. Saying so explicitly is what an empty value is for: a zero
	// progress object, or an empty chip list.
	if prev, ok := s.agents.Get(body.Session); ok && prev.Agent == body.Agent {
		if body.Model == "" {
			body.Model = prev.Model
		}
		if body.CostUSD == 0 {
			body.CostUSD = prev.CostUSD
		}
		if body.Progress == nil {
			body.Progress = prev.Progress
		}
		if body.Chips == nil {
			body.Chips = prev.Chips
		}
	}
	s.agents.Set(body.Session, body.Status)
	w.WriteHeader(http.StatusNoContent)
}

// handleSend types text into a session — what the browser does over the
// attach socket, without attaching. A one-shot client would resize the
// session to its own dimensions for as long as it stayed, which is not
// something a scripted send should do to a layout somebody is looking at.
//
// This is remote code execution into a shell, deliberately: it is the power
// the attach WebSocket has had since the first commit, behind the same auth.
// enter defaults to true — a send that has to be submitted separately is a
// send that half the callers will forget to submit.
func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text  string `json:"text"`
		Enter *bool  `json:"enter"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	name := r.PathValue("name")
	if !tmux.Has(name) {
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}
	if err := tmux.SendKeys(name, body.Text, body.Enter == nil || *body.Enter); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := tmux.New(body.Name); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"name": body.Name})
}

func (s *Server) handleKill(w http.ResponseWriter, r *http.Request) {
	if err := tmux.Kill(r.PathValue("name")); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRename(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := tmux.Rename(r.PathValue("name"), body.Name); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMouseGet(w http.ResponseWriter, r *http.Request) {
	on, err := tmux.Mouse(r.PathValue("name"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"enabled": on})
}

func (s *Server) handleMouseSet(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := tmux.SetMouse(r.PathValue("name"), body.Enabled); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- attach: PTY <-> WebSocket bridge ---

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	// Same-host origin check: a browser terminal is remote code execution
	// by design, so cross-site WebSocket hijacking must be rejected.
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true // non-browser client
		}
		u, err := url.Parse(origin)
		return err == nil && u.Host == r.Host
	},
}

type controlMsg struct {
	Type string `json:"type"`
	Data string `json:"data"`
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

// --- viewers: git diff + file reads, anchored to a session's cwd ---

// sessionCwd resolves the session's active-pane directory or writes the error.
func sessionCwd(w http.ResponseWriter, r *http.Request) (string, bool) {
	name := r.PathValue("name")
	if !tmux.Has(name) {
		http.Error(w, "no such session", http.StatusNotFound)
		return "", false
	}
	cwd, err := tmux.Cwd(name)
	if err != nil {
		writeErr(w, err)
		return "", false
	}
	return cwd, true
}

func git(cwd string, args ...string) (string, error) {
	return gitContext(context.Background(), cwd, args...)
}

func gitContext(ctx context.Context, cwd string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", cwd}, args...)...)
	// Killing git on a cancelled context does not by itself unblock the read:
	// anything git spawned that outlives it — a credential helper, a hook —
	// inherits the output pipe and holds it open. WaitDelay forces the pipe
	// shut shortly after, so a deadline is one the caller can rely on.
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return "", errors.New(msg)
	}
	return string(out), nil
}

const maxViewBytes = 2 << 20

func (s *Server) handleDiff(w http.ResponseWriter, r *http.Request) {
	cwd, ok := sessionCwd(w, r)
	if !ok {
		return
	}
	root, err := git(cwd, "rev-parse", "--show-toplevel")
	if err != nil {
		tcc.Note(cwd, err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "not a git repository: " + cwd})
		return
	}
	// Working tree vs HEAD, the whole repo. --no-ext-diff keeps any
	// configured external diff tool from hijacking the output.
	text, err := git(cwd, "diff", "HEAD", "--no-color", "--no-ext-diff")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if len(text) > maxViewBytes {
		text = text[:maxViewBytes] + "\n… (diff truncated)\n"
	}
	writeJSON(w, http.StatusOK, map[string]string{"root": strings.TrimSpace(root), "text": text})
}

func (s *Server) handleFiles(w http.ResponseWriter, r *http.Request) {
	cwd, ok := sessionCwd(w, r)
	if !ok {
		return
	}
	// Tracked + untracked-but-not-ignored markdown, relative to the session
	// cwd (ls-files scopes to the subtree it runs in). Outside a repo, fall
	// back to the directory's own *.md entries.
	var files []string
	if out, err := git(cwd, "ls-files", "-co", "--exclude-standard", "--", "*.md", "*.markdown"); err == nil {
		files = strings.Fields(strings.TrimSpace(out))
	} else {
		tcc.Note(cwd, err)
		matches, _ := filepath.Glob(filepath.Join(cwd, "*.md"))
		for _, m := range matches {
			files = append(files, filepath.Base(m))
		}
	}
	if files == nil {
		files = []string{}
	}
	if len(files) > 500 {
		files = files[:500]
	}
	writeJSON(w, http.StatusOK, map[string]any{"cwd": cwd, "files": files})
}

func (s *Server) handleFile(w http.ResponseWriter, r *http.Request) {
	cwd, ok := sessionCwd(w, r)
	if !ok {
		return
	}
	rel := r.URL.Query().Get("path")
	clean := filepath.Clean(rel)
	if rel == "" || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	full := filepath.Join(cwd, clean)
	info, err := os.Stat(full)
	if err != nil || info.IsDir() {
		tcc.Note(cwd, err)
		http.Error(w, "no such file", http.StatusNotFound)
		return
	}
	if info.Size() > maxViewBytes {
		http.Error(w, "file too large", http.StatusRequestEntityTooLarge)
		return
	}
	data, err := os.ReadFile(full)
	if err != nil {
		tcc.Note(cwd, err)
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": clean, "mtime": info.ModTime().UnixMilli(), "text": string(data)})
}

func (s *Server) handleAttach(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !tmux.Has(name) {
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}
	if err := tmux.EnsureClipboard(); err != nil {
		log.Printf("attach %s: clipboard setup: %v", name, err)
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	// Each websocket gets its own tmux client attached to the session, so
	// multiple browsers can view the same session just like multiple terminals.
	// -u forces UTF-8: daemons launched by launchd/systemd/GUI apps carry no
	// LANG/LC_*, and tmux replaces every non-ASCII glyph with "_" for a
	// locale-less client.
	cmd := exec.Command(tmux.Bin, "-u", "attach-session", "-t", "="+name)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	ptmx, err := pty.Start(cmd)
	if err != nil {
		log.Printf("attach %s: pty: %v", name, err)
		conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","data":"failed to start tmux client"}`))
		return
	}
	defer func() {
		ptmx.Close()
		cmd.Process.Kill()
		cmd.Wait()
	}()

	// pty -> ws. Closing conn on exit unblocks the read loop below when the
	// tmux client ends (session killed, server exit).
	go func() {
		defer conn.Close()
		buf := make([]byte, 32*1024)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				if werr := conn.WriteMessage(websocket.BinaryMessage, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// ws -> pty
	for {
		mt, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if mt != websocket.TextMessage {
			continue
		}
		var msg controlMsg
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		switch msg.Type {
		case "input":
			io.WriteString(ptmx, msg.Data)
		case "resize":
			if msg.Cols > 0 && msg.Rows > 0 {
				pty.Setsize(ptmx, &pty.Winsize{Cols: msg.Cols, Rows: msg.Rows})
			}
		}
	}
}

// --- relay tunnel ---

func (s *Server) handleRelayStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.relaym.Status())
}

// handleRelaySet applies tunnel configuration. A body with a url (re)configures
// and redials; {"off":true|false} alone toggles the existing config — off keeps
// the credential, and on is also the re-arm after a rejection.
func (s *Server) handleRelaySet(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL *string `json:"url"`
		Key *string `json:"key"`
		Off *bool   `json:"off"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	// Enabling the tunnel on an authless daemon would publish an
	// unauthenticated terminal; only off:true is allowed then.
	enabling := body.URL != nil || (body.Off != nil && !*body.Off)
	if enabling && s.token == "" {
		http.Error(w, "the daemon has no access token, and the relay would expose it publicly — restart with -token auto (or MUXDECK_TOKEN=auto), then retry", http.StatusBadRequest)
		return
	}
	var err error
	switch {
	case body.URL != nil:
		cfg := relay.Config{URL: *body.URL}
		if body.Key != nil {
			cfg.Key = *body.Key
		}
		if body.Off != nil {
			cfg.Off = *body.Off
		}
		err = s.relaym.Set(cfg)
	case body.Off != nil:
		err = s.relaym.SetOff(*body.Off)
	default:
		http.Error(w, "nothing to set", http.StatusBadRequest)
		return
	}
	if errors.Is(err, relay.ErrBadConfig) {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, s.relaym.Status())
}
