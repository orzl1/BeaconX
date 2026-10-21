package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/orzl1/BeaconX/internal/app/ai"
	"github.com/orzl1/BeaconX/internal/app/events"
	"github.com/orzl1/BeaconX/internal/app/evidence"
	"github.com/orzl1/BeaconX/internal/app/executor"
	"github.com/orzl1/BeaconX/internal/app/runner"
	"github.com/orzl1/BeaconX/internal/app/system"
	"github.com/orzl1/BeaconX/internal/app/tools"
	"github.com/orzl1/BeaconX/internal/config"
	"github.com/orzl1/BeaconX/internal/domain/policy"
	"github.com/orzl1/BeaconX/internal/httpapi"
	"github.com/orzl1/BeaconX/internal/infra/llm"
	"github.com/orzl1/BeaconX/internal/infra/migrate"
	"github.com/orzl1/BeaconX/internal/infra/pg"
	"github.com/orzl1/BeaconX/internal/infra/selfupdate"
	"github.com/orzl1/BeaconX/internal/logging"
	platformstate "github.com/orzl1/BeaconX/internal/platform"
	"github.com/orzl1/BeaconX/internal/tenant"
	"github.com/orzl1/BeaconX/internal/version"
	"github.com/orzl1/BeaconX/internal/webui"
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
  BEACONX_ADMIN_TOKEN 管理接口令牌（为空时启动生成临时令牌）
  BEACONX_MAX_CONCURRENT     最大并发任务数（默认 2）
  BEACONX_PLANNER_INTERVAL   规划轮询间隔（默认 2s）
  BEACONX_WORKER_INTERVAL    执行轮询间隔（默认 200ms）
  BEACONX_LEASE_TTL          动作租约时长（默认 30s）
  BEACONX_HEARTBEAT_INTERVAL 租约续约间隔（默认 10s）
  BEACONX_LLM_BASE_URL        OpenAI 兼容端点（与 MODEL 同时设置才启用 AI）
  BEACONX_LLM_API_KEY         模型 API Key
  BEACONX_LLM_MODEL           模型名（如 deepseek-flash）
  BEACONX_LLM_MAX_TOKENS      单次最大输出 token（默认 2048）
  BEACONX_LLM_TIMEOUT         单次请求超时（默认 60s）
  BEACONX_LLM_MAX_RETRIES     429/5xx 重试次数（默认 2）
  BEACONX_POLICY_DEFAULT_ACTION 无规则命中时的动作 allow|ask|deny（默认 ask）
  BEACONX_APPROVAL_TIMEOUT     审批等待超时（默认 60s）
  BEACONX_EVIDENCE_DIR         证据目录（默认状态目录下 tenants/<id>/evidence）
  BEACONX_EVIDENCE_KEY         证据加密密钥（64 位十六进制；为空时自动生成 evidence.key）
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

	// 任务运行时：内存事件总线 + 引擎（M2 默认使用空闲规划器与空执行器）。
	broker := events.NewBroker(512)
	store := pg.NewStore(db)
	sink := func(ev pg.MissionEvent) {
		var payload json.RawMessage
		if ev.Payload != nil {
			if raw, err := json.Marshal(ev.Payload); err == nil {
				payload = raw
			}
		}
		broker.Publish(events.Event{
			ID:        ev.ID,
			MissionID: ev.MissionID,
			Type:      ev.Type,
			Payload:   payload,
			CreatedAt: ev.CreatedAt,
		})
	}
	// 证据存储：正文加密落盘，快照带签名；密钥优先取配置，否则在状态目录生成并复用。
	evDir := cfg.Evidence.Dir
	if evDir == "" {
		evDir = filepath.Join(cfg.State.Dir, "tenants", strconv.FormatInt(cfg.Tenant.DefaultID, 10), "evidence")
	}
	evKey, err := loadOrCreateEvidenceKey(cfg.Evidence.KeyHex, filepath.Join(cfg.State.Dir, "evidence.key"))
	if err != nil {
		log.Error("准备证据密钥失败", "error", err)
		return 1
	}
	evStore, err := evidence.New(evidence.Config{Dir: evDir, Key: evKey})
	if err != nil {
		log.Error("初始化证据存储失败", "error", err)
		return 1
	}

	// 工具与策略：内置默认规则幂等写入；执行器是所有工具调用的唯一入口。
	if err := store.EnsurePolicyRules(ctx, cfg.Tenant.DefaultID, policy.SeedDefaults()); err != nil {
		log.Error("写入内置策略规则失败", "error", err)
		return 1
	}
	registry, err := tools.NewRegistry(tools.NewHTTPProbe(), tools.NewShellExec())
	if err != nil {
		log.Error("初始化工具注册表失败", "error", err)
		return 1
	}
	exec := executor.New(store, registry, executor.Config{
		TenantID:      cfg.Tenant.DefaultID,
		DefaultAction: policy.Action(cfg.Policy.DefaultAction),
		AskTimeout:    cfg.Policy.ApprovalTimeout.Value(),
		Evidence:      evStore,
	}, log)

	// 规划/执行实现：配置了模型端点时走真实 LLM，否则使用 M2 的空实现。
	var (
		planner runner.Planner = runner.IdlePlanner{}
		worker  runner.Worker  = runner.NoopWorker{}
		aiMode  string         = "idle"
	)
	if cfg.LLM.BaseURL != "" && cfg.LLM.Model != "" {
		profiles := []llm.Profile{{
			Name:    "primary",
			BaseURL: cfg.LLM.BaseURL,
			APIKey:  cfg.LLM.APIKey,
			Model:   cfg.LLM.Model,
		}}
		for i, fb := range cfg.LLM.Fallbacks {
			profiles = append(profiles, llm.Profile{
				Name:    fmt.Sprintf("fallback-%d", i+1),
				BaseURL: fb.BaseURL,
				APIKey:  fb.APIKey,
				Model:   fb.Model,
			})
		}
		router, err := llm.NewRouter(profiles, cfg.LLM.Timeout.Value(), cfg.LLM.MaxRetries, log)
		if err != nil {
			log.Error("初始化模型路由失败", "error", err)
			return 1
		}
		planner = ai.NewPlanner(store, router, cfg.LLM.Model, ai.Config{}, log)
		worker = ai.NewWorker(store, router, cfg.LLM.Model, log).SetTools(exec, llmToolDefs(registry.List("worker")))
		aiMode = "llm"
	}
	engine := runner.New(runner.Config{
		TenantID:          cfg.Tenant.DefaultID,
		MaxConcurrent:     cfg.Engine.MaxConcurrent,
		PlannerInterval:   cfg.Engine.PlannerInterval.Value(),
		WorkerInterval:    cfg.Engine.WorkerInterval.Value(),
		LeaseTTL:          cfg.Engine.LeaseTTL.Value(),
		HeartbeatInterval: cfg.Engine.HeartbeatInterval.Value(),
		Owner:             fmt.Sprintf("beaconx-%d", os.Getpid()),
	}, store, planner, worker, log, sink)
	log.Info("规划模式", "mode", aiMode, "model", cfg.LLM.Model)
	if err := engine.Start(ctx); err != nil {
		log.Error("启动任务运行时失败", "error", err)
		return 1
	}

	adminToken, err := resolveAdminToken(cfg.Auth.AdminToken, log)
	if err != nil {
		log.Error("准备管理令牌失败", "error", err)
		return 1
	}

	exePath, _ := os.Executable()
	updater := selfupdate.New(selfupdate.Config{
		Repo:           cfg.SelfUpdate.Repo,
		CurrentVersion: version.Version,
		BinaryPath:     exePath,
		StateDir:       filepath.Join(cfg.State.Dir, "selfupdate"),
		Enabled:        cfg.SelfUpdate.Enabled,
	})
	handler, err := httpapi.NewHandler(httpapi.Options{
		Logger:         log,
		RequestTimeout: cfg.Server.RequestTimeout.Value(),
		MaxBodyBytes:   cfg.Server.MaxBodyBytes,
		TenantResolver: httpapi.StaticTenant(tenant.ID(cfg.Tenant.DefaultID)),
		Authenticator:  httpapi.NewTokenAuthenticator(adminToken),
		Store:          store,
		Engine:         engine,
		Evidence:       evStore,
		Updater:        updater,
		WebUI:          webui.Handler(),
		Broker:         broker,
		TenantID:       cfg.Tenant.DefaultID,
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
		if err := engine.Stop(shutdownCtx); err != nil {
			log.Warn("停止任务运行时超时", "error", err)
		}
		broker.Close()
	}

	log.Info("已停止")
	return 0
}

// loadOrCreateEvidenceKey 读取或生成证据加密密钥（32 字节，十六进制持久化）。
func loadOrCreateEvidenceKey(configuredHex, keyPath string) ([]byte, error) {
	if configuredHex != "" {
		return hex.DecodeString(configuredHex)
	}
	if raw, err := os.ReadFile(keyPath); err == nil {
		key, decErr := hex.DecodeString(strings.TrimSpace(string(raw)))
		if decErr != nil || len(key) != 32 {
			return nil, fmt.Errorf("证据密钥文件 %s 已损坏", keyPath)
		}
		return key, nil
	}
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		return nil, fmt.Errorf("生成证据密钥失败: %w", err)
	}
	if err := os.WriteFile(keyPath, []byte(hex.EncodeToString(key[:])), 0o600); err != nil {
		return nil, fmt.Errorf("写入证据密钥失败: %w", err)
	}
	return key[:], nil
}

// llmToolDefs 把工具定义转换为模型可识别的 function 定义。
func llmToolDefs(defs []tools.Definition) []llm.ToolDef {
	out := make([]llm.ToolDef, 0, len(defs))
	for _, d := range defs {
		out = append(out, llm.ToolDef{
			Name:        d.Name,
			Description: d.Description,
			Parameters:  d.Parameters,
		})
	}
	return out
}

// resolveAdminToken 返回管理接口令牌；未配置时生成临时令牌并打印到日志。
func resolveAdminToken(configured string, log *slog.Logger) (string, error) {
	if configured != "" {
		return configured, nil
	}
	var buf [24]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("生成临时管理令牌失败: %w", err)
	}
	token := hex.EncodeToString(buf[:])
	log.Warn("未配置 BEACONX_ADMIN_TOKEN，已生成临时管理令牌", "token", token)
	return token, nil
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
