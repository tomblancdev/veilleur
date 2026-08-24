package door

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Mock is an in-memory fleet: a test (and `door.mode: mock` for local
// hacking) can drive the whole engine without a hypervisor anywhere.
type Mock struct {
	mu sync.Mutex

	NodesUp   map[string]bool
	NodeTTYs  map[string]int
	GuestsUp  map[int]bool
	GuestNode map[int]string
	GuestHA   map[int]bool
	Locks     map[string]bool
	Total     int

	// ObserveErr makes Observe fail — the "we cannot see the fleet" case,
	// where the engine must do nothing at all.
	ObserveErr error
	// Actions records every side effect, in order, for assertions.
	Actions []string
	// Now, when set, is the clock the snapshot is stamped with.
	Now func() time.Time
}

// NewMock builds a mock with the given nodes (all up) and no guests.
func NewMock(nodes ...string) *Mock {
	m := &Mock{
		NodesUp:   map[string]bool{},
		NodeTTYs:  map[string]int{},
		GuestsUp:  map[int]bool{},
		GuestNode: map[int]string{},
		GuestHA:   map[int]bool{},
		Locks:     map[string]bool{"maintenance": false, "converge": false},
	}
	for _, n := range nodes {
		m.NodesUp[n] = true
	}
	m.Total = len(nodes)
	return m
}

// AddGuest declares a guest on a node, initially stopped.
func (m *Mock) AddGuest(vmid int, node string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.GuestNode[vmid] = node
	m.GuestsUp[vmid] = false
}

func (m *Mock) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// Observe returns the mock's current world.
func (m *Mock) Observe(_ context.Context, localNodes []string) (Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ObserveErr != nil {
		return Snapshot{}, m.ObserveErr
	}
	snap := Snapshot{
		At:           m.now(),
		Source:       "mock",
		Nodes:        map[string]NodeState{},
		Guests:       map[int]GuestState{},
		Locks:        map[string]bool{},
		ClusterTotal: m.Total,
	}
	for k, v := range m.Locks {
		snap.Locks[k] = v
	}
	local := map[string]bool{}
	for _, n := range localNodes {
		local[n] = true
	}
	for name, up := range m.NodesUp {
		st := NodeState{Online: up, TTYs: -1}
		if up {
			// a node that is up answers its own door; the control nodes
			// always do, and so does any on-demand node we asked about
			if !local[name] || m.NodeTTYs[name] >= 0 {
				st.TTYs = m.NodeTTYs[name]
			}
			snap.ClusterOnline++
		}
		snap.Nodes[name] = st
	}
	for vmid, node := range m.GuestNode {
		status := "stopped"
		if m.GuestsUp[vmid] {
			status = "running"
		}
		snap.Guests[vmid] = GuestState{
			VMID: vmid, Node: node, Type: "qemu",
			Status: status, HA: m.GuestHA[vmid],
			Name: fmt.Sprintf("guest-%d", vmid),
		}
	}
	return snap, nil
}

// Wake turns a node on.
func (m *Mock) Wake(_ context.Context, node string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Actions = append(m.Actions, "wake "+node)
	m.NodesUp[node] = true
	return nil
}

// StartGuest turns a guest on (its node must be up, as in life).
func (m *Mock) StartGuest(_ context.Context, vmid int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Actions = append(m.Actions, fmt.Sprintf("start %d", vmid))
	if node, ok := m.GuestNode[vmid]; ok && !m.NodesUp[node] {
		return fmt.Errorf("mock: node %s is down", node)
	}
	m.GuestsUp[vmid] = true
	return nil
}

// StopGuest turns a guest off.
func (m *Mock) StopGuest(_ context.Context, vmid int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Actions = append(m.Actions, fmt.Sprintf("stop %d", vmid))
	m.GuestsUp[vmid] = false
	return nil
}

// PowerOffNode turns a node off.
func (m *Mock) PowerOffNode(_ context.Context, node string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Actions = append(m.Actions, "poweroff "+node)
	m.NodesUp[node] = false
	return nil
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
func (m *Mock) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Actions = nil
}

// WakeSenders counts how many senders were used for the last wake — the mock
// records one action per sender, so a test can assert redundancy.
func (m *Mock) Count(action string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, a := range m.Actions {
		if a == action {
			n++
		}
	}
	return n
}
