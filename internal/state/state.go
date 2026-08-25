// Package state keeps the one thing a person writes: a hold.
//
// v0.2 kept claims here — leases with deadlines that clients had to remember
// to renew, and whose absence was read as "nobody wants this". That reading
// stopped a backup server a minute after the night shift woke it. v0.3 keeps
// no machine state at all: a wake is a request that is forgotten once the
// chain is up, and what keeps a thing up afterwards is measured, not
// remembered.
//
// A hold is the exception, and it may live for ever — because a person
// decided, and a person can be asked. The price is that it must be loud.
package state

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Hold keeps a target up until a person lifts it.
type Hold struct {
	ID      string    `json:"id"`
	Target  string    `json:"target"`
	By      string    `json:"by"`
	Reason  string    `json:"reason"`
	Since   time.Time `json:"since"`
	HandsOff bool     `json:"hands_off"` // also: do not START it (A.3)
}

// Event is one line of the audit log; the structured logger has it too.
type Event struct {
	At     time.Time `json:"at"`
	Type   string    `json:"type"`
	Actor  string    `json:"actor,omitempty"`
	Target string    `json:"target,omitempty"`
	Detail string    `json:"detail,omitempty"`
}

type file struct {
	Holds []Hold `json:"holds"`
}

// Store is safe for concurrent use.
type Store struct {
	mu     sync.RWMutex
	path   string
	events string
	f      file
}

// ErrNotFound is returned for an unknown hold.
var ErrNotFound = errors.New("no such hold")

// Open loads (or creates) the store under dir.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("state dir: %w", err)
	}
	probe := filepath.Join(dir, ".writable")
	if err := os.WriteFile(probe, []byte("veilleur"), 0o600); err != nil {
		return nil, fmt.Errorf("state: %s is not writable by uid %d: %w", dir, os.Getuid(), err)
	}
	_ = os.Remove(probe)

	s := &Store{path: filepath.Join(dir, "holds.json"), events: filepath.Join(dir, "events.jsonl")}
	raw, err := os.ReadFile(s.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return s, nil
	case err != nil:
		return nil, fmt.Errorf("state: %w", err)
	}
	if err := json.Unmarshal(raw, &s.f); err != nil {
		return nil, fmt.Errorf("state %s: %w", s.path, err)
	}
	return s, nil
}

func (s *Store) save() error {
	raw, err := json.MarshalIndent(s.f, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Log appends one event; a failed write never fails the action it describes.
func (s *Store) Log(e Event) {
	if e.At.IsZero() {
		e.At = time.Now()
	}
	f, err := os.OpenFile(s.events, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	if raw, err := json.Marshal(e); err == nil {
		_, _ = f.Write(append(raw, '\n'))
	}
}

// Take records a hold. A second hold on the same target is not an error —
// two people may both want it, and it stays up until both let go.
func (s *Store) Take(h Hold) (Hold, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h.ID = newID()
	if h.Since.IsZero() {
		h.Since = time.Now()
	}
	s.f.Holds = append(s.f.Holds, h)
	if err := s.save(); err != nil {
		return Hold{}, err
	}
	s.Log(Event{Type: "hold.taken", Actor: h.By, Target: h.Target, Detail: h.Reason})
	return h, nil
}

// Release lifts a hold.
func (s *Store) Release(id, by string) (Hold, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, h := range s.f.Holds {
		if h.ID != id {
			continue
		}
		s.f.Holds = append(s.f.Holds[:i], s.f.Holds[i+1:]...)
		if err := s.save(); err != nil {
			return Hold{}, err
		}
		s.Log(Event{Type: "hold.released", Actor: by, Target: h.Target,
			Detail: fmt.Sprintf("held %s by %s", time.Since(h.Since).Truncate(time.Minute), h.By)})
		return h, nil
	}
	return Hold{}, ErrNotFound
}

// All holds, oldest first — the board shows the stalest at the top.
func (s *Store) All() []Hold {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := append([]Hold(nil), s.f.Holds...)
	sort.Slice(out, func(i, j int) bool { return out[i].Since.Before(out[j].Since) })
	return out
}

// On reports the holds on a target.
func (s *Store) On(target string) []Hold {
	var out []Hold
	for _, h := range s.All() {
		if h.Target == target {
			out = append(out, h)
		}
	}
	return out
}

// HandsOff reports whether a hold forbids touching this target at all.
func (s *Store) HandsOff(target string) bool {
	for _, h := range s.On(target) {
		if h.HandsOff {
			return true
		}
	}
	return false
}

func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
