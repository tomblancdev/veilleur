// Package store keeps the watchman's state: claims in one JSON file written
// atomically, and an append-only event log. Tens of records, not thousands —
// a file is the honest database here, and a human can read the backup
// (La Loge's store, same reasoning).
//
// The store is deliberately REBUILDABLE: an empty file means "no claims",
// and no claims never turns anything on. Losing it costs the current
// holds, never the fleet's safety (power.md §7).
package store

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

// Release says how a claim ends.
const (
	ReleaseExplicit = "explicit" // the client says when
	ReleaseIdle     = "idle"     // the target reports it is unused
	ReleaseDeadline = "deadline" // the clock alone
)

// State of a claim at a point in time.
type State string

const (
	Held     State = "held"
	Released State = "released"
	Expired  State = "expired"
)

// Claim is the only verb: subject needs target up, because reason, until
// condition. Every claim carries a Deadline, whatever its Release rule —
// nothing may pin a machine on forever (power.md decision 8).
type Claim struct {
	ID      string `json:"id"`
	Seq     int    `json:"seq"` // human number: #42
	Subject string `json:"subject"`
	Via     string `json:"via"` // header | token:<name> | dev
	Target  string `json:"target"`
	Reason  string `json:"reason"`

	HeldSince time.Time `json:"held_since"`
	Deadline  time.Time `json:"deadline"`

	Release   string        `json:"release"`
	IdleAfter time.Duration `json:"idle_after,omitempty"`
	LastActive time.Time    `json:"last_active"`

	ReleasedAt *time.Time `json:"released_at,omitempty"`
	ReleasedBy string     `json:"released_by,omitempty"`
}

// StateAt tells whether the claim still holds its target up at t.
func (c Claim) StateAt(t time.Time) State {
	switch {
	case c.ReleasedAt != nil:
		return Released
	case !t.Before(c.Deadline):
		return Expired
	case c.Release == ReleaseIdle && c.IdleAfter > 0 && t.Sub(c.LastActive) >= c.IdleAfter:
		return Expired
	default:
		return Held
	}
}

// Event is one line of the audit log (also emitted as a structured log line
// and therefore as a Loki stream — the trail lives there, not here).
type Event struct {
	At      time.Time `json:"at"`
	Type    string    `json:"type"`
	Actor   string    `json:"actor,omitempty"`
	Target  string    `json:"target,omitempty"`
	ClaimID string    `json:"claim_id,omitempty"`
	Detail  string    `json:"detail,omitempty"`
}

type state struct {
	Seq    int     `json:"seq"`
	Claims []Claim `json:"claims"`
}

// Store is safe for concurrent use.
type Store struct {
	mu     sync.Mutex
	path   string
	events string
	st     state
	// how long a finished claim stays in the file before it is pruned;
	// the event log and Loki keep the history.
	keep time.Duration
}

// ErrNotFound is returned for an unknown claim id.
var ErrNotFound = errors.New("no such claim")

// Open loads (or creates) the store under dir.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("store dir: %w", err)
	}
	s := &Store{
		path:   filepath.Join(dir, "claims.json"),
		events: filepath.Join(dir, "events.jsonl"),
		keep:   7 * 24 * time.Hour,
	}
	// Prove the directory is writable NOW, not at the first claim. A store
	// that cannot be written is a watchman that will accept a claim and then
	// lose it — and /healthz would have said "ok" the whole time. Found by
	// the first smoke test of the published image (a data dir owned by the
	// wrong uid), which is exactly how it would have failed in the lab.
	probe := filepath.Join(dir, ".writable")
	if err := os.WriteFile(probe, []byte("veilleur"), 0o600); err != nil {
		return nil, fmt.Errorf("store: %s is not writable by this user (uid %d): %w", dir, os.Getuid(), err)
	}
	_ = os.Remove(probe)

	raw, err := os.ReadFile(s.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return s, nil
	case err != nil:
		return nil, fmt.Errorf("store: %w", err)
	}
	if err := json.Unmarshal(raw, &s.st); err != nil {
		return nil, fmt.Errorf("store %s: %w", s.path, err)
	}
	return s, nil
}

// save writes the file atomically (temp + rename): a torn claims.json after
// a power cut would be the one file we cannot afford to lose mid-write.
func (s *Store) save() error {
	raw, err := json.MarshalIndent(s.st, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Log appends one event. A failure to write the log never fails the action
// it describes — the structured logger has it too.
func (s *Store) Log(e Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logLocked(e)
}

func (s *Store) logLocked(e Event) {
	if e.At.IsZero() {
		e.At = time.Now()
	}
	f, err := os.OpenFile(s.events, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	raw, err := json.Marshal(e)
	if err != nil {
		return
	}
	_, _ = f.Write(append(raw, '\n'))
}

// Take records a new claim.
func (s *Store) Take(c Claim) (Claim, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.Seq++
	c.Seq = s.st.Seq
	c.ID = newID()
	if c.HeldSince.IsZero() {
		c.HeldSince = time.Now()
	}
	if c.LastActive.IsZero() {
		c.LastActive = c.HeldSince
	}
	s.st.Claims = append(s.st.Claims, c)
	s.pruneLocked()
	if err := s.save(); err != nil {
		return Claim{}, err
	}
	s.logLocked(Event{Type: "claim.taken", Actor: c.Subject, Target: c.Target, ClaimID: c.ID, Detail: c.Reason})
	return c, nil
}

// Release ends a claim now.
func (s *Store) Release(id, by string) (Claim, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.st.Claims {
		if s.st.Claims[i].ID != id {
			continue
		}
		if s.st.Claims[i].ReleasedAt == nil {
			now := time.Now()
			s.st.Claims[i].ReleasedAt = &now
			s.st.Claims[i].ReleasedBy = by
			if err := s.save(); err != nil {
				return Claim{}, err
			}
			s.logLocked(Event{Type: "claim.released", Actor: by, Target: s.st.Claims[i].Target, ClaimID: id})
		}
		return s.st.Claims[i], nil
	}
	return Claim{}, ErrNotFound
}

// Heartbeat refreshes an idle-ruled claim: "still in use".
func (s *Store) Heartbeat(id, by string, at time.Time, extendDeadline time.Duration) (Claim, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.st.Claims {
		if s.st.Claims[i].ID != id {
			continue
		}
		if s.st.Claims[i].ReleasedAt != nil {
			return s.st.Claims[i], nil
		}
		if at.IsZero() {
			at = time.Now()
		}
		s.st.Claims[i].LastActive = at
		if extendDeadline > 0 {
			if d := at.Add(extendDeadline); d.After(s.st.Claims[i].Deadline) {
				s.st.Claims[i].Deadline = d
			}
		}
		if err := s.save(); err != nil {
			return Claim{}, err
		}
		return s.st.Claims[i], nil
	}
	return Claim{}, ErrNotFound
}

// Get returns one claim.
func (s *Store) Get(id string) (Claim, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.st.Claims {
		if c.ID == id {
			return c, nil
		}
	}
	return Claim{}, ErrNotFound
}

// All returns every claim still in the file, newest first.
func (s *Store) All() []Claim {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]Claim(nil), s.st.Claims...)
	sort.Slice(out, func(i, j int) bool { return out[i].Seq > out[j].Seq })
	return out
}

// Held returns the claims holding their target up at t.
func (s *Store) Held(t time.Time) []Claim {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Claim
	for _, c := range s.st.Claims {
		if c.StateAt(t) == Held {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

// Sweep marks newly-expired claims in the log exactly once, so an expiry is
// an event and not just an absence. Returns those it just noticed.
func (s *Store) Sweep(now time.Time) []Claim {
	s.mu.Lock()
	defer s.mu.Unlock()
	var justExpired []Claim
	for i := range s.st.Claims {
		c := &s.st.Claims[i]
		if c.ReleasedAt != nil || c.StateAt(now) != Expired {
			continue
		}
		// record the expiry by closing it: released_by names the rule
		at := c.Deadline
		by := "deadline"
		if c.Release == ReleaseIdle && c.IdleAfter > 0 {
			if idleAt := c.LastActive.Add(c.IdleAfter); idleAt.Before(at) {
				at, by = idleAt, "idle"
			}
		}
		c.ReleasedAt = &at
		c.ReleasedBy = by
		justExpired = append(justExpired, *c)
		s.logLocked(Event{At: now, Type: "claim.expired", Actor: by, Target: c.Target, ClaimID: c.ID})
	}
	if len(justExpired) > 0 {
		s.pruneLocked()
		_ = s.save()
	}
	return justExpired
}

// pruneLocked drops finished claims older than keep.
func (s *Store) pruneLocked() {
	cut := time.Now().Add(-s.keep)
	kept := s.st.Claims[:0]
	for _, c := range s.st.Claims {
		if c.ReleasedAt != nil && c.ReleasedAt.Before(cut) {
			continue
		}
		kept = append(kept, c)
	}
	s.st.Claims = kept
}

func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
