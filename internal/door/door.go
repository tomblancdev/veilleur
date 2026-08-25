// Package door is how Le Veilleur touches machines: one ssh key restricted to
// one forced command on every hypervisor.
//
// The key point, and the reason a signal's `cmd` is not sent over the wire:
// **the node holds the commands, Le Veilleur only names them.** Le Veilleur
// may ask for `signal console_in_use`; it cannot ask for `rm -rf /`. The
// allowlist is rendered onto each node beside the script, so a compromised
// watchman is limited to the questions and actions that node agreed to.
package door

import (
	"context"
	"fmt"
	"strings"
)

// Answer is the result of running a named thing on a node.
type Answer struct {
	Node   string
	Exit   int
	Stdout string
}

// True reports the exit-0 convention.
func (a Answer) True() bool { return a.Exit == 0 }

// Door runs named commands on nodes. Everything the engine does goes
// through these three verbs.
type Door interface {
	// Signal asks a named question on a node. `node` may be AnyControl.
	Signal(ctx context.Context, node, name string) (Answer, error)
	// Act runs a named action for a target: "up", "down" or "state".
	Act(ctx context.Context, node, verb, target string) (Answer, error)
	// Nodes lists the doors we hold, for the board.
	Nodes() []string
}

// ErrUnreachable means the question could not be put — which is UNKNOWN to
// the engine, and UNKNOWN never permits a stop.
type ErrUnreachable struct {
	Node string
	Err  error
}

func (e *ErrUnreachable) Error() string { return fmt.Sprintf("door %s: %v", e.Node, e.Err) }
func (e *ErrUnreachable) Unwrap() error { return e.Err }

// safeName refuses anything that is not a bare identifier, so a name can
// never carry shell syntax to the far side even if the config is wrong.
func safeName(s string) error {
	if s == "" {
		return fmt.Errorf("empty name")
	}
	for _, r := range s {
		ok := r == '_' || r == '-' || r == '.' ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if !ok {
			return fmt.Errorf("name %q contains %q", s, string(r))
		}
	}
	return nil
}

func safeVerb(v string) error {
	switch v {
	case "up", "down", "state":
		return nil
	}
	return fmt.Errorf("verb %q is not up|down|state", v)
}

var _ = strings.TrimSpace
