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
		fmt.Printf("beaconx %s（commit %s，构建于 %s）\n", v.Version, v.Commit, v.BuildTime)
		return 0
	case "help", "-h", "--help":
		usage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "未知命令 %q\n\n", args[0])
		usage()
		return 2
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `beaconx — 自动化安全测试平台

用法：
  beaconx [serve] [flags]     启动服务端
  beaconx migrate [flags]     执行待应用的数据库迁移并退出
  beaconx healthcheck [flags] 探测运行中的实例
  beaconx version             打印构建元数据
  beaconx help                显示本帮助

环境变量：
  BEACONX_CONFIG      配置文件路径（默认：./config.json）
  BEACONX_ADDR        监听地址（默认：:6969）
  BEACONX_DB_DSN      PostgreSQL DSN
  BEACONX_HOME        状态目录（默认：./data）
  BEACONX_LOG_LEVEL   debug|info|warn|error
  BEACONX_LOG_FORMAT  json|text
`)
}

// newFlagSet 创建带中文用法说明的 FlagSet。
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "%s 用法：\n", name)
		fs.PrintDefaults()
	}
	return fs
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
	fs := newFlagSet("serve")
	cfgPath := fs.String("config", "", "配置文件路径")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	cfg, logger, err := loadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "加载配置失败：%v\n", err)
		return 1
	}
	log := logger.With("service", "beaconx", "version", version.Version)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	state := platformstate.New(cfg.State.Dir)
	if err := state.Ensure(); err != nil {
		log.Error("准备状态目录", "error", err)
		return 1
	}
	if err := state.EnsureTenant(tenant.ID(cfg.Tenant.DefaultID)); err != nil {
		log.Error("准备租户目录", "error", err)
		return 1
	}

	db, err := openDB(ctx, cfg, log)
	if err != nil {
		log.Error("连接数据库", "error", err)
		return 1
	}
	defer db.Close()

	latest, res, err := applyMigrations(ctx, db, log)
	if err != nil {
		log.Error("执行数据库迁移", "error", err)
		return 1
	}
	log.Info("数据库迁移就绪", "applied", res.Applied, "schema_version", res.Current, "latest", latest)

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
		log.Error("构建 HTTP 处理器", "error", err)
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
		log.Info("HTTP 服务已监听", "addr", cfg.Server.Addr)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("HTTP 服务器异常退出", "error", err)
			return 1
		}
	case <-ctx.Done():
		stop()
		log.Info("收到关闭请求")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout.Value())
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Error("优雅关闭失败", "error", err)
			return 1
		}
	}

	log.Info("已停止")
	return 0
}

func migrateCmd(args []string) int {
	fs := newFlagSet("migrate")
	cfgPath := fs.String("config", "", "配置文件路径")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	cfg, logger, err := loadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "加载配置失败：%v\n", err)
		return 1
	}
	log := logger.With("service", "beaconx")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := openDB(ctx, cfg, log)
	if err != nil {
		log.Error("连接数据库", "error", err)
		return 1
	}
	defer db.Close()

	latest, res, err := applyMigrations(ctx, db, log)
	if err != nil {
		log.Error("执行数据库迁移", "error", err)
		return 1
	}
	log.Info("数据库迁移完成", "applied", res.Applied, "schema_version", res.Current, "latest", latest)
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

// openDB 会短暂重试，使 compose 栈可以同时启动数据库与应用，
// 而不会陷入崩溃循环。
func openDB(ctx context.Context, cfg config.Config, log *slog.Logger) (*pg.DB, error) {
	opts := pg.Options{
		DSN:            cfg.Database.DSN,
		MaxConns:       cfg.Database.MaxConns,
		MinConns:       cfg.Database.MinConns,
		ConnectTimeout: cfg.Database.ConnectTimeout.Value(),
	}
	if opts.DSN == "" {
		return nil, errors.New("database.dsn 为必填项（可通过 BEACONX_DB_DSN 或 database.dsn 设置）")
	}
	var lastErr error
	for attempt := 1; attempt <= 15; attempt++ {
		db, err := pg.Open(ctx, opts)
		if err == nil {
			return db, nil
		}
		lastErr = err
		log.Warn("数据库尚未就绪", "attempt", attempt, "error", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return nil, fmt.Errorf("重试后数据库仍不可用：%w", lastErr)
}

func healthcheck(args []string) int {
	fs := newFlagSet("healthcheck")
	addr := fs.String("addr", "http://127.0.0.1:6969/healthz", "健康检查端点 URL")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(*addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "健康检查失败：%v\n", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "健康检查失败：状态码 %d\n", resp.StatusCode)
		return 1
	}
	fmt.Println("ok")
	return 0
}
