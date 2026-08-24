// Package web is the watchman's HTTP surface: the board (a page), the JSON
// API machines use, and the house contract — /healthz, /metrics, /openapi.json.
package web

import (
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/tomblancdev/veilleur/internal/auth"
	"github.com/tomblancdev/veilleur/internal/config"
	"github.com/tomblancdev/veilleur/internal/fleet"
	"github.com/tomblancdev/veilleur/internal/store"
	"github.com/tomblancdev/veilleur/ui"
)

// Server holds what every handler needs.
type Server struct {
	cfg     *config.Config
	store   *store.Store
	engine  *fleet.Engine
	auth    *auth.Auth
	log     *slog.Logger
	version string
	started time.Time
	page    *template.Template
}

// New builds the surface.
func New(cfg *config.Config, st *store.Store, e *fleet.Engine, a *auth.Auth, version string, log *slog.Logger) *Server {
	return &Server{
		cfg: cfg, store: st, engine: e, auth: a,
		version: version, started: time.Now(), log: log,
		page: template.Must(template.New("board").Funcs(funcs).Parse(boardHTML)),
	}
}

func (s *Server) identify(r *http.Request) auth.Identity { return s.auth.Identify(r) }

// Handler routes everything.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /metrics", s.metrics)
	mux.HandleFunc("GET /openapi.json", s.openapi)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(ui.Static)))

	mux.HandleFunc("GET /api/targets", s.apiTargets)
	mux.HandleFunc("GET /api/targets/{name}", s.apiTarget)
	mux.HandleFunc("POST /api/targets/{name}/ensure", s.apiEnsure)
	mux.HandleFunc("GET /api/claims", s.apiClaims)
	mux.HandleFunc("POST /api/claims", s.apiTakeClaim)
	mux.HandleFunc("DELETE /api/claims/{id}", s.apiReleaseClaim)
	mux.HandleFunc("POST /api/claims/{id}/heartbeat", s.apiHeartbeat)
	mux.HandleFunc("GET /api/fleet", s.apiFleet)

	mux.HandleFunc("POST /ui/hold", s.uiHold)
	mux.HandleFunc("POST /ui/release", s.uiRelease)
	mux.HandleFunc("GET /{$}", s.board)
	return mux
}

// healthz is the watcher's door. It answers 200 while the program runs and
// can see the fleet; a blind watchman is degraded, and says so.
func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	ok, why := s.engine.Healthy()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if !ok {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintf(w, "degraded: cannot observe the fleet: %s\n", why)
		return
	}
	fmt.Fprintln(w, "ok")
}

func (s *Server) metrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	fmt.Fprintf(w, "# HELP veilleur_build_info Build information of the watchman.\n# TYPE veilleur_build_info gauge\nveilleur_build_info{version=%q} 1\n", s.version)
	fmt.Fprintf(w, "# HELP veilleur_uptime_seconds Seconds since the watchman started.\n# TYPE veilleur_uptime_seconds gauge\nveilleur_uptime_seconds %d\n", int64(time.Since(s.started).Seconds()))
	fmt.Fprint(w, s.engine.Metrics())
}

// --- the page -------------------------------------------------------------

type boardData struct {
	House    string
	Version  string
	Identity auth.Identity
	Board    fleet.Board
	Claims   []claimRow
	Targets  []string
	Now      time.Time
}

type claimRow struct {
	store.Claim
	State store.State
	Left  time.Duration
}

func (s *Server) board(w http.ResponseWriter, r *http.Request) {
	id := s.identify(r)
	if !id.IsAdmin() {
		// v1: the board is an operations plane, so it is an admin plane
		// (power.md §8). A house-facing view is open point 3.
		http.Error(w, "the watchman's board is for admins", http.StatusForbidden)
		return
	}
	now := time.Now()
	var rows []claimRow
	for _, c := range s.store.All() {
		st := c.StateAt(now)
		row := claimRow{Claim: c, State: st}
		if st == store.Held {
			row.Left = time.Until(c.Deadline).Truncate(time.Second)
		}
		rows = append(rows, row)
	}
	data := boardData{
		House: s.cfg.House, Version: s.version, Identity: id,
		Board: s.engine.Board(), Claims: rows, Targets: s.cfg.TargetNames(), Now: now,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.page.Execute(w, data); err != nil {
		s.log.Error("render", "err", err)
	}
}

func (s *Server) uiHold(w http.ResponseWriter, r *http.Request) {
	id := s.identify(r)
	if !id.IsAdmin() {
		http.Error(w, "admins only", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	hold := r.FormValue("hold")
	if hold == "" {
		hold = "2h"
	}
	c, err := s.newClaim(id, claimRequest{
		Target:  r.FormValue("target"),
		Reason:  strings.TrimSpace(r.FormValue("reason")),
		Release: store.ReleaseExplicit,
		Hold:    hold,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	saved, err := s.store.Take(c)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.log.Info("claim taken", "claim", saved.ID, "target", saved.Target, "subject", saved.Subject, "via", "page", "reason", saved.Reason)
	s.engine.Kick()
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) uiRelease(w http.ResponseWriter, r *http.Request) {
	id := s.identify(r)
	if !id.IsAdmin() {
		http.Error(w, "admins only", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if _, err := s.store.Release(r.FormValue("id"), id.User); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	s.engine.Kick()
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

var funcs = template.FuncMap{
	"dur": func(v any) string {
		var d time.Duration
		switch t := v.(type) {
		case time.Duration:
			d = t
		case config.Duration:
			d = t.D()
		}
		if d <= 0 {
			return "—"
		}
		d = d.Truncate(time.Second)
		if d >= time.Hour {
			return fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
		}
		if d >= time.Minute {
			return fmt.Sprintf("%dm", int(d.Minutes()))
		}
		return d.String()
	},
	"hhmm": func(t time.Time) string { return t.Local().Format("15:04") },
	"join": strings.Join,
}

const boardHTML = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta http-equiv="refresh" content="20">
<title>Le Veilleur — {{.House}}</title>
<style>
:root{--bg:#0a0b0e;--fg:#e9e6dc;--dim:#7d8390;--acc:#c8ff00;--up:#c8ff00;--down:#4a4f5a;--warn:#ffb347;--line:#1c1f26}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--fg);font-family:"IBM Plex Mono",ui-monospace,Menlo,Consolas,monospace;font-size:15px;line-height:1.5}
main{max-width:900px;margin:0 auto;padding:24px 16px 64px}
img.mark{max-width:100%;height:auto;display:block;margin-bottom:8px}
h2{font-size:13px;letter-spacing:.18em;text-transform:uppercase;color:var(--dim);margin:32px 0 10px;font-weight:600}
table{width:100%;border-collapse:collapse}
th{text-align:left;font-size:11px;letter-spacing:.12em;text-transform:uppercase;color:var(--dim);font-weight:600;padding:6px 8px;border-bottom:1px solid var(--line)}
td{padding:8px;border-bottom:1px solid var(--line);vertical-align:top}
.dot{display:inline-block;width:8px;height:8px;border-radius:50%;margin-right:8px;vertical-align:middle}
.on{background:var(--up);box-shadow:0 0 8px var(--up)}
.off{background:var(--down)}
.name{color:var(--fg);font-weight:600}
.dim{color:var(--dim)}
.acc{color:var(--acc)}
.warn{color:var(--warn)}
.why{font-size:12px;color:var(--dim)}
form.inline{display:inline}
button,select,input{font:inherit;background:#12151b;color:var(--fg);border:1px solid var(--line);border-radius:3px;padding:5px 9px}
button{cursor:pointer;border-color:#2a2f3a}
button:hover{border-color:var(--acc);color:var(--acc)}
.bar{display:flex;gap:8px;align-items:center;flex-wrap:wrap;margin-top:10px}
.top{display:flex;justify-content:space-between;align-items:baseline;gap:12px;flex-wrap:wrap;color:var(--dim);font-size:12px}
.err{color:#ff6b6b}
code{color:var(--acc)}
</style></head>
<body><main>
<img class="mark" src="/static/logo-animated.svg" alt="Le Squat — le veilleur" width="640" height="200">
<div class="top">
  <span>signed in: <span class="acc">{{.Identity.User}}</span></span>
  <span>observed {{hhmm .Board.At}} via {{.Board.Source}} · <code>{{.Version}}</code></span>
</div>
{{if .Board.ObserveErr}}<p class="err">⚠ cannot see the fleet — nothing will be started or stopped until this clears: {{.Board.ObserveErr}}</p>{{end}}

<h2>The fleet</h2>
<table>
<tr><th>target</th><th>state</th><th>why</th></tr>
{{range .Board.Targets}}
<tr>
  <td><span class="dot {{if .Up}}on{{else}}off{{end}}"></span><span class="name">{{.Name}}</span>
      <div class="why">{{.Kind}}{{if .VMID}} {{.VMID}}{{end}} · on {{.Node}}{{if .Label}} — {{.Label}}{{end}}</div></td>
  <td>{{if .Up}}up{{else}}asleep{{end}}
      {{if .Pending}}<div class="why warn">{{.Pending}}…</div>{{end}}
      {{if not .Managed}}<div class="why">observe only</div>{{end}}</td>
  <td class="why">
    {{if .Wanted}}held by {{len .WantedBy}} claim(s){{else}}nobody needs it{{end}}
    {{if .UnwantedFor}}<br>idle {{dur .UnwantedFor}}{{end}}
    {{if .Blocked}}<br><span class="warn">staying up: {{.Blocked}}</span>{{end}}
    {{if .LastError}}<br><span class="err">{{.LastError}}</span>{{end}}
  </td>
</tr>
{{end}}
</table>

<h2>Keep something up</h2>
<form class="bar" method="post" action="/ui/hold">
  <select name="target">{{range .Targets}}<option value="{{.}}">{{.}}</option>{{end}}</select>
  <select name="hold"><option value="1h">1 h</option><option value="2h" selected>2 h</option><option value="4h">4 h</option><option value="8h">8 h</option></select>
  <input name="reason" placeholder="why?" size="28">
  <button type="submit">hold it</button>
</form>

<h2>Claims</h2>
<table>
<tr><th>#</th><th>who</th><th>target</th><th>why</th><th>state</th><th></th></tr>
{{range .Claims}}
<tr>
  <td class="dim">{{.Seq}}</td>
  <td>{{.Subject}}<div class="why">{{.Via}}</div></td>
  <td>{{.Target}}</td>
  <td class="why">{{.Reason}}</td>
  <td>{{if eq (printf "%s" .State) "held"}}<span class="acc">held</span><div class="why">{{dur .Left}} left</div>
      {{else}}<span class="dim">{{.State}}</span>{{if .ReleasedBy}}<div class="why">{{.ReleasedBy}}</div>{{end}}{{end}}</td>
  <td>{{if eq (printf "%s" .State) "held"}}<form class="inline" method="post" action="/ui/release"><input type="hidden" name="id" value="{{.ID}}"><button type="submit">release</button></form>{{end}}</td>
</tr>
{{end}}
</table>
<p class="why">A target is up while any claim on it — or on anything that requires it — is held.
Guards only ever refuse to stop something. Everything expires.</p>
</main></body></html>
`
