package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/tomblancdev/veilleur/internal/auth"
	"github.com/tomblancdev/veilleur/internal/store"
)

// The JSON API. Machines speak this; the page uses the same handlers.
// The schema is served at /openapi.json — the standard the lab agreed on,
// so a role, a script or another service can be a client without a library.

type claimRequest struct {
	Target    string `json:"target"`
	Reason    string `json:"reason"`
	Release   string `json:"release"`             // explicit | idle | deadline
	Hold      string `json:"hold,omitempty"`      // duration, e.g. "2h" — clamped to the target's max_hold
	IdleAfter string `json:"idle_after,omitempty"`
}

type errorBody struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, format string, args ...any) {
	writeJSON(w, code, errorBody{Error: fmt.Sprintf(format, args...)})
}

// newClaim validates a request against the target's policy and returns a
// claim ready to store. Every claim gets a deadline, whatever it asked for.
func (s *Server) newClaim(id auth.Identity, req claimRequest) (store.Claim, error) {
	t, ok := s.engine.Target(req.Target)
	if !ok {
		return store.Claim{}, fmt.Errorf("no such target: %q", req.Target)
	}
	now := time.Now()
	hold := t.MaxHold
	if req.Hold != "" {
		d, err := time.ParseDuration(req.Hold)
		if err != nil {
			return store.Claim{}, fmt.Errorf("hold: %w", err)
		}
		if d <= 0 {
			return store.Claim{}, fmt.Errorf("hold must be positive")
		}
		if d > t.MaxHold {
			// policy, not preference: a target says how long it may be pinned
			d = t.MaxHold
		}
		hold = d
	}
	release := req.Release
	if release == "" {
		release = store.ReleaseExplicit
	}
	switch release {
	case store.ReleaseExplicit, store.ReleaseIdle, store.ReleaseDeadline:
	default:
		return store.Claim{}, fmt.Errorf("release must be explicit, idle or deadline")
	}
	idleAfter := t.IdleAfter
	if req.IdleAfter != "" {
		d, err := time.ParseDuration(req.IdleAfter)
		if err != nil {
			return store.Claim{}, fmt.Errorf("idle_after: %w", err)
		}
		idleAfter = d
	}
	if release == store.ReleaseIdle && idleAfter <= 0 {
		return store.Claim{}, fmt.Errorf("release idle needs idle_after (the target declares none)")
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		reason = "no reason given"
	}
	return store.Claim{
		Subject: id.User, Via: id.Via, Target: req.Target, Reason: reason,
		HeldSince: now, LastActive: now, Deadline: now.Add(hold),
		Release: release, IdleAfter: idleAfter,
	}, nil
}

func (s *Server) apiClaims(w http.ResponseWriter, r *http.Request) {
	id := s.identify(r)
	if id.Role == auth.None {
		fail(w, http.StatusUnauthorized, "who are you?")
		return
	}
	all := s.store.All()
	if !id.IsAdmin() {
		var mine []store.Claim
		for _, c := range all {
			if c.Subject == id.User {
				mine = append(mine, c)
			}
		}
		all = mine
	}
	now := time.Now()
	type view struct {
		store.Claim
		State store.State `json:"state"`
	}
	out := make([]view, 0, len(all))
	for _, c := range all {
		out = append(out, view{Claim: c, State: c.StateAt(now)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"claims": out})
}

func (s *Server) apiTakeClaim(w http.ResponseWriter, r *http.Request) {
	id := s.identify(r)
	if id.Role == auth.None {
		fail(w, http.StatusUnauthorized, "who are you?")
		return
	}
	var req claimRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "body: %v", err)
		return
	}
	c, err := s.newClaim(id, req)
	if err != nil {
		fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	saved, err := s.store.Take(c)
	if err != nil {
		fail(w, http.StatusInternalServerError, "store: %v", err)
		return
	}
	s.log.Info("claim taken", "claim", saved.ID, "target", saved.Target, "subject", saved.Subject, "via", saved.Via, "reason", saved.Reason)
	s.engine.Kick()
	writeJSON(w, http.StatusCreated, saved)
}

func (s *Server) apiReleaseClaim(w http.ResponseWriter, r *http.Request) {
	id := s.identify(r)
	if id.Role == auth.None {
		fail(w, http.StatusUnauthorized, "who are you?")
		return
	}
	cid := r.PathValue("id")
	c, err := s.store.Get(cid)
	if err != nil {
		fail(w, http.StatusNotFound, "no such claim")
		return
	}
	if !id.IsAdmin() && c.Subject != id.User {
		fail(w, http.StatusForbidden, "not your claim")
		return
	}
	out, err := s.store.Release(cid, id.User)
	if err != nil {
		fail(w, http.StatusInternalServerError, "store: %v", err)
		return
	}
	s.log.Info("claim released", "claim", cid, "target", out.Target, "by", id.User)
	s.engine.Kick()
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) apiHeartbeat(w http.ResponseWriter, r *http.Request) {
	id := s.identify(r)
	if id.Role == auth.None {
		fail(w, http.StatusUnauthorized, "who are you?")
		return
	}
	cid := r.PathValue("id")
	c, err := s.store.Get(cid)
	if err != nil {
		fail(w, http.StatusNotFound, "no such claim")
		return
	}
	if !id.IsAdmin() && c.Subject != id.User {
		fail(w, http.StatusForbidden, "not your claim")
		return
	}
	out, err := s.store.Heartbeat(cid, id.User, time.Now(), 0)
	if err != nil {
		fail(w, http.StatusInternalServerError, "store: %v", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
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

// apiEnsure is the verb most clients want: claim it AND drive the chain up,
// answering with what is still missing and roughly how long it will take.
func (s *Server) apiEnsure(w http.ResponseWriter, r *http.Request) {
	id := s.identify(r)
	if id.Role == auth.None {
		fail(w, http.StatusUnauthorized, "who are you?")
		return
	}
	name := r.PathValue("name")
	var req claimRequest
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req) // an empty body is fine
	}
	req.Target = name
	c, err := s.newClaim(id, req)
	if err != nil {
		fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	saved, err := s.store.Take(c)
	if err != nil {
		fail(w, http.StatusInternalServerError, "store: %v", err)
		return
	}
	s.log.Info("ensure", "claim", saved.ID, "target", name, "subject", saved.Subject, "reason", saved.Reason)
	s.engine.Kick()
	chain := s.engine.Chain(name)
	up := false
	for _, v := range chain {
		if v.Name == name {
			up = v.Up
		}
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"claim":       saved,
		"target":      name,
		"up":          up,
		"chain":       chain,
		"eta_seconds": int(s.engine.ETA(name).Seconds()),
	})
}

func (s *Server) apiFleet(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.engine.Snapshot())
}
