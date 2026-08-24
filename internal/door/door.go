// Package door is how Le Veilleur touches machines: one ssh key restricted
// to one forced command on every hypervisor (`squat-veilleur`). The key can
// run nothing else, and each node's own copy of the script carries the
// allowlist of what it will accept — so a compromised watchman cannot stop
// a guest that is not in the graph, and cannot power off a 24/7 node at all
// (power.md §6).
package door

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// NodeState is what a node reports about itself and its cluster.
type NodeState struct {
	Online bool `json:"online"`
	// TTYs is the number of interactive sessions (`who` with a pts/tty).
	// -1 = not known (we could not ask that node) — treated as a veto.
	TTYs int `json:"ttys"`
}

// GuestState is one guest as the cluster sees it.
type GuestState struct {
	VMID   int    `json:"vmid"`
	Name   string `json:"name"`
	Node   string `json:"node"`
	Type   string `json:"type"`   // qemu | lxc
	Status string `json:"status"` // running | stopped
	HA     bool   `json:"ha"`     // managed by the HA stack — never ours to stop
}

// Snapshot is one observation of the fleet.
type Snapshot struct {
	At            time.Time            `json:"at"`
	Source        string               `json:"source"` // the node that answered
	Nodes         map[string]NodeState `json:"nodes"`
	Guests        map[int]GuestState   `json:"guests"`
	Locks         map[string]bool      `json:"locks"`
	ClusterOnline int                  `json:"cluster_online"`
	ClusterTotal  int                  `json:"cluster_total"`
}

// Running reports whether a guest is up. Unknown guests read as not running.
func (s Snapshot) Running(vmid int) bool {
	g, ok := s.Guests[vmid]
	return ok && g.Status == "running"
}

// NodeUp reports whether a node is online in the cluster.
func (s Snapshot) NodeUp(name string) bool {
	n, ok := s.Nodes[name]
	return ok && n.Online
}

// Fleet is what the engine is allowed to do to the world. Every method is
// idempotent: asking for something that is already true is not an error.
type Fleet interface {
	// Observe reads the whole fleet. It asks a control node for the
	// cluster-wide picture, and each on-demand node that is up for its own
	// local facts (its ttys) — that is the only place they can come from.
	Observe(ctx context.Context, localNodes []string) (Snapshot, error)
	Wake(ctx context.Context, node string) error
	StartGuest(ctx context.Context, vmid int) error
	StopGuest(ctx context.Context, vmid int) error
	PowerOffNode(ctx context.Context, node string) error
}

// raw is the JSON the forced command prints for `status`.
type raw struct {
	Node    string               `json:"node"`
	Cluster struct {
		Online int `json:"online"`
		Total  int `json:"total"`
	} `json:"cluster"`
	Locks  map[string]bool      `json:"locks"`
	TTYs   int                  `json:"ttys"`
	Nodes  map[string]NodeState `json:"nodes"`
	Guests []GuestState         `json:"guests"`
}

func parseStatus(out []byte) (raw, error) {
	var r raw
	if err := json.Unmarshal(out, &r); err != nil {
		return raw{}, fmt.Errorf("door status: %w", err)
	}
	return r, nil
}
