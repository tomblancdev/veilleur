package auth

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/tomblancdev/veilleur/internal/config"
)

func testAuth(t *testing.T) *Auth {
	t.Helper()
	dir := t.TempDir()
	tf := filepath.Join(dir, "tokens")
	if err := os.WriteFile(tf, []byte("# machines\narcade:client:s3cret\nops:admin:adm1n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := New(config.Auth{
		UserHeader: "Remote-User", GroupsHeader: "Remote-Groups",
		AdminGroups: []string{"admins"}, TrustedProxies: []string{"192.0.2.30"},
		TokensFile: tf,
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// Identity headers are only believed from the proxy — the whole permission
// model rests on that one line.
func TestHeadersOnlyFromTheTrustedProxy(t *testing.T) {
	a := testAuth(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Remote-User", "mallory")
	req.Header.Set("Remote-Groups", "admins")
	req.RemoteAddr = "192.0.2.99:1234"
	if id := a.Identify(req); id.Role != None {
		t.Fatalf("an untrusted hop must not be able to assert an identity, got %+v", id)
	}
	req.RemoteAddr = "192.0.2.30:1234"
	id := a.Identify(req)
	if !id.IsAdmin() || id.User != "mallory" {
		t.Fatalf("the proxy's identity should be believed, got %+v", id)
	}
}

func TestGroupsDecideAdmin(t *testing.T) {
	a := testAuth(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "192.0.2.30:1234"
	req.Header.Set("Remote-User", "alice")
	req.Header.Set("Remote-Groups", "players,friends")
	if id := a.Identify(req); id.IsAdmin() || id.Role != Client {
		t.Fatalf("a non-admin group should be a client, got %+v", id)
	}
}

func TestBearerTokens(t *testing.T) {
	a := testAuth(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "192.0.2.7:1234" // a machine, not the proxy
	req.Header.Set("Authorization", "Bearer s3cret")
	id := a.Identify(req)
	if id.User != "arcade" || id.Role != Client {
		t.Fatalf("the doorman's token should identify it, got %+v", id)
	}
	req.Header.Set("Authorization", "Bearer nope")
	if id := a.Identify(req); id.Role != None {
		t.Fatalf("an unknown token is nobody, got %+v", id)
	}
}
