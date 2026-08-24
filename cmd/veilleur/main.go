// Le Veilleur — the watchman of a home lab: machines are awake exactly as
// long as somebody has claimed them, and asleep the rest of the time.
//
// It never powers a 24/7 node, never touches an HA resource, and when it
// cannot see the fleet it does nothing at all.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata"

	"github.com/tomblancdev/veilleur/internal/auth"
	"github.com/tomblancdev/veilleur/internal/config"
	"github.com/tomblancdev/veilleur/internal/door"
	"github.com/tomblancdev/veilleur/internal/fleet"
	"github.com/tomblancdev/veilleur/internal/store"
	"github.com/tomblancdev/veilleur/internal/web"
)

// set by -ldflags "-X main.version=..."
var version = "dev"

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "veilleur")
	slog.SetDefault(logger)

	cfg, err := config.Load(os.Getenv("VEILLEUR_CONFIG"))
	if err != nil {
		logger.Error("config", "err", err)
		os.Exit(1)
	}
	if loc, err := time.LoadLocation(cfg.TZ); err == nil {
		time.Local = loc
	}

	st, err := store.Open(cfg.DataDir)
	if err != nil {
		logger.Error("store", "err", err)
		os.Exit(1)
	}
	a, err := auth.New(cfg.Auth)
	if err != nil {
		logger.Error("auth", "err", err)
		os.Exit(1)
	}

	var fl door.Fleet
	switch cfg.DoorCfg.Mode {
	case "mock":
		// local hacking only: an imaginary fleet that never touches a machine
		m := door.NewMock()
		for _, name := range cfg.TargetNames() {
			t := cfg.Targets[name]
			if t.Kind == config.KindNode {
				m.NodesUp[t.Node] = false
				m.NodeTTYs[t.Node] = 0
				m.Total++
			} else {
				m.AddGuest(t.VMID, t.Node)
			}
		}
		fl = m
		logger.Warn("door mode is mock — no machine will be touched")
	default:
		fl, err = door.NewSSH(cfg.DoorCfg)
		if err != nil {
			logger.Error("door", "err", err)
			os.Exit(1)
		}
	}

	engine := fleet.New(cfg, st, fl, logger)
	srv := web.New(cfg, st, engine, a, version, logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go engine.Run(ctx)

	hs := &http.Server{Addr: cfg.Listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = hs.Shutdown(shutdown)
	}()
	logger.Info("keeping watch", "addr", cfg.Listen, "version", version, "targets", len(cfg.Targets))
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("http", "err", err)
		os.Exit(1)
	}
	logger.Info("good night")
}
