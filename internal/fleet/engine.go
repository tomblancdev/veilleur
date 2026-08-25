// Package fleet is the brain: it asks signals, and moves machines.
//
// Two paths, deliberately separate (power.md §A):
//
//	WAKE  a request. Resolve `needs`, see what is already up, raise only what
//	      is missing, parents first. Nothing here consults the down config.
//	STOP  a loop. For each target some down entry MANAGES, evaluate its
//	      stop_when signals; UNKNOWN blocks, all-true plus grace acts.
//
// Nothing is remembered about who asked. A wake is forgotten once the chain
// is up; what keeps a thing up afterwards is measured.
package fleet

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/tomblancdev/veilleur/internal/config"
	"github.com/tomblancdev/veilleur/internal/door"
	"github.com/tomblancdev/veilleur/internal/state"
)

// Value is one signal's last answer.
type Value struct {
	Known bool      `json:"known"`
	True  bool      `json:"true"`
	At    time.Time `json:"at"`
	Err   string    `json:"error,omitempty"`
}

// Fresh reports whether the answer is still within its ttl.
func (v Value) Fresh(now time.Time, ttl time.Duration) bool {
	return v.Known && now.Sub(v.At) <= ttl
}

// Engine reconciles the fleet.
type Engine struct {
	cfg *config.Config
	st  *state.Store
	dr  door.Door
	log *slog.Logger
	now func() time.Time

	mu       sync.RWMutex
	vals     map[string]Value     // signal -> last answer
	up       map[string]bool      // target -> observed up
	upKnown  map[string]bool      // target -> did its state probe answer
	upSince  map[string]time.Time // target -> when we first saw it up
	okSince  map[string]time.Time // target -> since when every stop_when agreed
	blocked  map[string]string    // target -> why it is not being stopped
	pending  map[string]string    // target -> an action in flight
	lastErr  map[string]string
	acts     map[string]int
	seenSig  map[string]bool // signal -> has ever answered (min_uptime rule)
	firstErr string

	// One process, one key — but the reconcile loop and an API-triggered
	// wake run concurrently inside it. Without this, a stop decided a moment
	// ago can land on a target a wake is busy raising, and the machine goes
	// up and straight back down. One lock per target, held across the whole
	// action.
	acting map[string]*sync.Mutex
	actMu  sync.Mutex

	kick chan struct{}
}

// New builds an engine.
func New(cfg *config.Config, st *state.Store, dr door.Door, log *slog.Logger) *Engine {
	return &Engine{
		cfg: cfg, st: st, dr: dr, log: log, now: time.Now,
		vals: map[string]Value{}, up: map[string]bool{}, upKnown: map[string]bool{},
		upSince: map[string]time.Time{}, okSince: map[string]time.Time{},
		blocked: map[string]string{}, pending: map[string]string{},
		lastErr: map[string]string{}, acts: map[string]int{}, seenSig: map[string]bool{},
		acting: map[string]*sync.Mutex{},
		firstErr: "no observation yet",
		kick:     make(chan struct{}, 1),
	}
}

// SetClock replaces the clock (tests).
func (e *Engine) SetClock(f func() time.Time) { e.now = f }

// Kick asks for a pass as soon as possible.
func (e *Engine) Kick() {
	select {
	case e.kick <- struct{}{}:
	default:
	}
}

// Run loops until ctx is done.
func (e *Engine) Run(ctx context.Context) {
	t := time.NewTicker(e.cfg.Interval.D())
	defer t.Stop()
	e.Pass(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			e.Pass(ctx)
		case <-e.kick:
			e.Pass(ctx)
		}
	}
}

// --- observing ------------------------------------------------------------

// evalSignal returns the current value of a signal, refreshing it if stale.
// A hold: signal is answered from the store, not from a node.
func (e *Engine) evalSignal(ctx context.Context, name string) Value {
	now := e.now()
	if target, ok := holdTarget(name); ok {
		v := Value{Known: true, True: len(e.st.On(target)) > 0, At: now}
		e.setVal(name, v)
		return v
	}
	sig, ok := e.cfg.Signals[name]
	if !ok {
		return Value{Known: false, At: now, Err: "signal not declared"}
	}
	e.mu.RLock()
	prev, had := e.vals[name]
	e.mu.RUnlock()
	if had && prev.Fresh(now, sig.TTL.D()) {
		return prev
	}
	if e.nodeIsDown(sig.RunOn) {
		v := Value{Known: false, At: now, Err: sig.RunOn + " is asleep"}
		e.setVal(name, v)
		return v
	}
	ans, err := e.dr.Signal(ctx, sig.RunOn, name)
	v := Value{At: now}
	if err != nil {
		v.Known = false
		v.Err = err.Error()
	} else {
		v.Known = true
		v.True = ans.True()
	}
	e.setVal(name, v)
	return v
}

func (e *Engine) setVal(name string, v Value) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.vals[name] = v
	if v.Known {
		e.seenSig[name] = true
	}
}

// probe asks a target's own state probe.
func (e *Engine) probe(ctx context.Context, name string) (up bool, known bool) {
	t := e.cfg.Targets[name]
	node := t.Node
	if t.Kind == config.KindNode {
		node = config.AnyControl // a sleeping node cannot answer for itself
	}
	if e.nodeIsDown(node) {
		e.mu.Lock()
		e.upKnown[name] = false
		e.up[name] = false
		delete(e.upSince, name)
		delete(e.okSince, name)
		e.mu.Unlock()
		return false, false
	}
	ans, err := e.dr.Act(ctx, node, "state", name)
	now := e.now()
	e.mu.Lock()
	defer e.mu.Unlock()
	if err != nil {
		e.upKnown[name] = false
		e.lastErr[name] = err.Error()
		return false, false
	}
	delete(e.lastErr, name)
	e.upKnown[name] = true
	e.up[name] = ans.True()
	if ans.True() {
		if _, seen := e.upSince[name]; !seen {
			e.upSince[name] = now
		}
	} else {
		delete(e.upSince, name)
		delete(e.okSince, name)
	}
	return ans.True(), true
}

// down reports whether `node` is a declared node target we have just
// observed as down. Dialling a machine we know is asleep costs a full ssh
// timeout per question and answers nothing: with six things to ask about a
// sleeping tower, a pass took minutes instead of seconds and /healthz stayed
// 503 long after start. A node that is down makes its questions UNKNOWN
// immediately — which is the same answer, and blocks a stop either way.
func (e *Engine) nodeIsDown(node string) bool {
	if node == "" || node == config.AnyControl {
		return false
	}
	for _, n := range e.cfg.TargetNames() {
		t := e.cfg.Targets[n]
		if t.Kind == config.KindNode && t.Node == node {
			e.mu.RLock()
			defer e.mu.RUnlock()
			return e.upKnown[n] && !e.up[n]
		}
	}
	return false
}

// Pass is one reconcile: observe, then consider stopping.
func (e *Engine) Pass(ctx context.Context) {
	anyKnown := false
	// nodes first, so everything after knows which machines are awake
	for _, name := range e.cfg.TargetNames() {
		if e.cfg.Targets[name].Kind != config.KindNode {
			continue
		}
		if _, known := e.probe(ctx, name); known {
			anyKnown = true
		}
	}
	for _, name := range e.cfg.TargetNames() {
		if e.cfg.Targets[name].Kind == config.KindNode {
			continue
		}
		if _, known := e.probe(ctx, name); known {
			anyKnown = true
		}
	}
	e.mu.Lock()
	if anyKnown {
		e.firstErr = ""
	} else if e.firstErr == "" {
		e.firstErr = "no target could be observed"
	}
	e.mu.Unlock()
	if !anyKnown {
		// FAIL AS-IS: if we cannot see the fleet we touch nothing at all.
		e.log.Error("no target could be observed — doing nothing this pass")
		return
	}

	// Ask every declared signal, not only the ones a stop decision happens to
	// need. Lazily-asked signals left the board and /metrics blank whenever
	// nothing was close to being stopped — so you could not see what the
	// watchman knew, which is most of the value of having a board at all.
	// Each answer is still cached for its ttl, so this costs one round per
	// signal per ttl window, not one per pass.
	for _, name := range e.cfg.SignalNames() {
		e.evalSignal(ctx, name)
	}

	e.stopPass(ctx)
}

// --- the stop path --------------------------------------------------------

func (e *Engine) stopPass(ctx context.Context) {
	// children before the things they run on: a down entry's ThenConsider
	// names what may be freed, so evaluate in Manages order and revisit.
	for _, name := range e.stopOrder() {
		e.considerDown(ctx, name)
	}
}

// stopOrder lists managed targets, guests before the nodes they run on.
func (e *Engine) stopOrder() []string {
	var guests, nodes []string
	for _, n := range e.cfg.TargetNames() {
		if !e.cfg.Managed(n) {
			continue
		}
		if e.cfg.Targets[n].Kind == config.KindNode {
			nodes = append(nodes, n)
		} else {
			guests = append(guests, n)
		}
	}
	sort.Strings(guests)
	sort.Strings(nodes)
	return append(guests, nodes...)
}

func (e *Engine) considerDown(ctx context.Context, name string) {
	t := e.cfg.Targets[name]
	d, hasDown := e.cfg.Downs[name]
	if !hasDown {
		return
	}
	e.mu.RLock()
	up, known := e.up[name], e.upKnown[name]
	upSince := e.upSince[name]
	e.mu.RUnlock()
	if !known || !up {
		e.clear(name)
		return
	}
	if e.st.HandsOff(name) {
		e.setBlocked(name, "hands-off")
		return
	}
	now := e.now()

	// min_uptime: a thing just raised is not yet idle. Between "raised" and
	// "in use" every activity signal honestly says no — that gap is how a
	// backup was lost (power.md §A.2 rule 4).
	if !upSince.IsZero() && now.Sub(upSince) < t.MinUptime.D() {
		e.setBlocked(name, "min_uptime")
		return
	}

	allTrue := true
	for _, ref := range d.StopWhen {
		sig, negated := config.SignalRef(ref)
		v := e.evalSignal(ctx, sig)
		if !v.Known {
			e.setBlocked(name, "unknown:"+sig)
			e.clearOK(name)
			return // UNKNOWN blocks a stop; it never permits one
		}
		e.mu.RLock()
		everSeen := e.seenSig[sig]
		e.mu.RUnlock()
		if !everSeen {
			e.setBlocked(name, "never-answered:"+sig)
			e.clearOK(name)
			return
		}
		want := v.True
		if negated {
			want = !want
		}
		if !want {
			e.setBlocked(name, "held-by:"+ref)
			e.clearOK(name)
			allTrue = false
			break
		}
	}
	if !allTrue {
		return
	}

	// every condition agrees; the grace runs from the moment they did
	e.mu.Lock()
	if _, ok := e.okSince[name]; !ok {
		e.okSince[name] = now
	}
	since := e.okSince[name]
	e.mu.Unlock()
	if now.Sub(since) < d.Grace.D() {
		e.setBlocked(name, "grace")
		return
	}

	e.setBlocked(name, "")

	// take the target's action lock, then look again: a wake may have been
	// raising it while we were deciding, and a decision taken before that
	// wake must not land after it.
	lock := e.lockFor(name)
	lock.Lock()
	defer lock.Unlock()
	if up, known := e.probe(ctx, name); !known || !up {
		return // it went away by itself
	}
	e.mu.RLock()
	freshUpSince := e.upSince[name]
	e.mu.RUnlock()
	if !freshUpSince.IsZero() && e.now().Sub(freshUpSince) < t.MinUptime.D() {
		e.setBlocked(name, "min_uptime") // it was raised while we deliberated
		return
	}

	e.setPending(name, "stopping")
	e.log.Info("stopping", "target", name, "kind", t.Kind,
		"quiet_for", now.Sub(since).Truncate(time.Second).String(),
		"because", d.StopWhen)
	node := t.Node
	if t.Kind == config.KindNode {
		node = t.Name // a node powers itself off; its own door refuses if it may not
	}
	_, err := e.dr.Act(ctx, node, "down", name)
	e.finish(name, "stop", err)
	if err == nil {
		e.mu.Lock()
		delete(e.upSince, name)
		delete(e.okSince, name)
		e.up[name] = false
		e.mu.Unlock()
		for _, next := range d.ThenConsider {
			e.considerDown(ctx, next)
		}
	}
}

// --- the wake path --------------------------------------------------------

// Wake raises a target and everything it needs, parents first. It returns
// when the chain is up or a timeout is hit; the caller decides whether to
// wait for it.
func (e *Engine) Wake(ctx context.Context, name, why string) error {
	chain := e.cfg.Chain(name)
	if chain == nil {
		return fmt.Errorf("no such target: %q", name)
	}
	for _, step := range chain {
		if e.st.HandsOff(step) {
			return fmt.Errorf("%s is hands-off — a person asked for it to be left alone", step)
		}
		if up, known := e.probe(ctx, step); known && up {
			continue
		}
		t := e.cfg.Targets[step]
		node := t.Node
		if t.Kind == config.KindNode {
			node = config.AnyControl
		}
		lock := e.lockFor(step)
		lock.Lock()
		// look again under the lock: another wake may have raised it while
		// we queued behind a stop
		if up, known := e.probe(ctx, step); known && up {
			lock.Unlock()
			continue
		}
		e.setPending(step, "raising")
		e.log.Info("raising", "target", step, "kind", t.Kind, "why", why)
		_, err := e.dr.Act(ctx, node, "up", step)
		e.finish(step, "up", err)
		if err != nil {
			lock.Unlock()
			return fmt.Errorf("raising %s: %w", step, err)
		}
		err = e.await(ctx, step, t.UpTimeout.D())
		lock.Unlock()
		if err != nil {
			return err
		}
	}
	e.Kick()
	return nil
}

// await polls a target's own state probe until it says up.
func (e *Engine) await(ctx context.Context, name string, timeout time.Duration) error {
	deadline := e.now().Add(timeout)
	for {
		if up, known := e.probe(ctx, name); known && up {
			e.setPending(name, "")
			return nil
		}
		if e.now().After(deadline) {
			e.setPending(name, "")
			return fmt.Errorf("%s did not come up within %s", name, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// lockFor returns the per-target action lock, creating it on first use.
func (e *Engine) lockFor(name string) *sync.Mutex {
	e.actMu.Lock()
	defer e.actMu.Unlock()
	l, ok := e.acting[name]
	if !ok {
		l = &sync.Mutex{}
		e.acting[name] = l
	}
	return l
}

func holdTarget(sig string) (string, bool) {
	if len(sig) > len(config.HoldPrefix) && sig[:len(config.HoldPrefix)] == config.HoldPrefix {
		return sig[len(config.HoldPrefix):], true
	}
	return "", false
}
