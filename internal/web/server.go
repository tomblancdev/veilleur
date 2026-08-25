// Package web is the watchman's HTTP surface: the board, the JSON API, and
// the house contract — /healthz, /metrics, /openapi.json.
package web

import (
	"context"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/tomblancdev/veilleur/internal/auth"
	"github.com/tomblancdev/veilleur/internal/config"
	"github.com/tomblancdev/veilleur/internal/fleet"
	"github.com/tomblancdev/veilleur/internal/state"
	"github.com/tomblancdev/veilleur/ui"
)

// Server holds what every handler needs.
type Server struct {
	cfg     *config.Config
	store   *state.Store
	engine  *fleet.Engine
	auth    *auth.Auth
	log     *slog.Logger
	version string
	started time.Time
	page    *template.Template
}

// New builds the surface.
func New(cfg *config.Config, st *state.Store, e *fleet.Engine, a *auth.Auth, version string, log *slog.Logger) *Server {
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
	mux.HandleFunc("POST /api/targets/{name}/wake", s.apiWake)
	mux.HandleFunc("GET /api/signals", s.apiSignals)
	mux.HandleFunc("GET /api/holds", s.apiHolds)
	mux.HandleFunc("POST /api/holds", s.apiTakeHold)
	mux.HandleFunc("DELETE /api/holds/{id}", s.apiReleaseHold)

	mux.HandleFunc("POST /ui/wake", s.uiWake)
	mux.HandleFunc("POST /ui/hold", s.uiHold)
	mux.HandleFunc("POST /ui/release", s.uiRelease)
	mux.HandleFunc("GET /{$}", s.board)
	return mux
}

// healthz: 503 while the fleet cannot be observed at all.
func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	ok, why := s.engine.Healthy()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if !ok {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintf(w, "degraded: %s\n", why)
		return
	}
	fmt.Fprintln(w, "ok")
}

func (s *Server) metrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	fmt.Fprintf(w, "# HELP veilleur_build_info Build information.\n# TYPE veilleur_build_info gauge\nveilleur_build_info{version=%q} 1\n", s.version)
	fmt.Fprintf(w, "# HELP veilleur_uptime_seconds Seconds since start.\n# TYPE veilleur_uptime_seconds gauge\nveilleur_uptime_seconds %d\n", int64(time.Since(s.started).Seconds()))
	fmt.Fprint(w, s.engine.Metrics())
}

// --- the page -------------------------------------------------------------

type boardData struct {
	House    string
	Version  string
	Identity auth.Identity
	Board    fleet.Board
	Targets  []string
	Now      time.Time
}

func (s *Server) board(w http.ResponseWriter, r *http.Request) {
	id := s.identify(r)
	if !id.IsAdmin() {
		http.Error(w, "the watchman's board is for admins", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := boardData{House: s.cfg.House, Version: s.version, Identity: id,
		Board: s.engine.Board(), Targets: s.cfg.TargetNames(), Now: time.Now()}
	if err := s.page.Execute(w, data); err != nil {
		s.log.Error("render", "err", err)
	}
}

func (s *Server) uiWake(w http.ResponseWriter, r *http.Request) {
	id := s.identify(r)
	if !id.IsAdmin() {
		http.Error(w, "admins only", http.StatusForbidden)
		return
	}
	_ = r.ParseForm()
	name := r.FormValue("target")
	if _, ok := s.cfg.Targets[name]; !ok {
		http.Error(w, "no such target", http.StatusBadRequest)
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		if err := s.engine.Wake(ctx, name, "the board ("+id.User+")"); err != nil {
			s.log.Error("wake failed", "target", name, "err", err)
		}
	}()
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) uiHold(w http.ResponseWriter, r *http.Request) {
	id := s.identify(r)
	if !id.IsAdmin() {
		http.Error(w, "admins only", http.StatusForbidden)
		return
	}
	_ = r.ParseForm()
	reason := strings.TrimSpace(r.FormValue("reason"))
	if reason == "" {
		http.Error(w, "a hold needs a reason — it may outlive your memory of it", http.StatusBadRequest)
		return
	}
	h, err := s.store.Take(state.Hold{
		Target: r.FormValue("target"), By: id.User, Reason: reason,
		HandsOff: r.FormValue("hands_off") == "on",
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.log.Info("hold taken", "hold", h.ID, "target", h.Target, "by", h.By, "reason", h.Reason, "hands_off", h.HandsOff)
	s.engine.Kick()
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) uiRelease(w http.ResponseWriter, r *http.Request) {
	id := s.identify(r)
	if !id.IsAdmin() {
		http.Error(w, "admins only", http.StatusForbidden)
		return
	}
	_ = r.ParseForm()
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
		switch {
		case d >= 24*time.Hour:
			return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
		case d >= time.Hour:
			return fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
		case d >= time.Minute:
			return fmt.Sprintf("%dm", int(d.Minutes()))
		}
		return d.String()
	},
	"hhmm":  func(t time.Time) string { return t.Local().Format("15:04") },
	"since": func(t time.Time) string { return time.Since(t).Truncate(time.Minute).String() },
	"join":  strings.Join,
}

const boardHTML = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta http-equiv="refresh" content="20"><title>Le Veilleur — {{.House}}</title>
<style>
:root{--bg:#0a0b0e;--fg:#e9e6dc;--dim:#7d8390;--acc:#c8ff00;--up:#c8ff00;--down:#4a4f5a;--warn:#ffb347;--err:#ff6b6b;--line:#1c1f26}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--fg);font-family:"IBM Plex Mono",ui-monospace,Menlo,Consolas,monospace;font-size:15px;line-height:1.5}
main{max-width:940px;margin:0 auto;padding:24px 16px 64px}
img.mark{max-width:100%;height:auto;display:block;margin-bottom:8px}
h2{font-size:13px;letter-spacing:.18em;text-transform:uppercase;color:var(--dim);margin:32px 0 10px;font-weight:600}
table{width:100%;border-collapse:collapse}
th{text-align:left;font-size:11px;letter-spacing:.12em;text-transform:uppercase;color:var(--dim);font-weight:600;padding:6px 8px;border-bottom:1px solid var(--line)}
td{padding:8px;border-bottom:1px solid var(--line);vertical-align:top}
.dot{display:inline-block;width:8px;height:8px;border-radius:50%;margin-right:8px;vertical-align:middle}
.on{background:var(--up);box-shadow:0 0 8px var(--up)}.off{background:var(--down)}
.name{font-weight:600}.dim{color:var(--dim)}.acc{color:var(--acc)}.warn{color:var(--warn)}.err{color:var(--err)}
.why{font-size:12px;color:var(--dim)}
form.inline{display:inline}
button,select,input{font:inherit;background:#12151b;color:var(--fg);border:1px solid var(--line);border-radius:3px;padding:5px 9px}
button{cursor:pointer;border-color:#2a2f3a}button:hover{border-color:var(--acc);color:var(--acc)}
.bar{display:flex;gap:8px;align-items:center;flex-wrap:wrap;margin-top:10px}
.top{display:flex;justify-content:space-between;align-items:baseline;gap:12px;flex-wrap:wrap;color:var(--dim);font-size:12px}
.held{border-left:3px solid var(--warn);padding-left:10px;margin:8px 0}
code{color:var(--acc)}
</style></head><body><main>
<img class="mark" src="/static/logo-animated.svg" alt="Le Squat — le veilleur" width="640" height="200">
<div class="top"><span>signed in: <span class="acc">{{.Identity.User}}</span></span>
<span>observed {{hhmm .Board.At}} · <code>{{.Version}}</code></span></div>
{{if .Board.ObserveErr}}<p class="err">⚠ {{.Board.ObserveErr}} — nothing will be started or stopped until this clears.</p>{{end}}

{{$anyHold := false}}{{range .Board.Targets}}{{if .Holds}}{{$anyHold = true}}{{end}}{{end}}
{{if $anyHold}}<h2>Held by a person</h2>
{{range .Board.Targets}}{{range .Holds}}
<div class="held"><span class="warn">{{.Target}}</span>{{if .HandsOff}} <span class="dim">(hands-off)</span>{{end}}
 — {{.Reason}}<div class="why">{{.By}}, {{since .Since}} ago
 <form class="inline" method="post" action="/ui/release"><input type="hidden" name="id" value="{{.ID}}"><button type="submit">lift</button></form></div></div>
{{end}}{{end}}{{end}}

<h2>The fleet</h2>
<table><tr><th>target</th><th>state</th><th>why it is not stopping</th></tr>
{{range .Board.Targets}}
<tr><td><span class="dot {{if .Up}}on{{else}}off{{end}}"></span><span class="name">{{.Name}}</span>
 <div class="why">{{.Kind}}{{if .Node}} on {{.Node}}{{end}}{{if .Label}} — {{.Label}}{{end}}</div></td>
<td>{{if .Up}}up{{if .UpFor}} <span class="why">{{dur .UpFor}}</span>{{end}}{{else}}{{if .Known}}asleep{{else}}<span class="warn">unknown</span>{{end}}{{end}}
 {{if .Pending}}<div class="why warn">{{.Pending}}…</div>{{end}}
 {{if not .Managed}}<div class="why">observed only — never stopped</div>{{end}}</td>
<td class="why">{{if .Up}}{{if .Blocked}}<span class="warn">{{.Blocked}}</span>{{else}}{{if .Managed}}stopping{{else}}not mine to stop{{end}}{{end}}
 {{if .QuietFor}}<br>quiet {{dur .QuietFor}}{{end}}{{end}}
 {{if .LastError}}<br><span class="err">{{.LastError}}</span>{{end}}</td></tr>
{{end}}</table>

<h2>Wake something</h2>
<form class="bar" method="post" action="/ui/wake">
 <select name="target">{{range .Targets}}<option value="{{.}}">{{.}}</option>{{end}}</select>
 <button type="submit">wake it</button></form>

<h2>Hold something up</h2>
<form class="bar" method="post" action="/ui/hold">
 <select name="target">{{range .Targets}}<option value="{{.}}">{{.}}</option>{{end}}</select>
 <input name="reason" placeholder="why? (a hold has no expiry)" size="34">
 <label class="why"><input type="checkbox" name="hands_off"> hands-off</label>
 <button type="submit">hold</button></form>

<h2>Signals</h2>
<table><tr><th>signal</th><th>answer</th><th>means</th></tr>
{{range $n, $v := .Board.Signals}}<tr><td class="name">{{$n}}</td>
<td>{{if $v.Known}}{{if $v.True}}<span class="acc">yes</span>{{else}}<span class="dim">no</span>{{end}}{{else}}<span class="warn">unknown</span>{{end}}</td>
<td class="why">{{if $v.Err}}<span class="err">{{$v.Err}}</span>{{end}}</td></tr>{{end}}
</table>
<p class="why">A signal that cannot be answered blocks a stop; it never permits one.
Only a target Le Veilleur <em>manages</em> is ever stopped — anything else simply keeps its machine up.</p>
</main></body></html>
`


