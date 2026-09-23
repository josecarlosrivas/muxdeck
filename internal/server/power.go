package server

import (
	"encoding/json"
	"net/http"
)

// --- keep awake while viewing: GET reports state, POST sets the per-daemon
// preference. Presence itself rides the attach and mush-stream sockets.

func (s *Server) handlePowerStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.powerm.Status())
}

func (s *Server) handlePowerSet(w http.ResponseWriter, r *http.Request) {
	var body struct {
		KeepAwake *bool `json:"keep_awake_while_viewing"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.KeepAwake == nil {
		http.Error(w, "keep_awake_while_viewing (true|false) is required", http.StatusBadRequest)
		return
	}
	if err := s.powerm.SetEnabled(*body.KeepAwake); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, s.powerm.Status())
}
