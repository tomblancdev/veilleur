package door

import (
	"strings"
	"testing"
)

// The rule that was missing on 2026-08-26: a question and an action read the
// same exit code and must not mean the same thing by it.
func TestAnExitCodeMeansOneThingForAQuestionAndAnotherForAnAction(t *testing.T) {
	cases := []struct {
		name    string
		verb    string
		exit    int
		wantErr bool
	}{
		{"a state probe saying `down` is an ANSWER", "state", 1, false},
		{"a state probe saying `up` is an answer too", "state", 0, false},
		{"an `up` that succeeded is fine", "up", 0, false},
		{"an `up` the node REFUSED is a failure", "up", 2, true},
		{"a `down` the node REFUSED is a failure", "down", 1, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ans := Answer{Node: "a-node", Exit: c.exit, Stderr: "it said why"}
			got, err := judge(c.verb, "a-target", ans)
			if c.wantErr && err == nil {
				t.Fatalf("%s %d: wanted an error", c.verb, c.exit)
			}
			if !c.wantErr && err != nil {
				t.Fatalf("%s %d: wanted an answer, got %v", c.verb, c.exit, err)
			}
			if got.Exit != c.exit {
				t.Fatalf("the exit code must survive: %d != %d", got.Exit, c.exit)
			}
		})
	}
}

// The reason a refusal is worth turning into an error at all: the node's own
// sentence has to reach whoever is reading the log.
func TestARefusalCarriesTheNodesOwnWords(t *testing.T) {
	ans := Answer{Node: "a-node", Exit: 2, Stderr: "cannot start: the card is busy"}
	_, err := judge("up", "a-target", ans)
	if err == nil {
		t.Fatal("wanted an error")
	}
	for _, want := range []string{"up", "a-target", "a-node", "cannot start: the card is busy"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the error must name %q: %v", want, err)
		}
	}
}

// A refusal with nothing on stderr must still be legible rather than blank.
func TestASilentRefusalStillSaysSomething(t *testing.T) {
	_, err := judge("down", "a-target", Answer{Node: "a-node", Exit: 1})
	if err == nil || !strings.Contains(err.Error(), "no output") {
		t.Fatalf("wanted a legible error, got %v", err)
	}
}

// The dangerous half of the same rule: a node that CANNOT RUN the question
// has not answered it. Read as a plain non-zero exit, "command not found"
// becomes a confident "no" — and the signals that say a machine is in use are
// exactly the ones that then say it is idle. A broken door must make the
// watchman blind, never decisive.
func TestACommandThatCouldNotRunIsUnknownNotNo(t *testing.T) {
	for _, exit := range []int{126, 127} {
		ans := Answer{Node: "a-node", Exit: exit, Stderr: "sh: no such file"}
		if _, err := judge("state", "a-target", ans); err == nil {
			t.Fatalf("state, exit %d: must be UNKNOWN, not a clean 'down'", exit)
		}
		if _, err := judge("up", "a-target", ans); err == nil {
			t.Fatalf("up, exit %d: must be UNKNOWN", exit)
		}
		var unreachable *ErrUnreachable
		_, err := judge("state", "a-target", ans)
		if !errorsAs(err, &unreachable) {
			t.Fatalf("exit %d must be the door's UNKNOWN (ErrUnreachable), got %T", exit, err)
		}
	}
}

// ...and an ordinary "no" must stay an ordinary no, or every idle machine
// reads as unknown and nothing is ever stopped again.
func TestAPlainNoIsStillAnAnswer(t *testing.T) {
	if _, err := judge("state", "a-target", Answer{Node: "a-node", Exit: 1}); err != nil {
		t.Fatalf("exit 1 on a question is an ANSWER: %v", err)
	}
}

func errorsAs(err error, target **ErrUnreachable) bool {
	for err != nil {
		if e, ok := err.(*ErrUnreachable); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
