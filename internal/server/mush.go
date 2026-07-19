package server

import (
	"encoding/json"
	"net/http"

	"github.com/gorilla/websocket"
	"github.com/josecarlosrivas/muxdeck/internal/tmux"
)

// --- mush runs: muxdeck as a protocol client of a mush engine ---

func (s *Server) handleMushList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"available": s.mushruns.Available(),
		"runs":      s.mushruns.List(),
	})
}

// handleMushStart spawns a run. The working directory comes from the named
// session's active pane (same resolution as the diff viewer), so ":mush" acts
// on whatever repo the focused terminal is in — including through the remotes
// proxy, where the request lands on the daemon that owns the session.
func (s *Server) handleMushStart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Task    string `json:"task"`
		Session string `json:"session"`
		Dir     string `json:"dir"`
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
	run, err := s.mushruns.Start(body.Task, dir)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, run.Info())
}

// handleMushStream bridges a run to a websocket: replay first, then live
// events; incoming messages are protocol commands (approval_response,
// user_turn, interrupt) validated and forwarded to the engine.
func (s *Server) handleMushStream(w http.ResponseWriter, r *http.Request) {
	run, ok := s.mushruns.Get(r.PathValue("id"))
	if !ok {
		http.Error(w, "no such run", http.StatusNotFound)
		return
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	replay, live, trimmed := run.Subscribe()
	if trimmed {
		note, _ := json.Marshal(map[string]any{"type": "_trimmed"})
		conn.WriteMessage(websocket.TextMessage, note)
	}
	for _, frame := range replay {
		if conn.WriteMessage(websocket.TextMessage, frame) != nil {
			if live != nil {
				run.Unsubscribe(live)
			}
			return
		}
	}
	if live == nil {
		// Terminal run: replay was the whole story. Hold the socket open for
		// the client to close after rendering.
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}
	defer run.Unsubscribe(live)

	go func() {
		defer conn.Close()
		for frame := range live {
			if conn.WriteMessage(websocket.TextMessage, frame) != nil {
				return
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
		if err := run.Command(data); err != nil {
			msg, _ := json.Marshal(map[string]string{"type": "_error", "error": err.Error()})
			conn.WriteMessage(websocket.TextMessage, msg)
		}
	}
}

func (s *Server) handleMushStop(w http.ResponseWriter, r *http.Request) {
	run, ok := s.mushruns.Get(r.PathValue("id"))
	if !ok {
		http.Error(w, "no such run", http.StatusNotFound)
		return
	}
	run.Stop()
	w.WriteHeader(http.StatusNoContent)
}
