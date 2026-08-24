package fleet

import (
	"fmt"
	"sort"
	"time"

	"github.com/tomblancdev/veilleur/internal/config"
	"github.com/tomblancdev/veilleur/internal/door"
)

// --- the little book-keeping the board and /metrics read ------------------

func (e *Engine) markUnwanted(name string, now time.Time) time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	if t, ok := e.unwantedSince[name]; ok {
		return t
	}
	e.unwantedSince[name] = now
	return now
}

func (e *Engine) clearUnwanted(name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.unwantedSince, name)
	delete(e.blocked, name)
}

func (e *Engine) setBlocked(name, why string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if why == "" {
		delete(e.blocked, name)
		return
	}
	e.blocked[name] = why
}

func (e *Engine) setPending(name, what string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.pending[name] = what
}

func (e *Engine) finish(name string, err error, now time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.pending, name)
	if err != nil {
		e.lastError[name] = err.Error()
		e.log.Error("action failed", "target", name, "err", err)
		return
	}
	delete(e.lastError, name)
	e.lastChanged[name] = now
	delete(e.unwantedSince, name)
}

func (e *Engine) bump(m map[string]int, k string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	m[k]++
}

// accountPowerLocked keeps a running total of how long each node has been
// powered — the number that tells us whether any of this worked.
func (e *Engine) accountPowerLocked(snap door.Snapshot, now time.Time) {
	for _, name := range e.cfg.TargetNames() {
		t := e.cfg.Targets[name]
		if t.Kind != config.KindNode {
			continue
		}
		up := snap.NodeUp(t.Node)
		since, tracking := e.poweredSince[name]
		switch {
		case up && !tracking:
			e.poweredSince[name] = now
		case !up && tracking:
			e.poweredTotal[name] += now.Sub(since)
			delete(e.poweredSince, name)
		}
	}
}

// Board is the whole picture, for the page and the API.
type Board struct {
	At        time.Time    `json:"at"`
	Source    string       `json:"source"`
	ObserveErr string      `json:"observe_error,omitempty"`
	Targets   []TargetView `json:"targets"`
}

// Board renders the current state.
func (e *Engine) Board() Board {
	now := e.now()
	wanted, by := e.Wanted(now)
	e.mu.RLock()
	defer e.mu.RUnlock()
	b := Board{At: e.snap.At, Source: e.snap.Source, ObserveErr: e.snapErr}
	for _, name := range e.cfg.TargetNames() {
		t := e.cfg.Targets[name]
		v := TargetView{Target: t, Wanted: wanted[name], WantedBy: by[name]}
		if t.Kind == config.KindNode {
			n, ok := e.snap.Nodes[t.Node]
			v.Known, v.Up = ok, ok && n.Online
		} else {
			g, ok := e.snap.Guests[t.VMID]
			v.Known, v.Up = ok, ok && g.Status == "running"
		}
		if since, ok := e.unwantedSince[name]; ok {
			v.UnwantedFor = config.Duration(now.Sub(since).Truncate(time.Second))
		}
		v.Blocked = e.blocked[name]
		v.Pending = e.pending[name]
		v.LastError = e.lastError[name]
		if ts, ok := e.lastChanged[name]; ok {
			t := ts
			v.LastChangedAt = &t
		}
		b.Targets = append(b.Targets, v)
	}
	return b
}

// Snapshot exposes the last observation (the API's /fleet).
func (e *Engine) Snapshot() door.Snapshot {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.snap
}

// Healthy reports whether the last observation succeeded.
func (e *Engine) Healthy() (bool, string) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.snapErr == "", e.snapErr
}

// Metrics renders the Prometheus text exposition for the engine's counters.
func (e *Engine) Metrics() string {
	now := e.now()
	wanted, _ := e.Wanted(now)
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := ""
	add := func(s string) { out += s }

	add("# HELP veilleur_target_up Whether a target is observed up (1) or down (0).\n# TYPE veilleur_target_up gauge\n")
	for _, name := range e.cfg.TargetNames() {
		t := e.cfg.Targets[name]
		up := 0
		if t.Kind == config.KindNode {
			if n, ok := e.snap.Nodes[t.Node]; ok && n.Online {
				up = 1
			}
		} else if g, ok := e.snap.Guests[t.VMID]; ok && g.Status == "running" {
			up = 1
		}
		add(fmt.Sprintf("veilleur_target_up{target=%q,kind=%q} %d\n", name, t.Kind, up))
	}

	add("# HELP veilleur_target_wanted Whether any held claim keeps a target up.\n# TYPE veilleur_target_wanted gauge\n")
	for _, name := range e.cfg.TargetNames() {
		w := 0
		if wanted[name] {
			w = 1
		}
		add(fmt.Sprintf("veilleur_target_wanted{target=%q} %d\n", name, w))
	}

	add("# HELP veilleur_claims_held Claims currently holding a target up.\n# TYPE veilleur_claims_held gauge\n")
	counts := map[string]int{}
	for _, c := range e.st.Held(now) {
		counts[c.Target]++
	}
	for _, name := range e.cfg.TargetNames() {
		add(fmt.Sprintf("veilleur_claims_held{target=%q} %d\n", name, counts[name]))
	}

	add("# HELP veilleur_actions_total Actions taken, by kind.\n# TYPE veilleur_actions_total counter\n")
	for _, name := range e.cfg.TargetNames() {
		add(fmt.Sprintf("veilleur_actions_total{target=%q,action=\"wake\"} %d\n", name, e.wakes[name]))
		add(fmt.Sprintf("veilleur_actions_total{target=%q,action=\"start\"} %d\n", name, e.starts[name]))
		add(fmt.Sprintf("veilleur_actions_total{target=%q,action=\"stop\"} %d\n", name, e.stops[name]))
	}

	add("# HELP veilleur_stop_refused_total Stops a guard refused, by guard.\n# TYPE veilleur_stop_refused_total counter\n")
	guards := make([]string, 0, len(e.refusals))
	for g := range e.refusals {
		guards = append(guards, g)
	}
	sort.Strings(guards)
	for _, g := range guards {
		add(fmt.Sprintf("veilleur_stop_refused_total{guard=%q} %d\n", g, e.refusals[g]))
	}

	add("# HELP veilleur_node_powered_seconds_total How long each on-demand node has been powered.\n# TYPE veilleur_node_powered_seconds_total counter\n")
	for _, name := range e.cfg.TargetNames() {
		t := e.cfg.Targets[name]
		if t.Kind != config.KindNode {
			continue
		}
		total := e.poweredTotal[name]
		if since, ok := e.poweredSince[name]; ok {
			total += now.Sub(since)
		}
		add(fmt.Sprintf("veilleur_node_powered_seconds_total{target=%q,node=%q} %.0f\n", name, t.Node, total.Seconds()))
	}

	ok := 0.0
	if e.snapErr == "" {
		ok = 1
	}
	add("# HELP veilleur_observe_ok Whether the last observation of the fleet succeeded.\n# TYPE veilleur_observe_ok gauge\n")
	add(fmt.Sprintf("veilleur_observe_ok %.0f\n", ok))
	return out
}

// Chain returns the target and everything it requires, in the order they
// must come up — what the caller of `ensure` is really waiting for.
func (e *Engine) Chain(name string) []TargetView {
	board := e.Board()
	views := map[string]TargetView{}
	for _, v := range board.Targets {
		views[v.Name] = v
	}
	need := map[string]bool{}
	var walk func(string)
	walk = func(n string) {
		if need[n] {
			return
		}
		need[n] = true
		for _, r := range e.cfg.Targets[n].Requires {
			walk(r)
		}
	}
	if _, ok := e.cfg.Targets[name]; !ok {
		return nil
	}
	walk(name)
	var out []TargetView
	for _, n := range e.cfg.Order() {
		if need[n] {
			out = append(out, views[n])
		}
	}
	return out
}

// ETA is a rough "how long until this is usable", summing the up timeouts
// of everything in the chain that is not up yet. Honest, not precise — the
// page says "about", and the real number lands in the metrics.
func (e *Engine) ETA(name string) time.Duration {
	var eta time.Duration
	for _, v := range e.Chain(name) {
		if !v.Up {
			eta += v.UpTimeout.D()
		}
	}
	return eta
}

// Config exposes the graph (the API's /targets).
func (e *Engine) Config() *config.Config { return e.cfg }

// Target looks one up.
func (e *Engine) Target(name string) (config.Target, bool) {
	t, ok := e.cfg.Targets[name]
	return t, ok
}
