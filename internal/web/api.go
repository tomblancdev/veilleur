package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/tomblancdev/veilleur/internal/auth"
	"github.com/tomblancdev/veilleur/internal/state"
)

// The JSON API. Three nouns: targets, signals, holds. No claims — a wake is
// a request that is forgotten once the chain is up (power.md §A).

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, format string, args ...any) {
	writeJSON(w, code, map[string]string{"error": fmt.Sprintf(format, args...)})
}

func (s *Server) whoOr401(w http.ResponseWriter, r *http.Request) (auth.Identity, bool) {
	id := s.identify(r)
	if id.Role == auth.None {
		fail(w, http.StatusUnauthorized, "who are you?")
		return id, false
	}
	return id, true
}

func (s *Server) apiTargets(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.engine.Board())
}

func (s *Server) apiTarget(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	for _, v := range s.engine.Board().Targets {
		if v.Name == name {
			writeJSON(w, http.StatusOK, v)
			return
		}
	}
	fail(w, http.StatusNotFound, "no such target")
}

func (s *Server) apiSignals(w http.ResponseWriter, _ *http.Request) {
	b := s.engine.Board()
	type row struct {
		Name  string `json:"name"`
		RunOn string `json:"run_on"`
		Means string `json:"means"`
		Known bool   `json:"known"`
		True  bool   `json:"true"`
		At    string `json:"at,omitempty"`
		Err   string `json:"error,omitempty"`
	}
	cfg := s.engine.Config()
	out := make([]row, 0, len(cfg.Signals))
	for _, n := range cfg.SignalNames() {
		sig := cfg.Signals[n]
		v := b.Signals[n]
		r := row{Name: n, RunOn: sig.RunOn, Means: sig.Means, Known: v.Known, True: v.True, Err: v.Err}
		if !v.At.IsZero() {
			r.At = v.At.Format(time.RFC3339)
		}
		out = append(out, r)
	}
	writeJSON(w, http.StatusOK, map[string]any{"signals": out})
}

// apiWake is the verb every client wants: raise this, and everything it
// needs, and tell me what that implies.
func (s *Server) apiWake(w http.ResponseWriter, r *http.Request) {
	id, ok := s.whoOr401(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	chain := s.engine.Chain(name)
	if chain == nil {
		fail(w, http.StatusNotFound, "no such target: %q", name)
		return
	}
	var body struct {
		Reason string `json:"reason"`
		Wait   bool   `json:"wait"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	why := strings.TrimSpace(body.Reason)
	if why == "" {
		why = "no reason given"
	}
	why = fmt.Sprintf("%s (%s via %s)", why, id.User, id.Via)

	if body.Wait {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Minute)
		defer cancel()
		if err := s.engine.Wake(ctx, name, why); err != nil {
			fail(w, http.StatusConflict, "%v", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"target": name, "up": true, "chain": chain})
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		if err := s.engine.Wake(ctx, name, why); err != nil {
			s.log.Error("wake failed", "target", name, "err", err)
		}
	}()
	writeJSON(w, http.StatusAccepted, map[string]any{"target": name, "chain": chain, "reason": why})
}

// --- holds: the only state a person writes --------------------------------

func (s *Server) apiHolds(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.whoOr401(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"holds": s.store.All()})
}

func (s *Server) apiTakeHold(w http.ResponseWriter, r *http.Request) {
	id, ok := s.whoOr401(w, r)
	if !ok {
		return
	}
	if !id.IsAdmin() {
		fail(w, http.StatusForbidden, "a hold is a person's decision — admins only")
		return
	}
	var body struct {
		Target   string `json:"target"`
		Reason   string `json:"reason"`
		HandsOff bool   `json:"hands_off"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		fail(w, http.StatusBadRequest, "body: %v", err)
		return
	}
	if _, exists := s.engine.Config().Targets[body.Target]; !exists {
		fail(w, http.StatusBadRequest, "no such target: %q", body.Target)
		return
	}
	reason := strings.TrimSpace(body.Reason)
	if reason == "" {
		fail(w, http.StatusBadRequest, "a hold needs a reason — it may outlive your memory of it")
		return
	}
	h, err := s.store.Take(state.Hold{Target: body.Target, By: id.User, Reason: reason, HandsOff: body.HandsOff})
	if err != nil {
		fail(w, http.StatusInternalServerError, "store: %v", err)
		return
	}
	s.log.Info("hold taken", "hold", h.ID, "target", h.Target, "by", h.By, "reason", h.Reason, "hands_off", h.HandsOff)
	s.engine.Kick()
	writeJSON(w, http.StatusCreated, h)
}

func (s *Server) apiReleaseHold(w http.ResponseWriter, r *http.Request) {
	id, ok := s.whoOr401(w, r)
	if !ok {
		return
	}
	if !id.IsAdmin() {
		fail(w, http.StatusForbidden, "admins only")
		return
	}
	h, err := s.store.Release(r.PathValue("id"), id.User)
	if err != nil {
		fail(w, http.StatusNotFound, "no such hold")
		return
	}
	s.log.Info("hold released", "hold", h.ID, "target", h.Target, "by", id.User)
	s.engine.Kick()
	writeJSON(w, http.StatusOK, h)
}
