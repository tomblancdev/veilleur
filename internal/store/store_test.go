package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestClaimStates(t *testing.T) {
	now := time.Date(2026, 9, 12, 21, 0, 0, 0, time.UTC)
	c := Claim{HeldSince: now, LastActive: now, Deadline: now.Add(time.Hour), Release: ReleaseExplicit}
	if got := c.StateAt(now.Add(30 * time.Minute)); got != Held {
		t.Errorf("inside its deadline it is held, got %s", got)
	}
	if got := c.StateAt(now.Add(2 * time.Hour)); got != Expired {
		t.Errorf("past its deadline it is expired, got %s", got)
	}
	// an idle-ruled claim ends on silence, well inside its deadline
	c.Release, c.IdleAfter = ReleaseIdle, 20*time.Minute
	if got := c.StateAt(now.Add(10 * time.Minute)); got != Held {
		t.Errorf("10 min of silence is not idle yet, got %s", got)
	}
	if got := c.StateAt(now.Add(25 * time.Minute)); got != Expired {
		t.Errorf("25 min of silence past a 20 min window is expired, got %s", got)
	}
	// released beats everything
	rel := now.Add(time.Minute)
	c.ReleasedAt = &rel
	if got := c.StateAt(now.Add(2 * time.Minute)); got != Released {
		t.Errorf("a released claim stays released, got %s", got)
	}
}

func TestTakeReleaseAndReload(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	c, err := s.Take(Claim{Subject: "tom", Target: "console", Release: ReleaseExplicit,
		HeldSince: now, LastActive: now, Deadline: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Held(now)) != 1 {
		t.Fatal("the claim should be held")
	}
	if _, err := s.Release(c.ID, "tom"); err != nil {
		t.Fatal(err)
	}
	if len(s.Held(now)) != 0 {
		t.Fatal("a released claim holds nothing")
	}
	// the store is the truth across a restart
	again, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.All()) != 1 {
		t.Fatalf("the claim should survive a reload, got %d", len(again.All()))
	}
	if again.All()[0].ReleasedAt == nil {
		t.Error("and it should still be released")
	}
}

// An expiry is an event, not an absence: Sweep closes it exactly once.
func TestSweepClosesExpiredClaimsOnce(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err := s.Take(Claim{Subject: "tom", Target: "console", Release: ReleaseDeadline,
		HeldSince: now.Add(-2 * time.Hour), LastActive: now.Add(-2 * time.Hour), Deadline: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if got := len(s.Sweep(now)); got != 1 {
		t.Fatalf("one claim should have just expired, got %d", got)
	}
	if got := len(s.Sweep(now)); got != 0 {
		t.Fatalf("an expiry is noticed once, got %d", got)
	}
}

// The failure the first smoke test of the published image found.
func TestOpenRefusesAnUnwritableDir(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root writes anywhere")
	}
	dir := filepath.Join(t.TempDir(), "locked")
	if err := os.Mkdir(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err == nil {
		t.Fatal("a store that cannot be written must fail at Open, not at the first claim")
	}
}
