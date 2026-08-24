package door

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/tomblancdev/veilleur/internal/config"
)

// SSH runs the forced command over one scoped identity. Host keys are
// pinned: a different machine at the same address is an error, never a
// prompt (the La Loge contract).
type SSH struct {
	user    string
	timeout time.Duration
	signer  ssh.Signer

	mu    sync.RWMutex
	hosts map[string]config.DoorHost // node -> its door
	keys  map[string]ssh.PublicKey
}

// NewSSH loads the private key and pins every declared host key.
func NewSSH(cfg config.Door) (*SSH, error) {
	rawKey, err := os.ReadFile(cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("door key: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(rawKey)
	if err != nil {
		return nil, fmt.Errorf("door key: %w", err)
	}
	timeout := cfg.Timeout.D()
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	user := cfg.User
	if user == "" {
		user = "veilleur"
	}
	s := &SSH{
		user:    user,
		timeout: timeout,
		signer:  signer,
		hosts:   map[string]config.DoorHost{},
		keys:    map[string]ssh.PublicKey{},
	}
	for _, h := range cfg.Hosts {
		pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(h.HostKey))
		if err != nil {
			return nil, fmt.Errorf("door host %s: host key: %w", h.Node, err)
		}
		s.hosts[h.Node] = h
		s.keys[h.Node] = pk
	}
	return s, nil
}

// controls lists the 24/7 nodes, which are the ones that answer cluster-wide
// questions and send magic packets.
func (s *SSH) controls() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []string
	for node, h := range s.hosts {
		if h.Control {
			out = append(out, node)
		}
	}
	// sorted, not map order: which node we ask first must be the same on
	// every pass, or a half-broken door makes the whole thing flap.
	sort.Strings(out)
	return out
}

func (s *SSH) run(ctx context.Context, node, args string) ([]byte, error) {
	s.mu.RLock()
	h, ok := s.hosts[node]
	key := s.keys[node]
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("door: no door declared for node %q", node)
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	d := net.Dialer{Timeout: s.timeout}
	conn, err := d.DialContext(ctx, "tcp", h.Addr)
	if err != nil {
		return nil, fmt.Errorf("door %s: dial: %w", node, err)
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	cfg := &ssh.ClientConfig{
		User:              s.user,
		Auth:              []ssh.AuthMethod{ssh.PublicKeys(s.signer)},
		HostKeyCallback:   ssh.FixedHostKey(key),
		HostKeyAlgorithms: []string{key.Type()},
		Timeout:           s.timeout,
	}
	cc, chans, reqs, err := ssh.NewClientConn(conn, h.Addr, cfg)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("door %s: ssh: %w", node, err)
	}
	client := ssh.NewClient(cc, chans, reqs)
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("door %s: session: %w", node, err)
	}
	defer sess.Close()
	var out, errb bytes.Buffer
	sess.Stdout = &out
	sess.Stderr = &errb
	if err := sess.Run(args); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return out.Bytes(), fmt.Errorf("door %s: %s: %s", node, strings.Fields(args)[0], msg)
	}
	return out.Bytes(), nil
}

// anyControl runs a command on the first control node that answers. Two
// 24/7 nodes carry the door precisely so one of them being down is not an
// outage of the watchman (N+1, like the magic packet itself).
func (s *SSH) anyControl(ctx context.Context, args string) ([]byte, error) {
	var lastErr error
	for _, node := range s.controls() {
		out, err := s.run(ctx, node, args)
		if err == nil {
			return out, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("door: no control node declared")
	}
	return nil, lastErr
}

// Observe asks a control node for the cluster-wide picture, then each named
// on-demand node that is up for the facts only it can answer (its ttys).
func (s *SSH) Observe(ctx context.Context, localNodes []string) (Snapshot, error) {
	out, err := s.anyControl(ctx, "status")
	if err != nil {
		return Snapshot{}, err
	}
	r, err := parseStatus(out)
	if err != nil {
		return Snapshot{}, err
	}
	snap := Snapshot{
		At:            time.Now(),
		Source:        r.Node,
		Nodes:         map[string]NodeState{},
		Guests:        map[int]GuestState{},
		Locks:         r.Locks,
		ClusterOnline: r.Cluster.Online,
		ClusterTotal:  r.Cluster.Total,
	}
	for name, n := range r.Nodes {
		// a node's tty count is local knowledge; the control node cannot
		// know it, so it starts unknown and is filled in below.
		n.TTYs = -1
		snap.Nodes[name] = n
	}
	for _, g := range r.Guests {
		snap.Guests[g.VMID] = g
	}
	// the control node knows its own ttys
	if n, ok := snap.Nodes[r.Node]; ok {
		n.TTYs = r.TTYs
		snap.Nodes[r.Node] = n
	}
	for _, node := range localNodes {
		st, ok := snap.Nodes[node]
		if !ok || !st.Online {
			continue // asleep: nobody is logged into it
		}
		if _, declared := s.hosts[node]; !declared {
			continue
		}
		lout, lerr := s.run(ctx, node, "status")
		if lerr != nil {
			// leave TTYs at -1: unknown, which the guard reads as a veto
			continue
		}
		lr, perr := parseStatus(lout)
		if perr != nil {
			continue
		}
		st.TTYs = lr.TTYs
		snap.Nodes[node] = st
	}
	return snap, nil
}

// Wake sends the magic packet from EVERY control node, not the first one
// that answers. A magic packet is a single unacknowledged broadcast frame:
// it can simply be lost, and when it is, the node stays dark and the next
// attempt is a whole reconcile interval away. This lab watched exactly that
// happen — one node's packet did nothing while the other's woke the machine
// in 60 s, on hardware that was armed and healthy either way. The night
// shift's own alarm clock had known this since it was written ("both 24/7
// nodes send it; a second packet is free"); this did not, until it did.
//
// Succeeds if any node got its packet out.
func (s *SSH) Wake(ctx context.Context, node string) error {
	controls := s.controls()
	if len(controls) == 0 {
		return fmt.Errorf("door: no control node declared")
	}
	var sent int
	var lastErr error
	for _, from := range controls {
		if _, err := s.run(ctx, from, "wake "+node); err != nil {
			lastErr = err
			continue
		}
		sent++
	}
	if sent == 0 {
		return fmt.Errorf("door: no control node could wake %s: %w", node, lastErr)
	}
	return nil
}

// StartGuest starts a guest anywhere in the cluster.
func (s *SSH) StartGuest(ctx context.Context, vmid int) error {
	_, err := s.anyControl(ctx, fmt.Sprintf("start %d", vmid))
	return err
}

// StopGuest asks a guest to shut down gracefully.
func (s *SSH) StopGuest(ctx context.Context, vmid int) error {
	_, err := s.anyControl(ctx, fmt.Sprintf("stop %d", vmid))
	return err
}

// PowerOffNode is asked of the node itself: its own copy of the script is
// the thing that decides whether it is allowed to sleep at all. A 24/7
// node's script refuses, so this cannot be aimed at one by mistake.
func (s *SSH) PowerOffNode(ctx context.Context, node string) error {
	_, err := s.run(ctx, node, "poweroff")
	return err
}
