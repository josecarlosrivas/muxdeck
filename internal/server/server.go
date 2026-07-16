// Package server exposes the muxdeck HTTP API and the PTY<->WebSocket bridge.
package server

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"

	"github.com/josecarlosrivas/muxdeck/internal/agent"
	"github.com/josecarlosrivas/muxdeck/internal/tmux"
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
}

func New(static fs.FS, token string, foldCase bool) *Server {
	s := &Server{mux: http.NewServeMux(), token: token, foldCase: foldCase, agents: agent.NewStore()}
	s.mux.Handle("/", http.FileServerFS(static))
	s.mux.HandleFunc("POST /api/login", s.handleLogin)
	s.mux.HandleFunc("GET /api/sessions", s.auth(s.handleList))
	s.mux.HandleFunc("POST /api/sessions", s.auth(s.handleCreate))
	s.mux.HandleFunc("DELETE /api/sessions/{name}", s.auth(s.handleKill))
	s.mux.HandleFunc("POST /api/sessions/{name}/rename", s.auth(s.handleRename))
	s.mux.HandleFunc("GET /api/sessions/{name}/mouse", s.auth(s.handleMouseGet))
	s.mux.HandleFunc("POST /api/sessions/{name}/mouse", s.auth(s.handleMouseSet))
	s.mux.HandleFunc("GET /api/sessions/{name}/attach", s.auth(s.handleAttach))
	s.mux.HandleFunc("POST /api/agent/status", s.auth(s.handleAgentStatus))
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

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
	sessions, err := tmux.List()
	if err != nil {
		writeErr(w, err)
		return
	}
	names := make([]string, len(sessions))
	for i, sess := range sessions {
		names[i] = sess.Name
	}
	s.agents.Prune(names)

	type listEntry struct {
		tmux.Session
		Agent *agent.Status `json:"agent,omitempty"`
	}
	out := make([]listEntry, len(sessions))
	for i, sess := range sessions {
		out[i] = listEntry{Session: sess}
		if st, ok := s.agents.Get(sess.Name); ok {
			out[i].Agent = &st
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleAgentStatus ingests self-reported agent state. The payload is
// display data, not commands — unknown sessions are accepted so a status
// posted during session startup isn't lost, and Prune reaps them later.
func (s *Server) handleAgentStatus(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Session string  `json:"session"`
		Agent   string  `json:"agent"`
		State   string  `json:"state"`
		Model   string  `json:"model"`
		CostUSD float64 `json:"cost_usd"`
		Note    string  `json:"note"`
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
	if len(body.Note) > 200 {
		body.Note = body.Note[:200]
	}
	// State-only posts (hooks) shouldn't blank the model/spend that the
	// statusline reported a moment earlier.
	if prev, ok := s.agents.Get(body.Session); ok && prev.Agent == body.Agent {
		if body.Model == "" {
			body.Model = prev.Model
		}
		if body.CostUSD == 0 {
			body.CostUSD = prev.CostUSD
		}
	}
	s.agents.Set(body.Session, agent.Status{
		Agent:   body.Agent,
		State:   body.State,
		Model:   body.Model,
		CostUSD: body.CostUSD,
		Note:    body.Note,
	})
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
	cmd := exec.Command("tmux", "attach-session", "-t", "="+name)
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
