package fleet

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/tomblancdev/veilleur/internal/config"
	"github.com/tomblancdev/veilleur/internal/door"
	"github.com/tomblancdev/veilleur/internal/state"
)

// the lab's own shape: two guests on one on-demand node, plus a guest that
// Le Veilleur is NOT allowed to stop (started by hand, or by onboot).
func labConfig() *config.Config {
	d := func(s string) config.Duration {
		v, err := time.ParseDuration(s)
		if err != nil {
			panic(err)
		}
		return config.Duration(v)
	}
	c := &config.Config{
		Interval: d("30s"), DoorCfg: config.Door{Mode: "mock"},
		Signals: map[string]config.Signal{
			"console_in_use":    {Name: "console_in_use", RunOn: "muscle1", TTL: d("1s")},
			"pbs_working":       {Name: "pbs_working", RunOn: "muscle1", TTL: d("1s")},
			"any_guest_running": {Name: "any_guest_running", RunOn: "muscle1", TTL: d("1s")},
			"human_session":     {Name: "human_session", RunOn: "muscle1", TTL: d("1s")},
			"cluster_whole":     {Name: "cluster_whole", RunOn: config.AnyControl, TTL: d("1s")},
		},
		Targets: map[string]config.Target{
			"muscle1": {Name: "muscle1", Kind: config.KindNode, Node: "muscle1", OnDemand: true,
				UpTimeout: d("3m"), MinUptime: d("1m")},
			"console": {Name: "console", Kind: config.KindGuest, Node: "muscle1", Needs: []string{"muscle1"},
				UpTimeout: d("3m"), MinUptime: d("1m")},
			"pbs": {Name: "pbs", Kind: config.KindGuest, Node: "muscle1", Needs: []string{"muscle1"},
				UpTimeout: d("5m"), MinUptime: d("10m")},
			// in nobody's `manages` — a guest someone started in the UI
			"byhand": {Name: "byhand", Kind: config.KindGuest, Node: "muscle1", Needs: []string{"muscle1"},
				UpTimeout: d("3m"), MinUptime: d("1m")},
		},
		Downs: map[string]config.Down{
			"console": {Name: "console", StopWhen: []string{"!console_in_use", "!hold:console"},
				Grace: d("2m"), ThenConsider: []string{"muscle1"}, Manages: []string{"console"}},
			"pbs": {Name: "pbs", StopWhen: []string{"!pbs_working", "!hold:pbs"},
				Grace: d("1m"), ThenConsider: []string{"muscle1"}, Manages: []string{"pbs"}},
			"muscle1": {Name: "muscle1", StopWhen: []string{"!any_guest_running", "!human_session", "!hold:muscle1", "cluster_whole"},
				Grace: d("10m"), Manages: []string{"muscle1", "console", "pbs"}},
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
	st *state.Store
	at time.Time
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	st, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := door.NewMock("infra1", "apps1", "muscle1")
	h := &harness{m: m, st: st, at: time.Date(2026, 9, 12, 21, 0, 0, 0, time.UTC)}
	// the world: everything down, nobody using anything, cluster whole
	for _, n := range []string{"muscle1", "console", "pbs", "byhand"} {
		m.State[n] = 1
	}
	m.Signals["console_in_use"] = 1
	m.Signals["pbs_working"] = 1
	m.Signals["any_guest_running"] = 1
	m.Signals["human_session"] = 1
	m.Signals["cluster_whole"] = 0
	// raising or dropping a target changes the mock's world, as life would
	m.OnUp = func(tgt string) { m.SetUp(tgt, true); h.refreshGuests() }
	m.OnDown = func(tgt string) { m.SetUp(tgt, false); h.refreshGuests() }

	h.e = New(labConfig(), st, m, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.e.SetClock(func() time.Time { return h.at })
	return h
}

// any_guest_running is derived from the mock's world, like the real one.
func (h *harness) refreshGuests() {
	running := 1
	for _, g := range []string{"console", "pbs", "byhand"} {
		if h.m.State[g] == 0 {
			running = 0
		}
	}
	h.m.Signals["any_guest_running"] = running
}

func (h *harness) pass()                    { h.e.Pass(context.Background()) }
func (h *harness) advance(d time.Duration)  { h.at = h.at.Add(d) }
func (h *harness) wake(t *testing.T, n string) {
	t.Helper()
	if err := h.e.Wake(context.Background(), n, "test"); err != nil {
		t.Fatalf("wake %s: %v", n, err)
	}
}

// settle runs the engine the way it runs in life.
func (h *harness) settle(steps int, step time.Duration) {
	for i := 0; i < steps; i++ {
		h.pass()
		h.advance(step)
	}
}

// THE CASE THAT LOST A BACKUP. PBS is raised at 02:55 and the job does not
// start until 03:10; in between, `pbs_working` honestly says no. Without
// min_uptime the stop path undoes the wake and the datastore is gone when
// vzdump runs.
func TestAFreshlyRaisedTargetIsNotIdle(t *testing.T) {
	h := newHarness(t)
	h.wake(t, "pbs")
	if h.m.State["pbs"] != 0 {
		t.Fatal("pbs should be up after a wake")
	}
	h.m.Reset()
	// ten minutes of "no backup task yet" — exactly the 02:55 -> 03:05 gap
	h.settle(20, 30*time.Second)
	if h.m.Took("down pbs") {
		t.Fatal("THE BACKUP BUG: pbs was stopped before its first task could start")
	}
	// the job starts, and keeps it up
	h.m.Signals["pbs_working"] = 0
	h.settle(4, time.Minute)
	if h.m.Took("down pbs") {
		t.Fatal("pbs was stopped while a backup was running")
	}
	// the job finishes; now it may go
	h.m.Signals["pbs_working"] = 1
	h.settle(10, time.Minute)
	if !h.m.Took("down pbs") {
		t.Fatalf("pbs should stop once its work is done; actions=%v", h.m.Actions)
	}
}

// Tom's 06:00 case: the backups finish while somebody is still playing.
func TestPlayingThroughTheBackupWindow(t *testing.T) {
	h := newHarness(t)
	h.wake(t, "console")
	h.m.Signals["console_in_use"] = 0 // somebody is streaming
	h.advance(2 * time.Minute)
	h.wake(t, "pbs")
	h.m.Signals["pbs_working"] = 0
	h.advance(15 * time.Minute)
	h.m.Reset()

	// the jobs finish; PBS may go, the tower may not
	h.m.Signals["pbs_working"] = 1
	h.settle(10, time.Minute)
	if !h.m.Took("down pbs") {
		t.Fatalf("pbs should stop when its work is done; actions=%v", h.m.Actions)
	}
	if h.m.Took("down muscle1") {
		t.Fatal("THE BUG: the tower slept while somebody was still playing")
	}

	// they stop playing; now the whole chain comes down
	h.m.Signals["console_in_use"] = 1
	h.settle(30, time.Minute)
	if !h.m.Took("down console") {
		t.Fatalf("the console should stop; actions=%v", h.m.Actions)
	}
	if !h.m.Took("down muscle1") {
		t.Fatalf("the tower should follow it down; actions=%v", h.m.Actions)
	}
}

// A guest nobody manages is never touched — and it keeps its node up.
func TestNeverStopsWhatItDoesNotManage(t *testing.T) {
	h := newHarness(t)
	h.wake(t, "byhand")
	h.m.Reset()
	h.settle(40, time.Minute)
	if h.m.Took("down byhand") {
		t.Fatal("a guest started by hand must never be stopped")
	}
	if h.m.Took("down muscle1") {
		t.Fatal("a node running an unmanaged guest must stay up")
	}
}

// UNKNOWN blocks a stop; it never permits one.
func TestUnknownBlocksAStop(t *testing.T) {
	h := newHarness(t)
	h.wake(t, "console")
	h.advance(5 * time.Minute)
	delete(h.m.Signals, "console_in_use") // the question cannot be put
	h.m.Reset()
	h.settle(20, time.Minute)
	if h.m.Took("down console") {
		t.Fatal("a signal that cannot be answered must block the stop")
	}
	for _, v := range h.e.Board().Targets {
		if v.Name == "console" && v.Blocked != "unknown:console_in_use" {
			t.Errorf("the board should name the unanswered signal, got %q", v.Blocked)
		}
	}
}

// A blind engine does nothing at all.
func TestBlindEngineActsOnNothing(t *testing.T) {
	h := newHarness(t)
	h.wake(t, "console")
	h.advance(10 * time.Minute)
	h.m.Reset()
	for _, n := range h.m.Known {
		h.m.Unreachable[n] = true
	}
	h.settle(10, time.Minute)
	if len(h.m.Actions) != 0 {
		t.Fatalf("a blind engine must touch nothing; actions=%v", h.m.Actions)
	}
	if ok, _ := h.e.Healthy(); ok {
		t.Error("health should report that nothing could be observed")
	}
}

// The fencing rule, as an ordinary signal.
func TestClusterNotWholeBlocksTheNode(t *testing.T) {
	h := newHarness(t)
	h.wake(t, "console")
	h.advance(5 * time.Minute)
	h.m.Signals["cluster_whole"] = 1 // a 24/7 node is missing
	h.m.Reset()
	h.settle(40, time.Minute)
	if h.m.Took("down muscle1") {
		t.Fatal("the fencing rule must stop the node being powered off")
	}
}

// A hold keeps a target up; lifting it lets go.
func TestHoldKeepsItUpAndLiftingLetsGo(t *testing.T) {
	h := newHarness(t)
	h.wake(t, "console")
	held, err := h.st.Take(state.Hold{Target: "console", By: "tom", Reason: "debugging"})
	if err != nil {
		t.Fatal(err)
	}
	h.advance(5 * time.Minute)
	h.m.Reset()
	h.settle(30, time.Minute)
	if h.m.Took("down console") {
		t.Fatal("a held target must not be stopped")
	}
	if _, err := h.st.Release(held.ID, "tom"); err != nil {
		t.Fatal(err)
	}
	h.m.Reset()
	h.settle(30, time.Minute)
	if !h.m.Took("down console") {
		t.Fatalf("lifting the hold should let it go; actions=%v", h.m.Actions)
	}
}

// hands-off refuses a wake as well as a stop.
func TestHandsOffRefusesAWake(t *testing.T) {
	h := newHarness(t)
	if _, err := h.st.Take(state.Hold{Target: "console", By: "tom", Reason: "reinstalling", HandsOff: true}); err != nil {
		t.Fatal(err)
	}
	if err := h.e.Wake(context.Background(), "console", "test"); err == nil {
		t.Fatal("a hands-off target must not be raised")
	}
	if h.m.Took("up console") {
		t.Fatal("and certainly must not be started")
	}
}

// Waking raises parents before children, and only what is missing.
func TestWakeRaisesOnlyWhatIsMissing(t *testing.T) {
	h := newHarness(t)
	h.wake(t, "console")
	var iNode, iGuest = -1, -1
	for i, a := range h.m.Actions {
		if a == "up muscle1" {
			iNode = i
		}
		if a == "up console" {
			iGuest = i
		}
	}
	if iNode < 0 || iGuest < 0 || iNode > iGuest {
		t.Fatalf("the node must be raised before the guest: %v", h.m.Actions)
	}
	h.m.Reset()
	h.wake(t, "pbs")
	if h.m.Took("up muscle1") {
		t.Fatalf("the tower was already up — it must not be raised again: %v", h.m.Actions)
	}
	if !h.m.Took("up pbs") {
		t.Fatal("pbs should have been raised")
	}
}

// One process, one key — but its reconcile loop and an API wake run
// concurrently inside it. A stop decided a moment ago must not land on a
// target a wake is busy raising, or the machine goes up and straight back
// down. Run with -race.
func TestWakeAndStopDoNotRaceOnTheSameTarget(t *testing.T) {
	h := newHarness(t)
	h.wake(t, "console")
	h.advance(30 * time.Minute) // well past min_uptime; it is stoppable
	h.pass()                    // let the grace clock start
	h.advance(30 * time.Minute)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); h.pass() }()
	go func() {
		defer wg.Done()
		_ = h.e.Wake(context.Background(), "console", "somebody wants it back")
	}()
	wg.Wait()

	// whatever the interleaving, the world must be self-consistent: if the
	// wake won, it is up; if the stop won, a later wake still brings it back.
	if err := h.e.Wake(context.Background(), "console", "settle"); err != nil {
		t.Fatalf("the console must still be raisable after the race: %v", err)
	}
	if h.m.State["console"] != 0 {
		t.Fatal("after a wake the console must be up")
	}
}

// A sleeping node must not be dialled for every question about it. Before
// this, six probes and signals aimed at a sleeping tower each burned the
// full ssh timeout, so a pass took minutes and /healthz stayed 503 long
// after start — which failed the converge's own health gate.
func TestASleepingNodeIsNotDialledForEveryQuestion(t *testing.T) {
	h := newHarness(t)
	// the tower is down; everything on it is therefore unanswerable
	h.m.State["muscle1"] = 1
	h.m.Unreachable["muscle1"] = true
	h.pass()

	b := h.e.Board()
	for _, v := range b.Targets {
		if v.Name == "muscle1" && !v.Known {
			t.Error("the node itself is answered by a CONTROL node and must stay known")
		}
		if v.Name == "console" && v.Known {
			t.Error("a guest on a sleeping node cannot be known")
		}
	}
	// and the engine must still consider itself sighted: it saw the node
	if ok, why := h.e.Healthy(); !ok {
		t.Fatalf("observing the node is observing the fleet: %q", why)
	}
	// the signals that live on the sleeping node are UNKNOWN, and say why
	if v, ok := b.Signals["console_in_use"]; ok && v.Known {
		t.Error("a signal answered by a sleeping node cannot be known")
	}
}
