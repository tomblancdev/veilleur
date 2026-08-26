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
	"github.com/tomblancdev/veilleur/internal/state"
)

func testServer(t *testing.T) (*Server, *state.Store, *door.Mock) {
	t.Helper()
	d := func(s string) config.Duration {
		v, _ := time.ParseDuration(s)
		return config.Duration(v)
	}
	cfg := &config.Config{
		House: "Example House", Interval: d("30s"), DoorCfg: config.Door{Mode: "mock"},
		Auth: config.Auth{UserHeader: "Remote-User", GroupsHeader: "Remote-Groups",
			AdminGroups: []string{"admins"}, TrustedProxies: []string{"192.0.2.10"}},
		Signals: map[string]config.Signal{
			"console_in_use": {Name: "console_in_use", RunOn: "tower", TTL: d("1s")},
		},
		Targets: map[string]config.Target{
			"tower": {Name: "tower", Kind: config.KindNode, Node: "tower", OnDemand: true, UpTimeout: d("1m"), MinUptime: d("1m")},
			"console": {Name: "console", Kind: config.KindGuest, Node: "tower", Needs: []string{"tower"}, UpTimeout: d("1m"), MinUptime: d("1m")},
		},
		Downs: map[string]config.Down{
			"console": {Name: "console", StopWhen: []string{"!console_in_use"}, Grace: d("2m"), Manages: []string{"console"}},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := door.NewMock("tower")
	m.Signals["console_in_use"] = 1
	m.State["tower"], m.State["console"] = 1, 1
	m.OnUp = func(tgt string) { m.SetUp(tgt, true) }
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	e := fleet.New(cfg, st, m, log)
	return New(cfg, st, e, mustAuth(t, cfg.Auth), "test", log), st, m
}

func asAdmin(r *http.Request) *http.Request {
	r.RemoteAddr = "192.0.2.10:1234"
	r.Header.Set("Remote-User", "tom")
	r.Header.Set("Remote-Groups", "admins")
	return r
}

func TestBoardIsAdminsOnly(t *testing.T) {
	s, _, _ := testServer(t)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusForbidden {
		t.Fatalf("anonymous should be refused, got %d", w.Code)
	}
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, asAdmin(httptest.NewRequest(http.MethodGet, "/", nil)))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "tower") {
		t.Fatalf("an admin should see the board (%d)", w.Code)
	}
}

func TestWakeReturnsTheChain(t *testing.T) {
	s, _, m := testServer(t)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, asAdmin(httptest.NewRequest(http.MethodPost, "/api/targets/console/wake",
		strings.NewReader(`{"reason":"play","wait":true}`))))
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body)
	}
	var body struct {
		Chain []string `json:"chain"`
		Up    bool     `json:"up"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Chain) != 2 || body.Chain[0] != "tower" {
		t.Fatalf("the chain should be node then guest: %v", body.Chain)
	}
	if !m.Took("up tower") || !m.Took("up console") {
		t.Fatalf("both should have been raised: %v", m.Actions)
	}
}

func TestWakeOfAnUnknownTargetIs404(t *testing.T) {
	s, _, _ := testServer(t)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, asAdmin(httptest.NewRequest(http.MethodPost, "/api/targets/nowhere/wake", nil)))
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", w.Code)
	}
}

// A hold must carry a reason: it can outlive the memory of why it was taken.
func TestHoldNeedsAReason(t *testing.T) {
	s, st, _ := testServer(t)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, asAdmin(httptest.NewRequest(http.MethodPost, "/api/holds",
		strings.NewReader(`{"target":"console"}`))))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body)
	}
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, asAdmin(httptest.NewRequest(http.MethodPost, "/api/holds",
		strings.NewReader(`{"target":"console","reason":"debugging the GPU"}`))))
	if w.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", w.Code, w.Body)
	}
	if len(st.All()) != 1 {
		t.Fatal("the hold should be stored")
	}
}

func TestHoldsAreAdminsOnly(t *testing.T) {
	s, _, _ := testServer(t)
	r := httptest.NewRequest(http.MethodPost, "/api/holds", strings.NewReader(`{"target":"console","reason":"x"}`))
	r.RemoteAddr = "192.0.2.10:1234"
	r.Header.Set("Remote-User", "alice")
	r.Header.Set("Remote-Groups", "players")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("a hold is a person's decision — admins only; got %d", w.Code)
	}
}

func TestOpenAPIAndHealthAreOpen(t *testing.T) {
	s, _, _ := testServer(t)
	for _, p := range []string{"/openapi.json", "/healthz", "/metrics"} {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, p, nil))
		if w.Code != http.StatusOK && w.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s should answer without auth, got %d", p, w.Code)
		}
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/openapi.json", nil))
	var doc map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatalf("the published schema must be valid JSON: %v", err)
	}
}

func mustAuth(t *testing.T, c config.Auth) *auth.Auth {
	t.Helper()
	a, err := auth.New(c)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
