// Package config loads Le Veilleur's configuration: one YAML file (the
// target graph, the door, auth) plus a few environment overrides for the
// things a container runtime likes to set. Data in, no logic — the graph is
// configuration, never code.
package config

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Kind of target.
const (
	KindNode  = "node"
	KindGuest = "guest"
)

// Target is one thing that can be on or off. Nodes and guests only in v1;
// a service inside a guest is the same shape plus a unit name (V3).
type Target struct {
	Name  string `yaml:"-" json:"name"`
	Kind  string `yaml:"kind" json:"kind"`   // node | guest
	Label string `yaml:"label" json:"label"` // one line for humans

	// where it lives. A guest names the node that runs it; a node names
	// itself (the PVE node name, which is also the door's host key).
	Node string `yaml:"node" json:"node"`
	VMID int    `yaml:"vmid" json:"vmid,omitempty"`

	// the graph: what must be up before this can be.
	Requires []string `yaml:"requires" json:"requires,omitempty"`

	// a node target must say so out loud. Le Veilleur refuses to manage a
	// node that is not declared on-demand — the 24/7 pair is out of its
	// reach by construction, not by configuration (power.md decision 7).
	OnDemand bool `yaml:"on_demand" json:"on_demand,omitempty"`
	WOL      bool `yaml:"wol" json:"wol,omitempty"`

	UpTimeout time.Duration `yaml:"up_timeout" json:"up_timeout"`
	DownGrace time.Duration `yaml:"down_grace" json:"down_grace"`

	// guards that may veto a STOP of this target (never a start).
	Guards []string `yaml:"guards" json:"guards,omitempty"`

	// default idle window for a claim that releases on idleness.
	IdleAfter time.Duration `yaml:"idle_after" json:"idle_after,omitempty"`

	// the longest a claim on this target may run before it must be renewed.
	MaxHold time.Duration `yaml:"max_hold" json:"max_hold"`

	// false = observe and report, never act. The safety catch for a target
	// you want on the board before you trust the machinery with it.
	Manage *bool `yaml:"manage" json:"manage"`
}

// Managed reports whether Le Veilleur may act on this target.
func (t Target) Managed() bool { return t.Manage == nil || *t.Manage }

// Auth says how a request proves who it is (the La Loge contract).
type Auth struct {
	UserHeader     string   `yaml:"user_header"`
	GroupsHeader   string   `yaml:"groups_header"`
	AdminGroups    []string `yaml:"admin_groups"`
	TrustedProxies []string `yaml:"trusted_proxies"` // identity headers are ignored from anywhere else
	TokensFile     string   `yaml:"tokens_file"`     // lines: name:role:token   (role = admin | client)
	DevUser        string   `yaml:"dev_user"`
	DevRole        string   `yaml:"dev_role"`
}

// DoorHost is one node's ssh door.
type DoorHost struct {
	Node    string `yaml:"node"`
	Addr    string `yaml:"addr"`
	HostKey string `yaml:"host_key"` // pinned; a different machine at the same address is an error
	// control = a 24/7 node: it answers cluster-wide questions and sends
	// the magic packets. An on-demand node's own door is used only for
	// what must be asked or done locally (its ttys, its poweroff).
	Control bool `yaml:"control"`
}

// Door is the one credential: an ssh key restricted to one forced command.
type Door struct {
	Mode    string        `yaml:"mode"` // ssh | mock
	User    string        `yaml:"user"`
	KeyFile string        `yaml:"key_file"`
	Timeout time.Duration `yaml:"timeout"`
	Hosts   []DoorHost    `yaml:"hosts"`
}

// Config is the whole thing.
type Config struct {
	Listen  string `yaml:"listen"`
	BaseURL string `yaml:"base_url"`
	DataDir string `yaml:"data_dir"`
	TZ      string `yaml:"tz"`
	House   string `yaml:"house"`

	// how often the engine compares the world to the claims. Every mutation
	// also kicks a reconcile at once — this is the floor, not the pulse.
	ReconcileInterval time.Duration `yaml:"reconcile_interval"`
	// the cap every claim's deadline is clamped to when it names none.
	DefaultHold time.Duration `yaml:"default_hold"`

	Auth    Auth              `yaml:"auth"`
	DoorCfg Door              `yaml:"door"`
	Targets map[string]Target `yaml:"targets"`
}

// Load reads the file, applies the environment overrides and validates.
func Load(path string) (*Config, error) {
	c := &Config{
		Listen:            ":8080",
		DataDir:           "/data",
		TZ:                "Europe/Paris",
		House:             "Le Squat",
		ReconcileInterval: 30 * time.Second,
		DefaultHold:       8 * time.Hour,
		Auth: Auth{
			UserHeader:   "Remote-User",
			GroupsHeader: "Remote-Groups",
			AdminGroups:  []string{"admins"},
		},
		DoorCfg: Door{Mode: "ssh", User: "veilleur", Timeout: 30 * time.Second},
	}
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("config: %w", err)
		}
		if err := yaml.Unmarshal(raw, c); err != nil {
			return nil, fmt.Errorf("config: %w", err)
		}
	}
	if v := os.Getenv("VEILLEUR_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("VEILLEUR_DATA_DIR"); v != "" {
		c.DataDir = v
	}
	if v := os.Getenv("VEILLEUR_BASE_URL"); v != "" {
		c.BaseURL = v
	}
	for name, t := range c.Targets {
		t.Name = name
		if t.UpTimeout == 0 {
			t.UpTimeout = 3 * time.Minute
		}
		if t.DownGrace == 0 {
			t.DownGrace = time.Minute
		}
		if t.MaxHold == 0 {
			t.MaxHold = c.DefaultHold
		}
		c.Targets[name] = t
	}
	return c, c.Validate()
}

// Validate refuses a graph that cannot be reasoned about. Every failure
// here is a config bug that would otherwise become a power bug.
func (c *Config) Validate() error {
	if len(c.Targets) == 0 {
		return fmt.Errorf("config: no targets declared")
	}
	for _, name := range c.TargetNames() {
		t := c.Targets[name]
		switch t.Kind {
		case KindNode:
			if !t.OnDemand {
				// the hard boundary: a 24/7 node can never be a target
				return fmt.Errorf("target %q: a node target must declare on_demand: true — Le Veilleur never powers a 24/7 node", name)
			}
			if t.Node == "" {
				return fmt.Errorf("target %q: a node target needs `node` (its PVE name)", name)
			}
			if len(t.Requires) > 0 {
				return fmt.Errorf("target %q: a node requires nothing — it is the bottom of the graph", name)
			}
		case KindGuest:
			if t.VMID <= 0 {
				return fmt.Errorf("target %q: a guest target needs `vmid`", name)
			}
			if t.Node == "" {
				return fmt.Errorf("target %q: a guest target needs `node` (where it runs)", name)
			}
		default:
			return fmt.Errorf("target %q: kind must be %q or %q, got %q", name, KindNode, KindGuest, t.Kind)
		}
		for _, r := range t.Requires {
			if _, ok := c.Targets[r]; !ok {
				return fmt.Errorf("target %q requires %q, which is not declared", name, r)
			}
			if r == name {
				return fmt.Errorf("target %q requires itself", name)
			}
		}
		for _, g := range t.Guards {
			if !knownGuard(g) {
				return fmt.Errorf("target %q: unknown guard %q (known: %s)", name, g, strings.Join(KnownGuards, ", "))
			}
		}
	}
	if err := c.checkCycles(); err != nil {
		return err
	}
	if c.DoorCfg.Mode == "ssh" {
		if c.DoorCfg.KeyFile == "" {
			return fmt.Errorf("door: key_file is required in ssh mode")
		}
		control := 0
		for _, h := range c.DoorCfg.Hosts {
			if h.Node == "" || h.Addr == "" || h.HostKey == "" {
				return fmt.Errorf("door host %q: node, addr and host_key are all required (the host key is pinned)", h.Node)
			}
			if h.Control {
				control++
			}
		}
		if control == 0 {
			return fmt.Errorf("door: at least one control host (a 24/7 node) is required — it is what sends the magic packet")
		}
	}
	return nil
}

// KnownGuards are the vetoes a target may name. A guard only ever refuses
// to stop something; none of them can start anything.
var KnownGuards = []string{"human_session", "maintenance_lock", "converge_lock", "cluster_whole", "ha_resident"}

func knownGuard(g string) bool {
	for _, k := range KnownGuards {
		if k == g {
			return true
		}
	}
	return false
}

// TargetNames returns the declared names, sorted — every listing in this
// program is stable, so a diff of two boards means something.
func (c *Config) TargetNames() []string {
	out := make([]string, 0, len(c.Targets))
	for n := range c.Targets {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// checkCycles walks the graph depth-first; a cycle would make "bring the
// parents up first" undefined.
func (c *Config) checkCycles() error {
	const (
		white = 0
		grey  = 1
		black = 2
	)
	colour := map[string]int{}
	var visit func(string, []string) error
	visit = func(n string, path []string) error {
		switch colour[n] {
		case grey:
			return fmt.Errorf("targets: dependency cycle %s", strings.Join(append(path, n), " -> "))
		case black:
			return nil
		}
		colour[n] = grey
		for _, r := range c.Targets[n].Requires {
			if err := visit(r, append(path, n)); err != nil {
				return err
			}
		}
		colour[n] = black
		return nil
	}
	for _, n := range c.TargetNames() {
		if err := visit(n, nil); err != nil {
			return err
		}
	}
	return nil
}

// Order returns the targets in dependency order: everything a target
// requires comes before it. Bring up in this order, stop in reverse.
func (c *Config) Order() []string {
	out := make([]string, 0, len(c.Targets))
	seen := map[string]bool{}
	var visit func(string)
	visit = func(n string) {
		if seen[n] {
			return
		}
		seen[n] = true
		for _, r := range c.Targets[n].Requires {
			visit(r)
		}
		out = append(out, n)
	}
	for _, n := range c.TargetNames() {
		visit(n)
	}
	return out
}

// Dependents lists the targets that require the given one (directly).
func (c *Config) Dependents(name string) []string {
	var out []string
	for _, n := range c.TargetNames() {
		for _, r := range c.Targets[n].Requires {
			if r == name {
				out = append(out, n)
				break
			}
		}
	}
	return out
}
