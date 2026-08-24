// Package fleet is the brain: it compares the claims to the world and moves
// the world, once, in dependency order.
//
// The rule, in one line: a target is up while any claim on it — or on
// anything that requires it — is held; it comes down when that set is empty,
// its grace has passed, and no guard objects.
package fleet

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/tomblancdev/veilleur/internal/config"
	"github.com/tomblancdev/veilleur/internal/door"
	"github.com/tomblancdev/veilleur/internal/store"
)

// TargetView is one row of the board.
type TargetView struct {
	config.Target
	Up            bool          `json:"up"`
	Known         bool          `json:"known"`
	Wanted        bool          `json:"wanted"`
	WantedBy      []string      `json:"wanted_by,omitempty"` // claim ids holding it, directly or through a dependent
	UnwantedFor   time.Duration `json:"unwanted_for,omitempty"`
	Blocked       string        `json:"blocked,omitempty"` // the guard refusing to stop it
	Pending       string        `json:"pending,omitempty"` // an action in flight
	LastError     string        `json:"last_error,omitempty"`
	LastChangedAt *time.Time    `json:"last_changed_at,omitempty"`
}

// Engine reconciles claims against the fleet.
type Engine struct {
	cfg  *config.Config
	st   *store.Store
	fl   door.Fleet
	log  *slog.Logger
	now  func() time.Time

	mu            sync.RWMutex
	snap          door.Snapshot
	snapErr       string
	unwantedSince map[string]time.Time
	lastError     map[string]string
	lastChanged   map[string]time.Time
	blocked       map[string]string
	pending       map[string]string

	// counters exported on /metrics
	wakes    map[string]int
	starts   map[string]int
	stops    map[string]int
	refusals map[string]int
	poweredSince map[string]time.Time
	poweredTotal map[string]time.Duration

	kick chan struct{}
}

// New builds an engine.
func New(cfg *config.Config, st *store.Store, fl door.Fleet, log *slog.Logger) *Engine {
	return &Engine{
		cfg: cfg, st: st, fl: fl, log: log,
		now:           time.Now,
		unwantedSince: map[string]time.Time{},
		lastError:     map[string]string{},
		lastChanged:   map[string]time.Time{},
		blocked:       map[string]string{},
		pending:       map[string]string{},
		wakes:         map[string]int{},
		starts:        map[string]int{},
		stops:         map[string]int{},
		refusals:      map[string]int{},
		poweredSince:  map[string]time.Time{},
		poweredTotal:  map[string]time.Duration{},
		kick:          make(chan struct{}, 1),
	}
}

// SetClock replaces the clock (tests).
func (e *Engine) SetClock(f func() time.Time) { e.now = f }

// Kick asks for a reconcile as soon as possible — every mutation calls it,
// so a claim taken through the API acts now instead of at the next tick.
func (e *Engine) Kick() {
	select {
	case e.kick <- struct{}{}:
	default:
	}
}

// Run reconciles on a ticker and whenever kicked, until ctx is done.
func (e *Engine) Run(ctx context.Context) {
	t := time.NewTicker(e.cfg.ReconcileInterval)
	defer t.Stop()
	e.Reconcile(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			e.Reconcile(ctx)
		case <-e.kick:
			e.Reconcile(ctx)
		}
	}
}

// onDemandNodes are the node targets — the only nodes we ever ask local
// questions of, and the only ones we may power off.
func (e *Engine) onDemandNodes() []string {
	var out []string
	for _, n := range e.cfg.TargetNames() {
		if t := e.cfg.Targets[n]; t.Kind == config.KindNode {
			out = append(out, t.Node)
		}
	}
	return out
}

// Reconcile is one pass: observe, decide, act.
func (e *Engine) Reconcile(ctx context.Context) {
	now := e.now()
	// an expiry is an event, not an absence
	for _, c := range e.st.Sweep(now) {
		e.log.Info("claim expired", "claim", c.ID, "target", c.Target, "subject", c.Subject, "rule", c.ReleasedBy)
	}

	snap, err := e.fl.Observe(ctx, e.onDemandNodes())
	if err != nil {
		// FAIL AS-IS (power.md decision 6): if we cannot see the fleet we
		// touch nothing at all. Never power off what you cannot reason about.
		e.mu.Lock()
		e.snapErr = err.Error()
		e.mu.Unlock()
		e.log.Error("observe failed — doing nothing this pass", "err", err)
		return
	}
	e.mu.Lock()
	e.snap, e.snapErr = snap, ""
	e.accountPowerLocked(snap, now)
	e.mu.Unlock()

	wanted, by := e.Wanted(now)
	order := e.cfg.Order()

	// UP pass, in dependency order: a parent is started before its children.
	for _, name := range order {
		t := e.cfg.Targets[name]
		if !wanted[name] || e.isUp(snap, t) || !t.Managed() {
			continue
		}
		if !e.requiresUp(snap, t) {
			continue // its parent is still coming up; next pass
		}
		e.bringUp(ctx, t, by[name], now)
	}

	// DOWN pass, in reverse: children stop before the thing they require.
	for i := len(order) - 1; i >= 0; i-- {
		name := order[i]
		t := e.cfg.Targets[name]
		if !e.isUp(snap, t) {
			e.clearUnwanted(name)
			continue
		}
		if wanted[name] {
			e.clearUnwanted(name)
			continue
		}
		if !t.Managed() {
			continue
		}
		e.considerDown(ctx, t, snap, now)
	}
}

// Wanted computes the desired state: every target with a held claim, plus
// everything those targets require, transitively. Returns the map and, for
// each target, the claim ids responsible.
func (e *Engine) Wanted(now time.Time) (map[string]bool, map[string][]string) {
	wanted := map[string]bool{}
	by := map[string][]string{}
	for _, c := range e.st.Held(now) {
		if _, ok := e.cfg.Targets[c.Target]; !ok {
			continue // a claim on a target that no longer exists holds nothing
		}
		wanted[c.Target] = true
		by[c.Target] = append(by[c.Target], c.ID)
	}
	// propagate down the graph until nothing changes: the tower is up
	// because the console is, and the console is up because someone plays.
	for changed := true; changed; {
		changed = false
		for name := range wanted {
			for _, r := range e.cfg.Targets[name].Requires {
				if !wanted[r] {
					wanted[r] = true
					changed = true
				}
				for _, id := range by[name] {
					if !contains(by[r], id) {
						by[r] = append(by[r], id)
					}
				}
			}
		}
	}
	for k := range by {
		sort.Strings(by[k])
	}
	return wanted, by
}

func (e *Engine) isUp(snap door.Snapshot, t config.Target) bool {
	if t.Kind == config.KindNode {
		return snap.NodeUp(t.Node)
	}
	return snap.Running(t.VMID)
}

func (e *Engine) requiresUp(snap door.Snapshot, t config.Target) bool {
	for _, r := range t.Requires {
		if !e.isUp(snap, e.cfg.Targets[r]) {
			return false
		}
	}
	return true
}

func (e *Engine) bringUp(ctx context.Context, t config.Target, why []string, now time.Time) {
	var err error
	switch t.Kind {
	case config.KindNode:
		if !t.WOL {
			return // nothing we know how to do; it must be woken by hand
		}
		e.setPending(t.Name, "waking")
		e.log.Info("waking", "target", t.Name, "node", t.Node, "claims", why)
		err = e.fl.Wake(ctx, t.Node)
		e.bump(e.wakes, t.Name)
	case config.KindGuest:
		e.setPending(t.Name, "starting")
		e.log.Info("starting", "target", t.Name, "vmid", t.VMID, "claims", why)
		err = e.fl.StartGuest(ctx, t.VMID)
		e.bump(e.starts, t.Name)
	}
	e.finish(t.Name, err, now)
}

// considerDown applies the grace, the dependents and the guards, in that
// order, and says out loud why it did not act.
func (e *Engine) considerDown(ctx context.Context, t config.Target, snap door.Snapshot, now time.Time) {
	// something that requires it is still up: wait for the child.
	for _, d := range e.cfg.Dependents(t.Name) {
		if e.isUp(snap, e.cfg.Targets[d]) {
			e.setBlocked(t.Name, "dependent:"+d)
			return
		}
	}
	since := e.markUnwanted(t.Name, now)
	if now.Sub(since) < t.DownGrace {
		e.setBlocked(t.Name, "grace")
		return
	}
	if g := e.veto(t, snap); g != "" {
		e.setBlocked(t.Name, "guard:"+g)
		e.bump(e.refusals, g)
		e.log.Info("stop refused", "target", t.Name, "guard", g)
		return
	}
	e.setBlocked(t.Name, "")
	var err error
	switch t.Kind {
	case config.KindNode:
		e.setPending(t.Name, "powering off")
		e.log.Info("powering off", "target", t.Name, "node", t.Node, "unwanted_for", now.Sub(since).String())
		err = e.fl.PowerOffNode(ctx, t.Node)
	case config.KindGuest:
		e.setPending(t.Name, "stopping")
		e.log.Info("stopping", "target", t.Name, "vmid", t.VMID, "unwanted_for", now.Sub(since).String())
		err = e.fl.StopGuest(ctx, t.VMID)
	}
	e.bump(e.stops, t.Name)
	e.finish(t.Name, err, now)
}

// veto returns the first guard refusing to stop this target, or "".
func (e *Engine) veto(t config.Target, snap door.Snapshot) string {
	for _, g := range t.Guards {
		switch g {
		case "human_session":
			n, ok := snap.Nodes[t.Node]
			// unknown (-1) counts as occupied: we cannot see, so we do not act
			if !ok || n.TTYs != 0 {
				return g
			}
		case "maintenance_lock":
			if snap.Locks["maintenance"] {
				return g
			}
		case "converge_lock":
			if snap.Locks["converge"] {
				return g
			}
		case "cluster_whole":
			// the fencing rule (architecture §4): never leave while the
			// 24/7 pair is short, or the survivor drops below quorum.
			if snap.ClusterTotal == 0 || snap.ClusterOnline < snap.ClusterTotal {
				return g
			}
		case "ha_resident":
			if gs, ok := snap.Guests[t.VMID]; ok && gs.HA {
				return g
			}
		}
	}
	// a hard boundary, not a configurable guard: an HA-managed guest is the
	// HA manager's to stop, never ours (power.md decision 7).
	if t.Kind == config.KindGuest {
		if gs, ok := snap.Guests[t.VMID]; ok && gs.HA {
			return "ha_resident"
		}
	}
	return ""
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
