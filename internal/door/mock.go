package door

import (
	"context"
	"fmt"
	"sync"
)

// Mock is an in-memory fleet: the engine can be driven end to end without a
// hypervisor anywhere.
type Mock struct {
	mu sync.Mutex

	// Signals: name -> exit code. Missing = unreachable (UNKNOWN).
	Signals map[string]int
	// State: target -> exit code of its `state` probe (0 = up).
	State map[string]int
	// Unreachable nodes refuse everything, as a dead door would.
	Unreachable map[string]bool
	// Actions records every side effect, in order.
	Actions []string
	// OnUp/OnDown let a test model the world changing.
	OnUp, OnDown func(target string)
	Known        []string
	// Says: "<verb> <target>" -> what that command prints on stdout. A real
	// node's up/down command may report something the watchman must record
	// (the tower's `down` prints the wake backstop it just armed).
	Says map[string]string
	// Refuses: "<verb> <target>" -> exit code, for an action the node runs
	// and REFUSES. Real ones exist: a hypervisor that cannot start a guest,
	// a node whose allowlist does not carry that target.
	Refuses map[string]int
	// Complains: "<verb> <target>" -> what it says on stderr while refusing.
	Complains map[string]string
}

// NewMock builds an empty fleet.
func NewMock(nodes ...string) *Mock {
	return &Mock{
		Signals: map[string]int{}, State: map[string]int{},
		Unreachable: map[string]bool{}, Known: nodes,
		Says:    map[string]string{},
		Refuses: map[string]int{}, Complains: map[string]string{},
	}
}

// Nodes lists the doors.
func (m *Mock) Nodes() []string { return m.Known }

func (m *Mock) reachable(node string) bool {
	if node == "any_control" || node == "" {
		for _, n := range m.Known {
			if !m.Unreachable[n] {
				return true
			}
		}
		return false
	}
	return !m.Unreachable[node]
}

// Signal answers a named question.
func (m *Mock) Signal(_ context.Context, node, name string) (Answer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.reachable(node) {
		return Answer{}, &ErrUnreachable{Node: node, Err: fmt.Errorf("mock: down")}
	}
	code, ok := m.Signals[name]
	if !ok {
		return Answer{}, &ErrUnreachable{Node: node, Err: fmt.Errorf("mock: no such signal %q", name)}
	}
	return Answer{Node: node, Exit: code}, nil
}

// Act runs up | down | state.
func (m *Mock) Act(_ context.Context, node, verb, target string) (Answer, error) {
	m.mu.Lock()
	if !m.reachable(node) {
		m.mu.Unlock()
		return Answer{}, &ErrUnreachable{Node: node, Err: fmt.Errorf("mock: down")}
	}
	switch verb {
	case "state":
		code, ok := m.State[target]
		if !ok {
			code = 1
		}
		m.mu.Unlock()
		return Answer{Node: node, Exit: code}, nil
	case "up", "down":
		m.Actions = append(m.Actions, verb+" "+target)
		key := verb + " " + target
		up, down := m.OnUp, m.OnDown
		said := m.Says[key]
		code, refused := m.Refuses[key]
		grumble := m.Complains[key]
		m.mu.Unlock()
		if refused {
			// the node WAS reached and said no: an error, never a silent
			// success the caller then waits on
			ans := Answer{Node: node, Exit: code, Stdout: said, Stderr: grumble}
			return ans, &ErrActionFailed{
				Node: node, Verb: verb, Target: target,
				Exit: code, Stderr: grumble,
			}
		}
		if verb == "up" && up != nil {
			up(target)
		}
		if verb == "down" && down != nil {
			down(target)
		}
		return Answer{Node: node, Stdout: said}, nil
	}
	m.mu.Unlock()
	return Answer{}, fmt.Errorf("mock: bad verb %q", verb)
}

// Took reports whether the action happened.
func (m *Mock) Took(action string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range m.Actions {
		if a == action {
			return true
		}
	}
	return false
}

// Reset clears the action log.
func (m *Mock) Reset() { m.mu.Lock(); m.Actions = nil; m.mu.Unlock() }

// SetUp marks a target up (0) or down (1) in the mock's world.
func (m *Mock) SetUp(target string, up bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if up {
		m.State[target] = 0
	} else {
		m.State[target] = 1
	}
}
