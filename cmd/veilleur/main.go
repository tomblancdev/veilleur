// Le Veilleur — the watchman of a home lab: machines are awake exactly as
// long as something says they are in use, and asleep the rest of the time.
//
// It never powers a 24/7 node, never stops anything it does not manage, and
// when it cannot see the fleet it does nothing at all.
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
	"github.com/tomblancdev/veilleur/internal/state"
	"github.com/tomblancdev/veilleur/internal/web"
)

// set by -ldflags "-X main.version=..."
var version = "dev"

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "veilleur")
	slog.SetDefault(logger)

	dir := os.Getenv("VEILLEUR_CONFIG_DIR")
	if dir == "" {
		dir = "/etc/veilleur"
	}
	cfg, err := config.Load(dir)
	if err != nil {
		logger.Error("config", "err", err)
		os.Exit(1)
	}
	if v := os.Getenv("VEILLEUR_LISTEN"); v != "" {
		cfg.Listen = v
	}
	if v := os.Getenv("VEILLEUR_DATA_DIR"); v != "" {
		cfg.DataDir = v
	}
	if loc, err := time.LoadLocation(cfg.TZ); err == nil {
		time.Local = loc
	}

	st, err := state.Open(cfg.DataDir)
	if err != nil {
		logger.Error("state", "err", err)
		os.Exit(1)
	}
	a, err := auth.New(cfg.Auth)
	if err != nil {
		logger.Error("auth", "err", err)
		os.Exit(1)
	}

	var dr door.Door
	switch cfg.DoorCfg.Mode {
	case "mock":
		m := door.NewMock("mock")
		for _, n := range cfg.SignalNames() {
			m.Signals[n] = 1
		}
		for _, n := range cfg.TargetNames() {
			m.State[n] = 1
		}
		dr = m
		logger.Warn("door mode is mock — no machine will be touched")
	default:
		dr, err = door.NewSSH(cfg.DoorCfg)
		if err != nil {
			logger.Error("door", "err", err)
			os.Exit(1)
		}
	}

	engine := fleet.New(cfg, st, dr, logger)
	srv := web.New(cfg, st, engine, a, version, logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go engine.Run(ctx)

	hs := &http.Server{Addr: cfg.Listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sd, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = hs.Shutdown(sd)
	}()
	logger.Info("keeping watch", "addr", cfg.Listen, "version", version,
		"targets", len(cfg.Targets), "signals", len(cfg.Signals))
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("http", "err", err)
		os.Exit(1)
	}
	logger.Info("good night")
}
