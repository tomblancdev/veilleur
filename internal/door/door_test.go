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
