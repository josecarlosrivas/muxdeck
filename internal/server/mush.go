package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gorilla/websocket"
	"github.com/josecarlosrivas/muxdeck/internal/mushrun"
	"github.com/josecarlosrivas/muxdeck/internal/tmux"
)

// --- mush runs: muxdeck as a viewer and starter over mush's ledger ---

func (s *Server) handleMushList(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rows, err := s.mushruns.List(r.URL.Query().Get("project"), limit)
	resp := map[string]any{
		"available": s.mushruns.Available(),
		"models":    s.mushruns.Models(),
		"serve":     s.mushruns.Serve(),
		"runs":      rows,
	}
	if err != nil {
		resp["runs"] = []mushrun.Row{}
		resp["error"] = err.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleMushStart starts a run. The working directory comes from the named
// session's active pane (same resolution as the diff viewer), so ":mush" acts
// on whatever repo the focused terminal is in — including through the remotes
// proxy, where the request lands on the daemon that owns the session.
func (s *Server) handleMushStart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Task    string `json:"task"`
		Session string `json:"session"`
		Dir     string `json:"dir"`
		Model   string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Task == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	dir := body.Dir
	if dir == "" && body.Session != "" {
		var err error
		if dir, err = tmux.Cwd(body.Session); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	}
	if dir == "" {
		http.Error(w, "need session or dir", http.StatusBadRequest)
		return
	}
	row, err := s.mushruns.Start(body.Task, dir, body.Model, "")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, row)
}

func (s *Server) mushRow(w http.ResponseWriter, r *http.Request) (mushrun.Row, bool) {
	row, err := s.mushruns.Get(r.PathValue("id"))
	if err != nil {
		code := http.StatusBadGateway
		if mushrun.IsNotFound(err) {
			code = http.StatusNotFound
		}
		writeJSON(w, code, map[string]string{"error": err.Error()})
		return row, false
	}
	return row, true
}

// handleMushStream bridges a run to a websocket: the ledger row first
// (`_run`), then the journal replayed from the top, then tailed while the run
// is live, with `_state` frames on every ledger transition. Incoming messages
// are protocol commands: approval_response is answered over whichever
// transport owns the run, interrupt and user_turn reach only engines this
// daemon spawned.
func (s *Server) handleMushStream(w http.ResponseWriter, r *http.Request) {
	row, ok := s.mushRow(w, r)
	if !ok {
		return
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go keepAlive(conn, ctx.Done())

	send := func(v any) bool {
		b, _ := json.Marshal(v)
		return conn.WriteMessage(websocket.TextMessage, b) == nil
	}
	sendRaw := func(b []byte) bool { return conn.WriteMessage(websocket.TextMessage, b) == nil }

	go func() {
		defer cancel()
		if !send(map[string]any{"type": "_run", "data": row}) {
			return
		}
		var offset int64
		state := row.State
		ledgerTick := time.NewTicker(2 * time.Second)
		defer ledgerTick.Stop()
		journalTick := time.NewTicker(400 * time.Millisecond)
		defer journalTick.Stop()
		terminal := row.Terminal()
		quiet := 0
		for {
			frames, next, err := s.mushruns.Journal(row, offset)
			if err != nil {
				send(map[string]any{"type": "_error", "error": "journal: " + err.Error()})
				return
			}
			offset = next
			for _, f := range frames {
				if !sendRaw(f) {
					return
				}
			}
			// A terminal row's journal may still be flushing; drain a few
			// quiet ticks before closing the stream.
			if terminal {
				if len(frames) == 0 {
					quiet++
				}
				if quiet >= 3 {
					send(map[string]any{"type": "_exit", "data": map[string]string{"state": state, "error": row.Error}})
					return
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-journalTick.C:
			case <-ledgerTick.C:
				fresh, err := s.mushruns.Get(row.ID)
				if err != nil {
					continue
				}
				if fresh.Journal != "" {
					row.Journal, row.Project = fresh.Journal, fresh.Project
				}
				if fresh.State != state || fresh.PRURL != row.PRURL || fresh.Steps != row.Steps {
					row = fresh
					state = fresh.State
					if !send(map[string]any{"type": "_state", "data": fresh}) {
						return
					}
				}
				terminal = fresh.Terminal()
			}
		}
	}()

	for {
		mt, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if mt != websocket.TextMessage {
			continue
		}
		if err := s.mushCommand(row.ID, data); err != nil {
			send(map[string]string{"type": "_error", "error": err.Error()})
		}
	}
}

func (s *Server) mushCommand(id string, frame []byte) error {
	var env struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(frame, &env); err != nil {
		return err
	}
	switch env.Type {
	case "approval_response":
		var d struct {
			Approved bool `json:"approved"`
		}
		if err := json.Unmarshal(env.Data, &d); err != nil {
			return err
		}
		return s.mushruns.Approve(id, d.Approved)
	case "interrupt":
		return s.mushruns.Interrupt(id)
	}
	return errors.New("not a protocol command: " + env.Type)
}

func (s *Server) handleMushStop(w http.ResponseWriter, r *http.Request) {
	if err := s.mushruns.Interrupt(r.PathValue("id")); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMushRetry(w http.ResponseWriter, r *http.Request) {
	row, ok := s.mushRow(w, r)
	if !ok {
		return
	}
	child, err := s.mushruns.Retry(row)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, child)
}

func (s *Server) handleMushResume(w http.ResponseWriter, r *http.Request) {
	row, ok := s.mushRow(w, r)
	if !ok {
		return
	}
	if err := s.mushruns.Resume(row); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
