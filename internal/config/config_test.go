package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func lab(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write(t, dir, "main.yaml", "house: Example House\ninterval: 30s\ndoor: { mode: mock }\n")
	write(t, dir, "signals/fleet.yaml", `
console_in_use: { run_on: tower, means: "somebody is streaming", ttl: 90s }
any_guest_running: { run_on: tower, means: "a guest is running" }
`)
	write(t, dir, "targets/tower.yaml", `
tower: { kind: node, node: tower, on_demand: true, up_timeout: 3m, down_grace: 10m }
`)
	write(t, dir, "targets/console.yaml", `
console: { kind: guest, node: tower, needs: [tower], min_uptime: 10m }
`)
	write(t, dir, "down/all.yaml", `
console: { stop_when: ["!console_in_use"], grace: 2m, manages: [console], then_consider: [tower] }
tower: { stop_when: ["!any_guest_running"], grace: 10m, manages: [tower] }
`)
	return dir
}

func TestLoadsThreeDirectories(t *testing.T) {
	c, err := Load(lab(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Signals) != 2 || len(c.Targets) != 2 || len(c.Downs) != 2 {
		t.Fatalf("signals=%d targets=%d downs=%d", len(c.Signals), len(c.Targets), len(c.Downs))
	}
	if c.Signals["console_in_use"].TTL.D() != 90*time.Second {
		t.Errorf("ttl should parse: %s", c.Signals["console_in_use"].TTL.D())
	}
	if c.Targets["console"].MinUptime.D() != 10*time.Minute {
		t.Errorf("min_uptime should parse: %s", c.Targets["console"].MinUptime.D())
	}
	// defaults fill in where the file is silent
	if c.Targets["tower"].MinUptime.D() == 0 {
		t.Error("min_uptime must never be zero — that is the gap that lost a backup")
	}
}

// The hard boundary: a node that has not declared itself on-demand can never
// be a target, so a 24/7 node cannot be powered off by a typo.
func TestNodeTargetMustBeOnDemand(t *testing.T) {
	dir := lab(t)
	write(t, dir, "targets/node-a.yaml", "node-a: { kind: node, node: node-a }\n")
	_, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), "on_demand") {
		t.Fatalf("want an on_demand refusal, got %v", err)
	}
}

func TestRefusesUnknownSignalInStopWhen(t *testing.T) {
	dir := lab(t)
	write(t, dir, "down/all.yaml", `
console: { stop_when: ["!nobody_declared_this"], manages: [console] }
`)
	_, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), "not declared") {
		t.Fatalf("want a refusal naming the signal, got %v", err)
	}
}

// A hold: reference is dynamic and must not need declaring.
func TestHoldSignalsNeedNoDeclaration(t *testing.T) {
	dir := lab(t)
	write(t, dir, "down/all.yaml", `
console: { stop_when: ["!console_in_use", "!hold:console"], manages: [console] }
tower: { stop_when: ["!any_guest_running"], manages: [tower] }
`)
	if _, err := Load(dir); err != nil {
		t.Fatalf("hold: signals are written by a person, not declared: %v", err)
	}
}

func TestRefusesEmptyStopWhen(t *testing.T) {
	dir := lab(t)
	write(t, dir, "down/all.yaml", "console: { stop_when: [], manages: [console] }\n")
	_, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), "stop_when is empty") {
		t.Fatalf("a target with no stop condition would stop the moment it is unwanted: %v", err)
	}
}

func TestRefusesCyclesAndUnknownNeeds(t *testing.T) {
	dir := lab(t)
	write(t, dir, "targets/console.yaml", "console: { kind: guest, node: tower, needs: [nowhere] }\n")
	if _, err := Load(dir); err == nil {
		t.Fatal("a needs pointing nowhere must be refused")
	}
	dir = lab(t)
	write(t, dir, "targets/loop.yaml", `
a: { kind: guest, node: tower, needs: [b] }
b: { kind: guest, node: tower, needs: [a] }
`)
	_, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("want a cycle refusal, got %v", err)
	}
}

func TestDuplicateKeyAcrossFilesIsRefused(t *testing.T) {
	dir := lab(t)
	write(t, dir, "targets/again.yaml", "console: { kind: guest, node: tower }\n")
	_, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), "declared twice") {
		t.Fatalf("a silently overridden target is a trap: %v", err)
	}
}

func TestManagedAndChain(t *testing.T) {
	c, err := Load(lab(t))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Managed("console") || !c.Managed("tower") {
		t.Error("both are named in a manages list")
	}
	chain := c.Chain("console")
	if len(chain) != 2 || chain[0] != "tower" || chain[1] != "console" {
		t.Fatalf("the node must come first: %v", chain)
	}
}

func TestSignalRef(t *testing.T) {
	for _, tc := range []struct {
		in   string
		name string
		neg  bool
	}{{"a", "a", false}, {"!a", "a", true}, {"not:a", "a", true}, {" !a ", "a", true}} {
		n, neg := SignalRef(tc.in)
		if n != tc.name || neg != tc.neg {
			t.Errorf("%q -> (%q,%v), want (%q,%v)", tc.in, n, neg, tc.name, tc.neg)
		}
	}
}

// The shipped example is the documentation: if it stops loading, the docs
// are lying. It also exercises the duration forms people actually type.
func TestExampleConfigLoads(t *testing.T) {
	c, err := Load("../../example")
	if err != nil {
		t.Fatalf("example/ must be valid: %v", err)
	}
	if len(c.Signals) == 0 || len(c.Targets) == 0 || len(c.Downs) == 0 {
		t.Fatal("all three directories should have loaded")
	}
	if c.Targets["pbs"].MinUptime.D() != 25*time.Minute {
		t.Errorf("pbs min_uptime should be 25m, got %s", c.Targets["pbs"].MinUptime.D())
	}
	if !c.Managed("console") || c.Managed("nonexistent") {
		t.Error("Managed should follow the manages lists")
	}
	if got := c.Chain("console"); len(got) != 2 || got[0] != "tower" {
		t.Errorf("chain should raise the node first: %v", got)
	}
}
