package config

import (
	"strings"
	"testing"
)

func base() *Config {
	return &Config{
		DoorCfg: Door{Mode: "mock"},
		Targets: map[string]Target{
			"muscle1": {Kind: KindNode, Node: "muscle1", OnDemand: true},
			"console": {Kind: KindGuest, VMID: 5001, Node: "muscle1", Requires: []string{"muscle1"}},
		},
	}
}

// The hard boundary: a node that has not declared itself on-demand can never
// become a target, so a 24/7 node cannot be powered off by a typo.
func TestNodeTargetMustBeOnDemand(t *testing.T) {
	c := base()
	c.Targets["infra1"] = Target{Kind: KindNode, Node: "infra1"}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "on_demand") {
		t.Fatalf("want an on_demand refusal, got %v", err)
	}
}

func TestRejectsUnknownRequiresAndCycles(t *testing.T) {
	c := base()
	c.Targets["console"] = Target{Kind: KindGuest, VMID: 5001, Node: "muscle1", Requires: []string{"nowhere"}}
	if err := c.Validate(); err == nil {
		t.Fatal("a requires pointing nowhere must be refused")
	}

	c = base()
	c.Targets["a"] = Target{Kind: KindGuest, VMID: 1, Node: "muscle1", Requires: []string{"b"}}
	c.Targets["b"] = Target{Kind: KindGuest, VMID: 2, Node: "muscle1", Requires: []string{"a"}}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("want a cycle refusal, got %v", err)
	}
}

func TestRejectsUnknownGuard(t *testing.T) {
	c := base()
	tt := c.Targets["muscle1"]
	tt.Guards = []string{"vibes"}
	c.Targets["muscle1"] = tt
	if err := c.Validate(); err == nil {
		t.Fatal("an unknown guard must be refused, not silently ignored")
	}
}

// Order puts everything a target requires before it.
func TestOrderIsDependencyOrder(t *testing.T) {
	c := base()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	order := c.Order()
	var iNode, iGuest = -1, -1
	for i, n := range order {
		switch n {
		case "muscle1":
			iNode = i
		case "console":
			iGuest = i
		}
	}
	if iNode < 0 || iGuest < 0 || iNode > iGuest {
		t.Fatalf("the node must come before the guest that requires it: %v", order)
	}
}

// The shipped example is the documentation: if it stops loading, the docs
// are lying. It also exercises the duration form people actually type.
func TestExampleConfigLoads(t *testing.T) {
	c, err := Load("../../targets.example.yaml")
	if err != nil {
		t.Fatalf("targets.example.yaml must be valid: %v", err)
	}
	m, ok := c.Targets["muscle1"]
	if !ok {
		t.Fatal("the example should declare the on-demand node")
	}
	if m.DownGrace.D().Minutes() != 10 {
		t.Errorf("down_grace 10m should parse, got %s", m.DownGrace.D())
	}
	if c.Targets["console"].IdleAfter.D().Minutes() != 20 {
		t.Errorf("idle_after 20m should parse, got %s", c.Targets["console"].IdleAfter.D())
	}
	if c.ReconcileInterval.D().Seconds() != 30 {
		t.Errorf("reconcile_interval 30s should parse, got %s", c.ReconcileInterval.D())
	}
	// defaults filled in where the file says nothing
	if c.Targets["pbs"].MaxHold.D().Hours() != 6 {
		t.Errorf("pbs caps holds at 6h, got %s", c.Targets["pbs"].MaxHold.D())
	}
	if c.Targets["console"].MaxHold.D() != c.DefaultHold.D() {
		t.Errorf("a target with no max_hold should inherit default_hold, got %s", c.Targets["console"].MaxHold.D())
	}
}
