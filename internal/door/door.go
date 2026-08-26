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
	// Stderr is the node's own explanation when something goes wrong. It is
	// carried because it used to be dropped: a `up <target>` that failed
	// exited non-zero, said why on stderr, and the watchman recorded a
	// SUCCESS - the caller then waited out the whole up_timeout and reported
	// only "did not come up", with the one useful sentence thrown away.
	Stderr string
}

// True reports the exit-0 convention. It answers a QUESTION - a signal, or a
// `state` probe - where a non-zero exit means "no". It is not how an ACTION
// is judged: see ErrActionFailed.
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

// ErrActionFailed means the command DID run and refused: the node was
// reached, it answered, and the answer was no.
//
// The distinction this type exists for: for a signal or a `state` probe a
// non-zero exit is an ANSWER (see Answer.True); for `up` and `down` it is a
// FAILURE. Both travelled the same path once, so an action that could not be
// carried out was indistinguishable from one that was - the caller went on to
// wait for a thing that was never going to happen.
type ErrActionFailed struct {
	Node   string
	Verb   string
	Target string
	Exit   int
	Stderr string
}

func (e *ErrActionFailed) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		msg = "no output"
	}
	return fmt.Sprintf("%s %s on %s exited %d: %s", e.Verb, e.Target, e.Node, e.Exit, msg)
}

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


// A shell that could not run the thing at all: 127 = not found, 126 = found
// but not executable. Neither is an answer to anything.
const (
	exitNotFound      = 127
	exitNotExecutable = 126
)

// cannotRun reports that the door reached the node and the node could not
// even run the command — which is UNKNOWN, not "no".
//
// This is the rule that was missing on 2026-08-26, and it is the dangerous
// half. A node's door was renamed and one stale authorized_keys line was left
// behind pointing at the deleted script, so every question asked on that node
// exited 127. Answer.True() maps every non-zero exit to false, so four
// signals — "is anybody streaming", "is a backup client talking", "is any
// guest running", "is a human logged in" — all answered NO, confidently. Each
// one is a reason to keep that machine up, so a broken door did not make the
// watchman cautious: it disarmed every guard at once, and a stop was decided
// on a tower with a virtual machine running on it. The engine already refuses
// to stop on UNKNOWN; it was never given the chance to, because the door
// reported a clean answer.
func cannotRun(ans Answer) bool {
	return ans.Exit == exitNotFound || ans.Exit == exitNotExecutable
}

// judge applies the one rule that separates a question from an action.
//
// `state` is a QUESTION - "is it up?" - and a non-zero exit is its answer.
// `up` and `down` are ACTIONS: the node was reached and refused, and that is
// an error carrying whatever it said. Both used to take the question's path,
// so an action that failed was reported as done and the caller waited for a
// thing that was never going to happen.
func judge(verb, target string, ans Answer) (Answer, error) {
	if cannotRun(ans) {
		return ans, unrunnable(ans)
	}
	if verb == "state" || ans.Exit == 0 {
		return ans, nil
	}
	return ans, &ErrActionFailed{
		Node: ans.Node, Verb: verb, Target: target,
		Exit: ans.Exit, Stderr: ans.Stderr,
	}
}

// unrunnable turns "the node could not run it" into the door's UNKNOWN.
func unrunnable(ans Answer) error {
	msg := strings.TrimSpace(ans.Stderr)
	if msg == "" {
		msg = fmt.Sprintf("exit %d", ans.Exit)
	}
	return &ErrUnreachable{Node: ans.Node, Err: fmt.Errorf("could not run it: %s", msg)}
}
