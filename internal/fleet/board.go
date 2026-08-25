package fleet

import (
	"fmt"
	"sort"
	"time"

	"github.com/tomblancdev/veilleur/internal/config"
	"github.com/tomblancdev/veilleur/internal/state"
)

// --- the little book-keeping the board and /metrics read ------------------

func (e *Engine) setBlocked(name, why string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if why == "" {
		delete(e.blocked, name)
		return
	}
	e.blocked[name] = why
}

func (e *Engine) clearOK(name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.okSince, name)
}

func (e *Engine) clear(name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.blocked, name)
	delete(e.okSince, name)
}

func (e *Engine) setPending(name, what string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if what == "" {
		delete(e.pending, name)
		return
	}
	e.pending[name] = what
}

func (e *Engine) finish(name, verb string, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.pending, name)
	e.acts[name+"/"+verb]++
	if err != nil {
		e.lastErr[name] = err.Error()
		e.log.Error("action failed", "target", name, "verb", verb, "err", err)
		return
	}
	delete(e.lastErr, name)
}

// TargetView is one row of the board.
type TargetView struct {
	config.Target
	Up        bool          `json:"up"`
	Known     bool          `json:"known"`
	Managed   bool          `json:"managed"`
	UpFor     config.Duration `json:"up_for,omitempty"`
	QuietFor  config.Duration `json:"quiet_for,omitempty"`
	Blocked   string        `json:"blocked,omitempty"`
	Pending   string        `json:"pending,omitempty"`
	LastError string        `json:"last_error,omitempty"`
	StopWhen  []string      `json:"stop_when,omitempty"`
	Holds     []state.Hold  `json:"holds,omitempty"`
}

// Board is the whole picture.
type Board struct {
	At         time.Time        `json:"at"`
	ObserveErr string           `json:"observe_error,omitempty"`
	Targets    []TargetView     `json:"targets"`
	Signals    map[string]Value `json:"signals"`
}

// Board renders the current state.
func (e *Engine) Board() Board {
	now := e.now()
	e.mu.RLock()
	defer e.mu.RUnlock()
	b := Board{At: now, ObserveErr: e.firstErr, Signals: map[string]Value{}}
	for k, v := range e.vals {
		b.Signals[k] = v
	}
	for _, name := range e.cfg.TargetNames() {
		v := TargetView{
			Target:  e.cfg.Targets[name],
			Up:      e.up[name],
			Known:   e.upKnown[name],
			Managed: e.cfg.Managed(name),
			Blocked: e.blocked[name],
			Pending: e.pending[name],
			Holds:   e.st.On(name),
		}
		v.LastError = e.lastErr[name]
		if d, ok := e.cfg.Downs[name]; ok {
			v.StopWhen = d.StopWhen
		}
		if since, ok := e.upSince[name]; ok {
			v.UpFor = config.Duration(now.Sub(since).Truncate(time.Second))
		}
		if since, ok := e.okSince[name]; ok {
			v.QuietFor = config.Duration(now.Sub(since).Truncate(time.Second))
		}
		b.Targets = append(b.Targets, v)
	}
	return b
}

// Healthy reports whether the last pass could see anything at all.
func (e *Engine) Healthy() (bool, string) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.firstErr == "", e.firstErr
}

// Config exposes the graph.
func (e *Engine) Config() *config.Config { return e.cfg }

// Chain is what a wake of this target implies.
func (e *Engine) Chain(name string) []string { return e.cfg.Chain(name) }

// Metrics renders the Prometheus text exposition.
func (e *Engine) Metrics() string {
	now := e.now()
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := ""
	add := func(f string, a ...any) { out += fmt.Sprintf(f, a...) }

	add("# HELP veilleur_target_up Whether a target is observed up.\n# TYPE veilleur_target_up gauge\n")
	for _, n := range e.cfg.TargetNames() {
		up := 0
		if e.up[n] {
			up = 1
		}
		add("veilleur_target_up{target=%q,kind=%q} %d\n", n, e.cfg.Targets[n].Kind, up)
	}
	add("# HELP veilleur_target_managed Whether Le Veilleur may stop this target.\n# TYPE veilleur_target_managed gauge\n")
	for _, n := range e.cfg.TargetNames() {
		m := 0
		if e.cfg.Managed(n) {
			m = 1
		}
		add("veilleur_target_managed{target=%q} %d\n", n, m)
	}
	// The one an alert needs: a target that is up, is ours to stop, and whose
	// every condition already agrees it MAY stop. If that persists, the
	// watchman is not doing its job — which is the failure this whole product
	// exists to prevent, so it must be visible from outside.
	add("# HELP veilleur_target_stoppable Managed, up, and every stop_when condition agrees it may stop.\n# TYPE veilleur_target_stoppable gauge\n")
	for _, n := range e.cfg.TargetNames() {
		v := 0
		if _, agreed := e.okSince[n]; agreed && e.up[n] && e.cfg.Managed(n) {
			v = 1
		}
		add("veilleur_target_stoppable{target=%q} %d\n", n, v)
	}
	add("# HELP veilleur_target_quiet_seconds How long every stop_when condition has agreed.\n# TYPE veilleur_target_quiet_seconds gauge\n")
	for _, n := range e.cfg.TargetNames() {
		q := 0.0
		if since, ok := e.okSince[n]; ok {
			q = now.Sub(since).Seconds()
		}
		add("veilleur_target_quiet_seconds{target=%q} %.0f\n", n, q)
	}

	add("# HELP veilleur_signal Last answer of a signal: 1 true, 0 false, -1 unknown.\n# TYPE veilleur_signal gauge\n")
	names := make([]string, 0, len(e.vals))
	for k := range e.vals {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		v := e.vals[k]
		val := -1
		if v.Known && v.True {
			val = 1
		} else if v.Known {
			val = 0
		}
		add("veilleur_signal{signal=%q} %d\n", k, val)
	}
	add("# HELP veilleur_holds Holds a person has placed on a target.\n# TYPE veilleur_holds gauge\n")
	held := map[string]int{}
	for _, h := range e.st.All() {
		held[h.Target]++
	}
	for _, n := range e.cfg.TargetNames() {
		add("veilleur_holds{target=%q} %d\n", n, held[n])
	}
	add("# HELP veilleur_hold_age_seconds How long the oldest hold on a target has stood.\n# TYPE veilleur_hold_age_seconds gauge\n")
	for _, n := range e.cfg.TargetNames() {
		age := 0.0
		for _, h := range e.st.On(n) {
			if a := now.Sub(h.Since).Seconds(); a > age {
				age = a
			}
		}
		add("veilleur_hold_age_seconds{target=%q} %.0f\n", n, age)
	}
	add("# HELP veilleur_actions_total Actions taken.\n# TYPE veilleur_actions_total counter\n")
	keys := make([]string, 0, len(e.acts))
	for k := range e.acts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t, verb := k[:len(k)-len("/up")], k[len(k)-2:]
		if k[len(k)-5:] == "/stop" {
			t, verb = k[:len(k)-5], "stop"
		}
		add("veilleur_actions_total{target=%q,action=%q} %d\n", t, verb, e.acts[k])
	}
	ok := 0
	if e.firstErr == "" {
		ok = 1
	}
	add("# HELP veilleur_observe_ok Whether the last pass could observe the fleet.\n# TYPE veilleur_observe_ok gauge\nveilleur_observe_ok %d\n", ok)
	return out
}
