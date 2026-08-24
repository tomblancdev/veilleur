package fleet

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/tomblancdev/veilleur/internal/config"
	"github.com/tomblancdev/veilleur/internal/door"
	"github.com/tomblancdev/veilleur/internal/store"
)

// the lab's own graph, small enough to reason about in a test:
//
//	console 5001 ─┐
//	              ├─ requires ─▶ muscle1 (on-demand node)
//	pbs     1002 ─┘
func labConfig() *config.Config {
	c := &config.Config{
		ReconcileInterval: config.Duration(time.Second),
		DefaultHold:       config.Duration(8 * time.Hour),
		DoorCfg:           config.Door{Mode: "mock"},
		Targets: map[string]config.Target{
			"muscle1": {
				Name: "muscle1", Kind: config.KindNode, Node: "muscle1",
				OnDemand: true, WOL: true,
				UpTimeout: config.Duration(2 * time.Minute), DownGrace: config.Duration(10 * time.Minute),
				Guards: []string{"human_session", "maintenance_lock", "converge_lock", "cluster_whole"},
			},
			"pbs": {
				Name: "pbs", Kind: config.KindGuest, VMID: 1002, Node: "muscle1",
				Requires: []string{"muscle1"}, DownGrace: config.Duration(time.Minute),
			},
			"console": {
				Name: "console", Kind: config.KindGuest, VMID: 5001, Node: "muscle1",
				Requires: []string{"muscle1"}, DownGrace: config.Duration(2 * time.Minute),
				IdleAfter: config.Duration(20 * time.Minute),
			},
		},
	}
	if err := c.Validate(); err != nil {
		panic(err)
	}
	return c
}

type harness struct {
	e  *Engine
	m  *door.Mock
	st *store.Store
	at time.Time
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := door.NewMock("infra1", "apps1", "muscle1")
	m.AddGuest(1002, "muscle1")
	m.AddGuest(5001, "muscle1")
	m.NodesUp["muscle1"] = false // the normal state
	m.NodeTTYs["muscle1"] = 0
	m.NodeTTYs["infra1"] = 0
	m.NodeTTYs["apps1"] = 0

	h := &harness{m: m, st: st, at: time.Date(2026, 9, 12, 21, 0, 0, 0, time.UTC)}
	h.e = New(labConfig(), st, m, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.e.SetClock(func() time.Time { return h.at })
	m.Now = func() time.Time { return h.at }
	return h
}

func (h *harness) tick()                   { h.e.Reconcile(context.Background()) }
func (h *harness) advance(d time.Duration) { h.at = h.at.Add(d) }

// settle runs the engine as it runs in life: observe, act, wait, observe
// again. A stop is a request — the guest is seen down on a LATER pass, and
// only then does the node it lived on start counting its own grace.
func (h *harness) settle(steps int, step time.Duration) {
	for i := 0; i < steps; i++ {
		h.tick()
		h.advance(step)
	}
}

func (h *harness) claim(t *testing.T, target, subject, reason string, release string) store.Claim {
	t.Helper()
	c, err := h.st.Take(store.Claim{
		Subject: subject, Target: target, Reason: reason, Via: "test",
		Release: release, HeldSince: h.at, LastActive: h.at,
		Deadline: h.at.Add(8 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// The case Tom named: still playing at 06:00 while the backups finish. The
// node must NOT go to sleep behind the finished backup, and PBS must stop
// anyway. Nothing here is special-cased — it is refcounting.
func TestPlayingThroughTheBackupWindow(t *testing.T) {
	h := newHarness(t)

	// 21:00 — the play page claims the console.
	h.claim(t, "console", "tom", "play page: wake it", store.ReleaseExplicit)
	h.tick()
	if !h.m.Took("wake muscle1") {
		t.Fatalf("the tower should have been woken; actions=%v", h.m.Actions)
	}
	h.tick() // the node is up now: the guest follows on the next pass
	if !h.m.Took("start 5001") {
		t.Fatalf("the console should have been started; actions=%v", h.m.Actions)
	}

	// 02:55 — the backups claim PBS. The tower is already up: no second wake.
	h.advance(6 * time.Hour)
	h.m.Reset()
	backup := h.claim(t, "pbs", "backups", "squat-nightly", store.ReleaseExplicit)
	h.tick()
	if h.m.Took("wake muscle1") {
		t.Error("the tower was already up — it must not be woken again")
	}
	if !h.m.Took("start 1002") {
		t.Fatalf("PBS should have been started; actions=%v", h.m.Actions)
	}

	// 05:10 — the jobs are done, the backups release. PBS stops; the tower
	// stays up because the console's claim is still held.
	h.advance(2*time.Hour + 15*time.Minute)
	h.m.Reset()
	if _, err := h.st.Release(backup.ID, "backups"); err != nil {
		t.Fatal(err)
	}
	h.tick()                    // marks pbs unwanted, grace starts
	h.advance(2 * time.Minute)  // past pbs's 1 min grace
	h.tick()
	if !h.m.Took("stop 1002") {
		t.Fatalf("PBS should have stopped when its claim was released; actions=%v", h.m.Actions)
	}
	if h.m.Took("poweroff muscle1") {
		t.Fatal("THE BUG: the tower slept while somebody was still playing")
	}
	if !h.m.NodesUp["muscle1"] {
		t.Fatal("the tower must still be powered while the console's claim is held")
	}

	// 06:00 — the old window. Nothing happens, because there is no window.
	h.advance(50 * time.Minute)
	h.m.Reset()
	h.tick()
	if len(h.m.Actions) != 0 {
		t.Fatalf("06:00 must be an ordinary minute now; actions=%v", h.m.Actions)
	}
}

// The evening leak: stop playing at 22:00 and the tower goes to sleep on its
// own grace, not at 06:00 the next morning.
func TestTowerSleepsAfterTheEveningNotAtSix(t *testing.T) {
	h := newHarness(t)
	c := h.claim(t, "console", "tom", "play page", store.ReleaseExplicit)
	h.settle(3, time.Minute)

	// 22:00 — done playing.
	stopped := h.at
	if _, err := h.st.Release(c.ID, "tom"); err != nil {
		t.Fatal(err)
	}
	h.m.Reset()
	h.settle(20, 2*time.Minute) // up to 40 simulated minutes

	if !h.m.Took("stop 5001") {
		t.Fatalf("the console should have stopped; actions=%v", h.m.Actions)
	}
	if !h.m.Took("poweroff muscle1") {
		t.Fatalf("the tower should have powered off within the hour, not at 06:00; actions=%v", h.m.Actions)
	}
	// the point of the whole product: no window, no waiting for the morning
	if slept := h.at.Sub(stopped); slept > time.Hour {
		t.Fatalf("the tower took %s to sleep — that is the evening leak again", slept)
	}
}

// A parent is never stopped while a child still runs, and never started
// after one: order is the graph's, not the map's.
func TestOrderParentsUpFirstChildrenDownFirst(t *testing.T) {
	h := newHarness(t)
	h.claim(t, "console", "tom", "play", store.ReleaseExplicit)
	h.tick()
	if h.m.Took("start 5001") {
		t.Error("the console must not be started before its node is up")
	}
	h.tick()
	if !h.m.Took("start 5001") {
		t.Fatal("the console should start once the node is up")
	}

	// now make everything unwanted at once
	for _, c := range h.st.All() {
		if _, err := h.st.Release(c.ID, "test"); err != nil {
			t.Fatal(err)
		}
	}
	h.m.Reset()
	h.settle(20, 2*time.Minute)

	stopIdx, offIdx := -1, -1
	for i, a := range h.m.Actions {
		if a == "stop 5001" && stopIdx < 0 {
			stopIdx = i
		}
		if a == "poweroff muscle1" && offIdx < 0 {
			offIdx = i
		}
	}
	if stopIdx < 0 || offIdx < 0 {
		t.Fatalf("expected both a stop and a poweroff; actions=%v", h.m.Actions)
	}
	if stopIdx > offIdx {
		t.Fatalf("the guest must stop before its node is powered off; actions=%v", h.m.Actions)
	}
}

// Fail as-is: with no view of the fleet, the engine touches nothing.
func TestObserveFailureActsOnNothing(t *testing.T) {
	h := newHarness(t)
	h.claim(t, "console", "tom", "play", store.ReleaseExplicit)
	h.m.ObserveErr = errors.New("the door is shut")
	h.tick()
	if len(h.m.Actions) != 0 {
		t.Fatalf("a blind engine must do nothing; actions=%v", h.m.Actions)
	}
	if ok, _ := h.e.Healthy(); ok {
		t.Error("health should report the observation failure")
	}
}

func TestGuardsRefuseToStop(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*harness)
		guard string
	}{
		{"a human is logged in", func(h *harness) { h.m.NodeTTYs["muscle1"] = 1 }, "human_session"},
		{"we cannot see its ttys", func(h *harness) { h.m.NodeTTYs["muscle1"] = -1 }, "human_session"},
		{"a maintenance pass holds the lock", func(h *harness) { h.m.Locks["maintenance"] = true }, "maintenance_lock"},
		{"a converge is running", func(h *harness) { h.m.Locks["converge"] = true }, "converge_lock"},
		{"the 24/7 pair is short", func(h *harness) { h.m.NodesUp["apps1"] = false }, "cluster_whole"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			c := h.claim(t, "console", "tom", "play", store.ReleaseExplicit)
			h.settle(3, time.Minute)
			if _, err := h.st.Release(c.ID, "tom"); err != nil {
				t.Fatal(err)
			}
			tc.setup(h)
			h.m.Reset()
			h.settle(20, 2*time.Minute)

			if h.m.Took("poweroff muscle1") {
				t.Fatalf("guard %s should have refused the poweroff; actions=%v", tc.guard, h.m.Actions)
			}
			var blocked string
			for _, v := range h.e.Board().Targets {
				if v.Name == "muscle1" {
					blocked = v.Blocked
				}
			}
			if blocked != "guard:"+tc.guard {
				t.Errorf("the board should name the guard: got %q, want %q", blocked, "guard:"+tc.guard)
			}
		})
	}
}

// An HA-managed guest is the HA manager's to stop, never ours — even if
// somebody declared it as a target.
func TestNeverStopsAnHAResource(t *testing.T) {
	h := newHarness(t)
	h.m.GuestHA[5001] = true
	h.m.GuestsUp[5001] = true
	h.m.NodesUp["muscle1"] = true
	h.advance(time.Hour)
	h.tick()
	if h.m.Took("stop 5001") {
		t.Fatalf("an HA resource must never be stopped by the watchman; actions=%v", h.m.Actions)
	}
}

// Everything expires: a claim past its deadline stops holding its target.
func TestDeadlineEndsAClaim(t *testing.T) {
	h := newHarness(t)
	if _, err := h.st.Take(store.Claim{
		Subject: "tom", Target: "console", Via: "test", Reason: "short hold",
		Release: store.ReleaseDeadline, HeldSince: h.at, LastActive: h.at,
		Deadline: h.at.Add(30 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	h.tick()
	h.tick()
	if !h.m.NodesUp["muscle1"] || !h.m.GuestsUp[5001] {
		t.Fatal("the claim should have brought both up")
	}
	h.advance(31 * time.Minute)
	h.m.Reset()
	h.tick()
	h.advance(3 * time.Minute)
	h.tick()
	if !h.m.Took("stop 5001") {
		t.Fatalf("an expired claim holds nothing; actions=%v", h.m.Actions)
	}
	if h.st.Held(h.at) != nil {
		t.Error("no claim should still be held")
	}
}

// An idle-ruled claim renews while the target reports activity.
func TestIdleClaimRenewsOnHeartbeat(t *testing.T) {
	h := newHarness(t)
	c, err := h.st.Take(store.Claim{
		Subject: "console", Target: "console", Via: "agent", Reason: "session open",
		Release: store.ReleaseIdle, IdleAfter: 20 * time.Minute,
		HeldSince: h.at, LastActive: h.at, Deadline: h.at.Add(8 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	h.advance(15 * time.Minute)
	if _, err := h.st.Heartbeat(c.ID, "agent", h.at, 0); err != nil {
		t.Fatal(err)
	}
	h.advance(15 * time.Minute) // 30 min since the claim, 15 since activity
	if got := len(h.st.Held(h.at)); got != 1 {
		t.Fatalf("a heartbeat should keep the claim held, got %d held", got)
	}
	h.advance(10 * time.Minute) // 25 min of silence
	if got := len(h.st.Held(h.at)); got != 0 {
		t.Fatalf("20 min idle should end the claim, got %d held", got)
	}
}

// A watchman that has never looked is not healthy. Before this, snapErr
// started empty, /healthz answered 200 from boot, and the converge's health
// gate passed on a service whose door was blocked (found live 2026-08-24).
func TestNotHealthyBeforeTheFirstObservation(t *testing.T) {
	h := newHarness(t)
	if ok, why := h.e.Healthy(); ok {
		t.Fatal("health must not say ok before the first successful pass")
	} else if why == "" {
		t.Error("and it should say why")
	}
	h.tick()
	if ok, why := h.e.Healthy(); !ok {
		t.Fatalf("after a good pass it is healthy, got %q", why)
	}
}
