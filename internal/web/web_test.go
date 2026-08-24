package web

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tomblancdev/veilleur/internal/auth"
	"github.com/tomblancdev/veilleur/internal/config"
	"github.com/tomblancdev/veilleur/internal/door"
	"github.com/tomblancdev/veilleur/internal/fleet"
	"github.com/tomblancdev/veilleur/internal/store"
)

func testServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	cfg := &config.Config{
		House: "Le Squat", ReconcileInterval: time.Minute, DefaultHold: 8 * time.Hour,
		DoorCfg: config.Door{Mode: "mock"},
		Auth:    config.Auth{UserHeader: "Remote-User", GroupsHeader: "Remote-Groups", AdminGroups: []string{"admins"}, TrustedProxies: []string{"10.0.0.10"}},
		Targets: map[string]config.Target{
			"muscle1": {Name: "muscle1", Kind: config.KindNode, Node: "muscle1", OnDemand: true, WOL: true, UpTimeout: time.Minute, DownGrace: time.Minute, MaxHold: 8 * time.Hour},
			"console": {Name: "console", Kind: config.KindGuest, VMID: 5001, Node: "muscle1", Requires: []string{"muscle1"}, UpTimeout: time.Minute, DownGrace: time.Minute, MaxHold: 4 * time.Hour, IdleAfter: 20 * time.Minute},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := door.NewMock("infra1", "apps1", "muscle1")
	m.AddGuest(5001, "muscle1")
	a, err := auth.New(cfg.Auth)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	e := fleet.New(cfg, st, m, log)
	return New(cfg, st, e, a, "test", log), st
}

func asAdmin(r *http.Request) *http.Request {
	r.RemoteAddr = "10.0.0.10:1234"
	r.Header.Set("Remote-User", "tom")
	r.Header.Set("Remote-Groups", "admins")
	return r
}

// The board is an operations plane, so it is an admin plane (v1).
func TestBoardIsAdminsOnly(t *testing.T) {
	s, _ := testServer(t)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusForbidden {
		t.Fatalf("an anonymous request should be refused, got %d", w.Code)
	}

	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, asAdmin(httptest.NewRequest(http.MethodGet, "/", nil)))
	if w.Code != http.StatusOK {
		t.Fatalf("an admin should see the board, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "muscle1") {
		t.Error("the board should list the targets")
	}
}

// ensure is the verb clients want: it claims and reports the chain.
func TestEnsureTakesAClaimAndReturnsTheChain(t *testing.T) {
	s, st := testServer(t)
	w := httptest.NewRecorder()
	r := asAdmin(httptest.NewRequest(http.MethodPost, "/api/targets/console/ensure",
		strings.NewReader(`{"reason":"play page: wake it","hold":"2h"}`)))
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d: %s", w.Code, w.Body)
	}
	var body struct {
		Claim store.Claim `json:"claim"`
		Chain []struct {
			Name string `json:"name"`
		} `json:"chain"`
		ETA int `json:"eta_seconds"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Claim.Target != "console" || body.Claim.Reason != "play page: wake it" {
		t.Fatalf("unexpected claim: %+v", body.Claim)
	}
	if len(body.Chain) != 2 || body.Chain[0].Name != "muscle1" || body.Chain[1].Name != "console" {
		t.Fatalf("the chain should be the node then the guest, got %+v", body.Chain)
	}
	if body.ETA <= 0 {
		t.Error("an ETA should be reported while things are down")
	}
	if got := len(st.Held(time.Now())); got != 1 {
		t.Fatalf("the claim should be held, got %d", got)
	}
}

// A target's max_hold is policy: asking for longer is clamped, not refused.
func TestHoldIsClampedToTheTargetsPolicy(t *testing.T) {
	s, _ := testServer(t)
	w := httptest.NewRecorder()
	r := asAdmin(httptest.NewRequest(http.MethodPost, "/api/claims",
		strings.NewReader(`{"target":"console","reason":"long night","hold":"48h"}`)))
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", w.Code, w.Body)
	}
	var c store.Claim
	if err := json.NewDecoder(w.Body).Decode(&c); err != nil {
		t.Fatal(err)
	}
	if d := time.Until(c.Deadline); d > 4*time.Hour+time.Minute {
		t.Fatalf("the console caps holds at 4h, got %s", d)
	}
}

func TestUnknownTargetIsRefused(t *testing.T) {
	s, _ := testServer(t)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, asAdmin(httptest.NewRequest(http.MethodPost, "/api/claims",
		strings.NewReader(`{"target":"nowhere"}`))))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for an unknown target, got %d", w.Code)
	}
}

// A client may not release someone else's claim.
func TestClientCannotReleaseAnotherSubjectsClaim(t *testing.T) {
	s, st := testServer(t)
	c, err := st.Take(store.Claim{Subject: "tom", Target: "console", Via: "test",
		HeldSince: time.Now(), LastActive: time.Now(), Deadline: time.Now().Add(time.Hour), Release: store.ReleaseExplicit})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodDelete, "/api/claims/"+c.ID, nil)
	r.RemoteAddr = "10.0.0.10:1234"
	r.Header.Set("Remote-User", "alice")
	r.Header.Set("Remote-Groups", "players")
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d: %s", w.Code, w.Body)
	}
}

func TestOpenAPIAndHealthAreOpen(t *testing.T) {
	s, _ := testServer(t)
	for _, path := range []string{"/openapi.json", "/healthz", "/metrics"} {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		// /healthz is 503 until the first observation succeeds; both are "answered"
		if w.Code != http.StatusOK && w.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s should answer without auth, got %d", path, w.Code)
		}
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/openapi.json", nil))
	var doc map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatalf("the published schema must be valid JSON: %v", err)
	}
	if _, ok := doc["paths"]; !ok {
		t.Error("the schema should describe paths")
	}
}
