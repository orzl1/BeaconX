package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/orzl1/BeaconX/internal/app/system"
	"github.com/orzl1/BeaconX/internal/config"
	"github.com/orzl1/BeaconX/internal/httpapi"
	"github.com/orzl1/BeaconX/internal/infra/migrate"
	"github.com/orzl1/BeaconX/internal/infra/pg"
	"github.com/orzl1/BeaconX/internal/logging"
	platformstate "github.com/orzl1/BeaconX/internal/platform"
	"github.com/orzl1/BeaconX/internal/tenant"
	"github.com/orzl1/BeaconX/internal/version"
	"github.com/orzl1/BeaconX/migrations"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		return serve(nil)
	}
	switch args[0] {
	case "serve":
		return serve(args[1:])
	case "migrate":
		return migrateCmd(args[1:])
	case "healthcheck":
		return healthcheck(args[1:])
	case "version", "--version", "-v":
		v := version.Current()
		fmt.Printf("beaconx %s (commit %s, built %s)\n", v.Version, v.Commit, v.BuildTime)
		return 0
	case "help", "-h", "--help":
		usage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", args[0])
		usage()
		return 2
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `beaconx — autonomous security testing platform

Usage:
  beaconx [serve] [flags]     start the server
  beaconx migrate [flags]     apply pending database migrations and exit
  beaconx healthcheck [flags] probe a running instance
  beaconx version             print build metadata
  beaconx help                show this help

Environment:
  BEACONX_CONFIG       config file path (default: ./config.json)
  BEACONX_ADDR         listen address (default: :8787)
  BEACONX_DB_DSN       PostgreSQL DSN
  BEACONX_HOME         state directory (default: ./data)
  BEACONX_LOG_LEVEL    debug|info|warn|error
  BEACONX_LOG_FORMAT   json|text
`)
}

func configPathFrom(cfgPath string) string {
	if cfgPath != "" {
		return cfgPath
	}
	if v := os.Getenv("BEACONX_CONFIG"); v != "" {
		return v
	}
	return config.DefaultPath
}

func loadConfig(cfgPath string) (config.Config, *slog.Logger, error) {
	cfg, err := config.LoadFrom(configPathFrom(cfgPath), os.Getenv)
	if err != nil {
		return config.Config{}, nil, err
	}
	logger, err := logging.New(cfg.Log)
	if err != nil {
		return config.Config{}, nil, err
	}
	return cfg, logger, nil
}

func serve(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "config file path")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, logger, err := loadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "configure: %v\n", err)
		return 1
	}
	log := logger.With("service", "beaconx", "version", version.Version)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	state := platformstate.New(cfg.State.Dir)
	if err := state.Ensure(); err != nil {
		log.Error("prepare state directory", "error", err)
		return 1
	}
	if err := state.EnsureTenant(tenant.ID(cfg.Tenant.DefaultID)); err != nil {
		log.Error("prepare tenant directory", "error", err)
		return 1
	}

	db, err := openDB(ctx, cfg, log)
	if err != nil {
		log.Error("connect database", "error", err)
		return 1
	}
	defer db.Close()

	latest, res, err := applyMigrations(ctx, db, log)
	if err != nil {
		log.Error("apply migrations", "error", err)
		return 1
	}
	log.Info("migrations ready", "applied", res.Applied, "schema_version", res.Current, "latest", latest)

	sys := &system.Service{
		DB:              db,
		LatestMigration: latest,
		Build: system.BuildInfo{
			Version:   version.Version,
			Commit:    version.Commit,
			BuildTime: version.BuildTime,
		},
		StartedAt: time.Now(),
	}

	handler, err := httpapi.NewHandler(httpapi.Options{
		Logger:         log,
		RequestTimeout: cfg.Server.RequestTimeout.Value(),
		MaxBodyBytes:   cfg.Server.MaxBodyBytes,
		TenantResolver: httpapi.StaticTenant(tenant.ID(cfg.Tenant.DefaultID)),
		Info:           func(ctx context.Context) any { return sys.NewInfo(ctx) },
		Ready: func(ctx context.Context) (any, bool) {
			r := sys.Readiness(ctx)
			return r, r.Ready
		},
	})
	if err != nil {
		log.Error("build http handler", "error", err)
		return 1
	}

	srv := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           handler,
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout.Value(),
		IdleTimeout:       cfg.Server.IdleTimeout.Value(),
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("http listening", "addr", cfg.Server.Addr)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server failed", "error", err)
			return 1
		}
	case <-ctx.Done():
		stop()
		log.Info("shutdown requested")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout.Value())
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Error("graceful shutdown failed", "error", err)
			return 1
		}
	}

	log.Info("stopped")
	return 0
}

func migrateCmd(args []string) int {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "config file path")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, logger, err := loadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "configure: %v\n", err)
		return 1
	}
	log := logger.With("service", "beaconx")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := openDB(ctx, cfg, log)
	if err != nil {
		log.Error("connect database", "error", err)
		return 1
	}
	defer db.Close()

	latest, res, err := applyMigrations(ctx, db, log)
	if err != nil {
		log.Error("apply migrations", "error", err)
		return 1
	}
	log.Info("migrations complete", "applied", res.Applied, "schema_version", res.Current, "latest", latest)
	return 0
}

func applyMigrations(ctx context.Context, db *pg.DB, log *slog.Logger) (int64, migrate.Result, error) {
	ms, err := migrate.Load(migrations.FS)
	if err != nil {
		return 0, migrate.Result{}, err
	}
	res, err := migrate.Apply(ctx, db.Pool(), migrations.FS)
	if err != nil {
		return 0, migrate.Result{}, err
	}
	return migrate.Latest(ms), res, nil
}

// openDB retries briefly so a compose stack can start the database and the app
// at the same time without a crash loop.
func openDB(ctx context.Context, cfg config.Config, log *slog.Logger) (*pg.DB, error) {
	opts := pg.Options{
		DSN:            cfg.Database.DSN,
		MaxConns:       cfg.Database.MaxConns,
		MinConns:       cfg.Database.MinConns,
		ConnectTimeout: cfg.Database.ConnectTimeout.Value(),
	}
	if opts.DSN == "" {
		return nil, errors.New("database.dsn is required (set BEACONX_DB_DSN or database.dsn)")
	}
	var lastErr error
	for attempt := 1; attempt <= 15; attempt++ {
		db, err := pg.Open(ctx, opts)
		if err == nil {
			return db, nil
		}
		lastErr = err
		log.Warn("database not ready", "attempt", attempt, "error", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return nil, fmt.Errorf("database unavailable after retries: %w", lastErr)
}

func healthcheck(args []string) int {
	fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	addr := fs.String("addr", "http://127.0.0.1:8787/healthz", "health endpoint URL")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(*addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck failed: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck failed: status %d\n", resp.StatusCode)
		return 1
	}
	fmt.Println("ok")
	return 0
}
