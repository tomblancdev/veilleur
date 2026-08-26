// Package config loads Le Veilleur's three directories.
//
//	signals/  named questions: which node answers, what a zero exit means
//	targets/  what exists, how to SEE it, how to RAISE it, what it needs first
//	down/     when a thing may stop, what may follow, and what we may touch
//
// Data, not logic (guidelines §1.7). Nothing here knows how to run a command:
// a signal or a target names one, and the NODE holds the command itself in
// its own allowlist. Le Veilleur may ask for `console_in_use`; it cannot ask
// for `rm -rf /`.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Kind of target.
const (
	KindNode  = "node"
	KindGuest = "guest"
)

// AnyControl is the pseudo-node meaning "any 24/7 node that answers".
const AnyControl = "any_control"

// Signal is a named question. The answer is an exit code from a command the
// node holds: 0 = true, anything else = false, unreachable = UNKNOWN.
type Signal struct {
	Name  string   `yaml:"-" json:"name"`
	RunOn string   `yaml:"run_on" json:"run_on"`
	Means string   `yaml:"means" json:"means"`
	TTL   Duration `yaml:"ttl" json:"ttl"`
}

// Target is a thing that can be on or off.
type Target struct {
	Name  string `yaml:"-" json:"name"`
	Kind  string `yaml:"kind" json:"kind"`
	Label string `yaml:"label" json:"label"`
	Node  string `yaml:"node" json:"node"`

	// Needs is the WAKE chain and only the wake chain: what must be up before
	// this can be raised. The stop chain lives in down/ and is deliberately
	// not its inverse (§A.5).
	Needs []string `yaml:"needs" json:"needs,omitempty"`

	// A node target must say so out loud, or Le Veilleur refuses to load: the
	// 24/7 pair is out of reach by construction, not by configuration.
	OnDemand bool `yaml:"on_demand" json:"on_demand,omitempty"`

	UpTimeout Duration `yaml:"up_timeout" json:"up_timeout"`

	// MinUptime: not eligible to stop until it has been up this long AND
	// every signal in its stop_when has been evaluable at least once. This is
	// what claims used to cover by accident, and the gap that lost a backup.
	MinUptime Duration `yaml:"min_uptime" json:"min_uptime"`

	// NOTE: there is deliberately no wake_backstop here. An earlier draft had
	// Le Veilleur arm the machine's RTC alarm before putting it to sleep —
	// which makes the guarantee that backups happen depend on the very
	// component you must assume can be compromised. The alarm stays owned by
	// the machine itself (rtc-alarm, armed at its own shutdown), on
	// hardware Le Veilleur cannot reach. It is the one boundary that does not
	// trust the watchman at all.
}

// Down says when a target may stop and what that may free.
type Down struct {
	Name string `yaml:"-" json:"name"`
	// StopWhen: every condition must hold. "signal" or "!signal".
	StopWhen []string `yaml:"stop_when" json:"stop_when"`
	Grace    Duration `yaml:"grace" json:"grace"`
	// ThenConsider: stopping this may have freed these; re-examine them.
	ThenConsider []string `yaml:"then_consider" json:"then_consider,omitempty"`
	// Manages: the targets this entry is allowed to stop. A guest started by
	// hand is in nobody's Manages and is therefore never touched.
	Manages []string `yaml:"manages" json:"manages,omitempty"`
}

// Auth is the La Loge contract: the proxy's headers, and bearer tokens.
type Auth struct {
	UserHeader     string   `yaml:"user_header"`
	GroupsHeader   string   `yaml:"groups_header"`
	AdminGroups    []string `yaml:"admin_groups"`
	TrustedProxies []string `yaml:"trusted_proxies"`
	TokensFile     string   `yaml:"tokens_file"`
	DevUser        string   `yaml:"dev_user"`
	DevRole        string   `yaml:"dev_role"`
}

// DoorHost is one node's ssh door.
type DoorHost struct {
	Node    string `yaml:"node"`
	Addr    string `yaml:"addr"`
	HostKey string `yaml:"host_key"`
	Control bool   `yaml:"control"`
}

// Door is the one credential.
type Door struct {
	Mode    string     `yaml:"mode"`
	User    string     `yaml:"user"`
	KeyFile string     `yaml:"key_file"`
	Timeout Duration   `yaml:"timeout"`
	Hosts   []DoorHost `yaml:"hosts"`
}

// Config is everything.
type Config struct {
	Listen  string `yaml:"listen"`
	BaseURL string `yaml:"base_url"`
	DataDir string `yaml:"data_dir"`
	TZ      string `yaml:"tz"`
	House   string `yaml:"house"`

	Interval Duration `yaml:"interval"`
	Auth     Auth     `yaml:"auth"`
	DoorCfg  Door     `yaml:"door"`

	Signals map[string]Signal `yaml:"-"`
	Targets map[string]Target `yaml:"-"`
	Downs   map[string]Down   `yaml:"-"`
}

// Load reads main.yaml plus the three directories beneath dir.
func Load(dir string) (*Config, error) {
	c := &Config{
		Listen: ":8080", DataDir: "/data", TZ: "Europe/Paris", House: "",
		Interval: Duration(30e9),
		Auth:     Auth{UserHeader: "Remote-User", GroupsHeader: "Remote-Groups", AdminGroups: []string{"admins"}},
		DoorCfg:  Door{Mode: "ssh", User: "root", Timeout: Duration(60e9)},
		Signals:  map[string]Signal{}, Targets: map[string]Target{}, Downs: map[string]Down{},
	}
	if dir == "" {
		return c, fmt.Errorf("config: no directory given")
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "main.yaml")); err == nil {
		if err := yaml.Unmarshal(raw, c); err != nil {
			return nil, fmt.Errorf("config main.yaml: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("config main.yaml: %w", err)
	}
	if err := loadDir(filepath.Join(dir, "signals"), &c.Signals); err != nil {
		return nil, err
	}
	if err := loadDir(filepath.Join(dir, "targets"), &c.Targets); err != nil {
		return nil, err
	}
	if err := loadDir(filepath.Join(dir, "down"), &c.Downs); err != nil {
		return nil, err
	}
	for n, v := range c.Signals {
		v.Name = n
		if v.TTL == 0 {
			v.TTL = Duration(90e9)
		}
		c.Signals[n] = v
	}
	for n, v := range c.Targets {
		v.Name = n
		if v.UpTimeout == 0 {
			v.UpTimeout = Duration(180e9)
		}
		if v.MinUptime == 0 {
			v.MinUptime = Duration(300e9)
		}
		c.Targets[n] = v
	}
	for n, v := range c.Downs {
		v.Name = n
		if v.Grace == 0 {
			v.Grace = Duration(120e9)
		}
		c.Downs[n] = v
	}
	return c, c.Validate()
}

// loadDir merges every *.yaml in dir into out (a map[string]T).
func loadDir[T any](dir string, out *map[string]T) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("config %s: %w", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && (strings.HasSuffix(e.Name(), ".yaml") || strings.HasSuffix(e.Name(), ".yml")) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, n := range names {
		raw, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			return fmt.Errorf("config %s/%s: %w", dir, n, err)
		}
		var chunk map[string]T
		if err := yaml.Unmarshal(raw, &chunk); err != nil {
			return fmt.Errorf("config %s/%s: %w", dir, n, err)
		}
		for k, v := range chunk {
			if _, dup := (*out)[k]; dup {
				return fmt.Errorf("config %s/%s: %q is declared twice", dir, n, k)
			}
			(*out)[k] = v
		}
	}
	return nil
}

// SignalRef splits "!name" into (name, negated).
func SignalRef(ref string) (string, bool) {
	ref = strings.TrimSpace(ref)
	if strings.HasPrefix(ref, "!") {
		return strings.TrimSpace(ref[1:]), true
	}
	if strings.HasPrefix(ref, "not:") {
		return strings.TrimSpace(ref[4:]), true
	}
	return ref, false
}

// HoldSignal is the reserved prefix for the one thing a person writes.
const HoldPrefix = "hold:"

// Validate refuses a configuration that cannot be reasoned about.
func (c *Config) Validate() error {
	if len(c.Targets) == 0 {
		return fmt.Errorf("config: no targets declared")
	}
	for _, n := range sortedKeys(c.Targets) {
		t := c.Targets[n]
		switch t.Kind {
		case KindNode:
			if !t.OnDemand {
				return fmt.Errorf("target %q: a node target must declare on_demand: true — Le Veilleur never powers a 24/7 node", n)
			}
			if len(t.Needs) > 0 {
				return fmt.Errorf("target %q: a node needs nothing — it is the bottom of the graph", n)
			}
		case KindGuest:
			if t.Node == "" {
				return fmt.Errorf("target %q: a guest needs `node` (which machine runs it)", n)
			}
		default:
			return fmt.Errorf("target %q: kind must be %q or %q, got %q", n, KindNode, KindGuest, t.Kind)
		}
		for _, need := range t.Needs {
			if _, ok := c.Targets[need]; !ok {
				return fmt.Errorf("target %q needs %q, which is not declared", n, need)
			}
			if need == n {
				return fmt.Errorf("target %q needs itself", n)
			}
		}
	}
	if err := c.checkCycles(); err != nil {
		return err
	}
	for _, n := range sortedKeys(c.Downs) {
		d := c.Downs[n]
		if _, ok := c.Targets[n]; !ok {
			return fmt.Errorf("down %q: no such target", n)
		}
		if len(d.StopWhen) == 0 {
			return fmt.Errorf("down %q: stop_when is empty — a target with no condition would stop the moment it is unwanted", n)
		}
		for _, ref := range d.StopWhen {
			name, _ := SignalRef(ref)
			if strings.HasPrefix(name, HoldPrefix) {
				continue // dynamic: a person writes it
			}
			if _, ok := c.Signals[name]; !ok {
				return fmt.Errorf("down %q: stop_when names signal %q, which is not declared", n, name)
			}
		}
		for _, m := range d.Manages {
			if _, ok := c.Targets[m]; !ok {
				return fmt.Errorf("down %q: manages %q, which is not a target", n, m)
			}
		}
		for _, t := range d.ThenConsider {
			if _, ok := c.Targets[t]; !ok {
				return fmt.Errorf("down %q: then_consider names %q, which is not a target", n, t)
			}
		}
	}
	if c.DoorCfg.Mode == "ssh" {
		if c.DoorCfg.KeyFile == "" {
			return fmt.Errorf("door: key_file is required in ssh mode")
		}
		control := 0
		for _, h := range c.DoorCfg.Hosts {
			if h.Node == "" || h.Addr == "" || h.HostKey == "" {
				return fmt.Errorf("door host %q: node, addr and host_key are all required (host keys are pinned)", h.Node)
			}
			if h.Control {
				control++
			}
		}
		if control == 0 {
			return fmt.Errorf("door: at least one control host (a 24/7 node) is required")
		}
	}
	return nil
}

// Managed reports whether any down entry is allowed to stop this target.
func (c *Config) Managed(name string) bool {
	for _, d := range c.Downs {
		for _, m := range d.Manages {
			if m == name {
				return true
			}
		}
	}
	return false
}

// Chain returns the target and everything it needs, in the order they must
// come up.
func (c *Config) Chain(name string) []string {
	var out []string
	seen := map[string]bool{}
	var walk func(string)
	walk = func(n string) {
		if seen[n] {
			return
		}
		seen[n] = true
		for _, need := range c.Targets[n].Needs {
			walk(need)
		}
		out = append(out, n)
	}
	if _, ok := c.Targets[name]; !ok {
		return nil
	}
	walk(name)
	return out
}

// TargetNames, sorted — every listing in this program is stable.
func (c *Config) TargetNames() []string { return sortedKeys(c.Targets) }

// SignalNames, sorted.
func (c *Config) SignalNames() []string { return sortedKeys(c.Signals) }

func sortedKeys[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (c *Config) checkCycles() error {
	const (
		white = iota
		grey
		black
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
		for _, need := range c.Targets[n].Needs {
			if err := visit(need, append(path, n)); err != nil {
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
