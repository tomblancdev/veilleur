// Package auth turns a request into an identity and a role. Le Veilleur
// never authenticates anyone itself: a reverse proxy doing forward-auth
// (Caddy + Authelia) sets identity headers, and only a trusted proxy may do
// so. Machines — the arcade doorman, a converge — use bearer tokens.
package auth

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/tomblancdev/veilleur/internal/config"
)

// Role is what an identity may do.
type Role string

const (
	None   Role = ""
	Client Role = "client" // take and release its OWN claims
	Admin  Role = "admin"  // everything, including someone else's claim
)

// Identity is the caller.
type Identity struct {
	User   string
	Groups []string
	Role   Role
	Via    string // header | token | dev
}

// IsAdmin is the only privilege question this program asks.
func (i Identity) IsAdmin() bool { return i.Role == Admin }

// Auth resolves identities.
type Auth struct {
	cfg     config.Auth
	proxies []*net.IPNet
	tokens  map[string]token
}

type token struct {
	name string
	role Role
}

// New parses the trusted proxies and the tokens file.
func New(cfg config.Auth) (*Auth, error) {
	a := &Auth{cfg: cfg, tokens: map[string]token{}}
	for _, c := range cfg.TrustedProxies {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if !strings.Contains(c, "/") {
			if strings.Contains(c, ":") {
				c += "/128"
			} else {
				c += "/32"
			}
		}
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			return nil, fmt.Errorf("trusted_proxies %q: %w", c, err)
		}
		a.proxies = append(a.proxies, n)
	}
	if cfg.TokensFile != "" {
		if err := a.loadTokens(cfg.TokensFile); err != nil {
			return nil, err
		}
	}
	return a, nil
}

// loadTokens reads `name:role:token` lines. The file is a secret: it is
// mounted, never baked, and never logged.
func (a *Auth) loadTokens(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // no machine clients yet
		}
		return fmt.Errorf("tokens_file: %w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			return fmt.Errorf("tokens_file line %d: want name:role:token", n)
		}
		role := Role(strings.TrimSpace(parts[1]))
		if role != Admin && role != Client {
			return fmt.Errorf("tokens_file line %d: role must be admin or client", n)
		}
		a.tokens[strings.TrimSpace(parts[2])] = token{name: strings.TrimSpace(parts[0]), role: role}
	}
	return sc.Err()
}

// trusted reports whether the request arrived from a proxy allowed to
// assert an identity.
func (a *Auth) trusted(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, n := range a.proxies {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// Identify resolves the caller. A bearer token wins over headers: a machine
// says who it is with something it holds, not with something a hop set.
func (a *Auth) Identify(r *http.Request) Identity {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		if t, ok := a.tokens[strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))]; ok {
			return Identity{User: t.name, Role: t.role, Via: "token"}
		}
		return Identity{}
	}
	if a.trusted(r) {
		user := r.Header.Get(a.cfg.UserHeader)
		if user != "" {
			groups := splitGroups(r.Header.Get(a.cfg.GroupsHeader))
			id := Identity{User: user, Groups: groups, Role: Client, Via: "header"}
			for _, g := range groups {
				for _, ag := range a.cfg.AdminGroups {
					if strings.EqualFold(g, ag) {
						id.Role = Admin
					}
				}
			}
			return id
		}
	}
	if a.cfg.DevUser != "" {
		role := Role(a.cfg.DevRole)
		if role == "" {
			role = Admin
		}
		return Identity{User: a.cfg.DevUser, Role: role, Via: "dev"}
	}
	return Identity{}
}

func splitGroups(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
