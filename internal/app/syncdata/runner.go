package syncdata

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/orzl1/BeaconX/internal/domain/asset"
	"github.com/orzl1/BeaconX/internal/infra/pg"
)

// maxSyncItems 是单次同步最多处理的条目数（项目或资产）。
//
// 同步是对外拉取，上游数据量不可控；限制条数可以把一次运行的内存与写入
// 控制在可预期范围内，超过时让运行失败并提示改用分页/过滤。
const maxSyncItems = 5000

// maxReportedItemErrors 是运行统计里最多保留的逐条错误数。
const maxReportedItemErrors = 5

// Runner 执行一次数据源同步。
type Runner struct {
	store  *pg.Store
	cfg    Config
	logger *slog.Logger
}

// New 创建同步执行器；cfg 中的零值会被补齐为默认限制。
func New(store *pg.Store, cfg Config) *Runner {
	logger := slog.Default()
	return &Runner{store: store, cfg: cfg.normalized(), logger: logger}
}

// Start 异步执行一次同步。
//
// 请求返回后运行继续：用 context.WithoutCancel 切断请求取消的影响，再叠加
// RunTimeout 作为总时限。执行结果（成功/失败/统计/脱敏错误）写回 sync_runs，
// 调用方通过 GET /sync/runs 轮询。
func (r *Runner) Start(ctx context.Context, tenantID int64, run pg.SyncRun) {
	go func() {
		execCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.cfg.RunTimeout)
		defer cancel()
		if err := r.Execute(execCtx, tenantID, run); err != nil {
			r.logger.Warn("同步运行失败",
				"tenant", tenantID, "run", run.ID, "source", run.SourceID, "error", err)
		}
	}()
}

// Execute 同步执行一次同步，并把结果写回运行记录（供测试与 Start 复用）。
//
// 返回的 error 只表示「运行记录写回失败」；同步本身失败会体现为运行记录的
// status=failed + error，不通过返回值上抛。
func (r *Runner) Execute(ctx context.Context, tenantID int64, run pg.SyncRun) error {
	source, err := r.store.GetSyncSource(ctx, tenantID, run.SourceID)
	stats := map[string]any{"mode": "", "projects": 0, "assets": 0, "skipped": 0, "imported": 0}
	if err != nil {
		return r.store.FinishSyncRun(ctx, tenantID, run.ID, pg.SyncRunFailed, stats, redactMessage("读取数据源失败: "+err.Error()))
	}
	stats["mode"] = source.Type

	var syncErr error
	switch source.Type {
	case pg.SyncSourceManual:
		syncErr = r.runManual(ctx, tenantID, source, run, stats)
	case pg.SyncSourceHTTPJSON:
		syncErr = r.runHTTPJSON(ctx, tenantID, source, run, stats)
	default:
		syncErr = fmt.Errorf("不支持的数据源类型 %q", source.Type)
	}

	status := pg.SyncRunSucceeded
	errText := ""
	if syncErr != nil {
		status = pg.SyncRunFailed
		errText = redactMessage(syncErr.Error())
	}
	return r.store.FinishSyncRun(ctx, tenantID, run.ID, status, stats, errText)
}

// runManual 统计 manual 数据源已有项目（不发起外部调用）。
func (r *Runner) runManual(ctx context.Context, tenantID int64, source pg.SyncSource, run pg.SyncRun, stats map[string]any) error {
	if !source.Enabled {
		return fmt.Errorf("数据源 %q 已停用", source.Name)
	}
	if run.ProjectID != nil {
		project, err := r.store.GetSyncProject(ctx, tenantID, *run.ProjectID)
		if err != nil {
			return fmt.Errorf("读取外部项目失败: %w", err)
		}
		stats["projects"] = 1
		stats["imported"] = 1
		stats["project"] = project.ExternalID
		return nil
	}
	count, err := r.store.CountSyncProjects(ctx, tenantID, source.ID)
	if err != nil {
		return fmt.Errorf("统计外部项目失败: %w", err)
	}
	stats["projects"] = count
	stats["imported"] = count
	return nil
}

// runHTTPJSON 拉取 http_json 数据源并导入 projects 或 assets。
func (r *Runner) runHTTPJSON(ctx context.Context, tenantID int64, source pg.SyncSource, run pg.SyncRun, stats map[string]any) error {
	if !source.Enabled {
		return fmt.Errorf("数据源 %q 已停用", source.Name)
	}
	rawURL, _ := source.Config["url"].(string)
	if strings.TrimSpace(rawURL) == "" {
		return fmt.Errorf("数据源缺少 config.url")
	}
	// 统计里只放 host（不含 userinfo/查询串），便于排障又不泄漏凭据。
	stats["source_host"] = redactedHost(rawURL)
	headers := headerMap(source.Config)
	body, err := r.fetch(ctx, rawURL, headers)
	if err != nil {
		return err
	}

	var probe map[string]json.RawMessage
	if err := json.Unmarshal(body, &probe); err != nil {
		return fmt.Errorf("上游响应不是合法 JSON")
	}
	projectsRaw, hasProjects := probe["projects"]
	assetsRaw, hasAssets := probe["assets"]
	if !hasProjects && !hasAssets {
		return fmt.Errorf("上游响应缺少 projects 或 assets 字段")
	}

	// 一次响应只处理一种载荷：同时出现时以 projects 为准并记入统计。
	if hasProjects && len(strings.TrimSpace(string(projectsRaw))) > 2 {
		return r.importProjects(ctx, tenantID, source, run, projectsRaw, stats)
	}
	if hasAssets && len(strings.TrimSpace(string(assetsRaw))) > 2 {
		return r.importAssets(ctx, tenantID, source, assetsRaw, stats)
	}
	// 空数组是合法的「上游暂无数据」，运行成功、统计为零。
	return nil
}

// fetch 发起一次受 SSRF 防护的 GET，返回受 5MB 限制的响应体。
func (r *Runner) fetch(ctx context.Context, rawURL string, headers map[string]string) ([]byte, error) {
	// 先做一次解析与地址校验：host 不可解析时立刻失败，不发请求。
	if _, err := validateTarget(ctx, rawURL, r.cfg.AllowPrivate); err != nil {
		return nil, err
	}
	client := newHTTPClient(r.cfg.AllowPrivate, r.cfg.Timeout)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("构造同步请求失败")
	}
	req.Header.Set("Accept", "application/json")
	for key, value := range headers {
		req.Header.Set(key, value)
	}

	resp, err := client.Do(req)
	if err != nil {
		// 网络错误文本里可能带完整 URL（含 userinfo），统一脱敏。
		return nil, fmt.Errorf("请求上游失败: %s", redactMessage(err.Error()))
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("上游返回 HTTP %d", resp.StatusCode)
	}
	limited := io.LimitReader(resp.Body, r.cfg.MaxBodyBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("读取上游响应失败")
	}
	if int64(len(body)) > r.cfg.MaxBodyBytes {
		return nil, fmt.Errorf("上游响应超过 %d 字节限制", r.cfg.MaxBodyBytes)
	}
	return body, nil
}

// importProjects 解析并 upsert 外部项目。
func (r *Runner) importProjects(ctx context.Context, tenantID int64, source pg.SyncSource, run pg.SyncRun, raw json.RawMessage, stats map[string]any) error {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return fmt.Errorf("上游 projects 不是数组")
	}
	if len(items) > maxSyncItems {
		return fmt.Errorf("上游返回 %d 个项目，超过单次同步上限 %d", len(items), maxSyncItems)
	}

	var scopedExternalID string
	if run.ProjectID != nil {
		project, err := r.store.GetSyncProject(ctx, tenantID, *run.ProjectID)
		if err != nil {
			return fmt.Errorf("读取外部项目失败: %w", err)
		}
		scopedExternalID = project.ExternalID
	}

	imported, skipped := 0, 0
	itemErrors := make([]string, 0, maxReportedItemErrors)
	foundScoped := false
	for _, item := range items {
		externalID, name, data, err := parseProjectItem(item)
		if err != nil {
			skipped++
			if len(itemErrors) < maxReportedItemErrors {
				itemErrors = append(itemErrors, err.Error())
			}
			continue
		}
		if scopedExternalID != "" {
			if externalID != scopedExternalID {
				continue
			}
			foundScoped = true
		}
		if _, err := r.store.UpsertSyncProject(ctx, tenantID, pg.SyncProjectInput{
			SourceID:   source.ID,
			ExternalID: externalID,
			Name:       name,
			Data:       data,
		}); err != nil {
			skipped++
			if len(itemErrors) < maxReportedItemErrors {
				itemErrors = append(itemErrors, redactMessage(err.Error()))
			}
			continue
		}
		imported++
	}
	if scopedExternalID != "" && !foundScoped {
		return fmt.Errorf("上游没有返回 external_id=%q 的项目", scopedExternalID)
	}

	stats["projects"] = imported
	stats["imported"] = imported
	stats["skipped"] = skipped
	if len(itemErrors) > 0 {
		stats["errors"] = itemErrors
	}
	return nil
}

// importAssets 解析并 upsert 资产（复用资产 Store 的身份归一化与 upsert 语义）。
func (r *Runner) importAssets(ctx context.Context, tenantID int64, source pg.SyncSource, raw json.RawMessage, stats map[string]any) error {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return fmt.Errorf("上游 assets 不是数组")
	}
	if len(items) > maxSyncItems {
		return fmt.Errorf("上游返回 %d 个资产，超过单次同步上限 %d", len(items), maxSyncItems)
	}

	imported, skipped := 0, 0
	itemErrors := make([]string, 0, maxReportedItemErrors)
	for _, item := range items {
		input, err := parseAssetItem(item)
		if err != nil {
			skipped++
			if len(itemErrors) < maxReportedItemErrors {
				itemErrors = append(itemErrors, err.Error())
			}
			continue
		}
		if _, err := r.store.UpsertAsset(ctx, tenantID, input); err != nil {
			skipped++
			if len(itemErrors) < maxReportedItemErrors {
				itemErrors = append(itemErrors, redactMessage(err.Error()))
			}
			continue
		}
		imported++
	}
	stats["assets"] = imported
	stats["imported"] = imported
	stats["skipped"] = skipped
	if len(itemErrors) > 0 {
		stats["errors"] = itemErrors
	}
	return nil
}

// headerMap 取出 config.headers（字符串映射）。
func headerMap(config map[string]any) map[string]string {
	out := map[string]string{}
	raw, ok := config["headers"].(map[string]any)
	if !ok {
		return out
	}
	for key, value := range raw {
		if text, ok := value.(string); ok && strings.TrimSpace(key) != "" {
			out[key] = text
		}
	}
	return out
}

// parseProjectItem 解析一条外部项目：external_id（或 id）必填，name 可选。
func parseProjectItem(raw json.RawMessage) (externalID, name string, data map[string]any, err error) {
	if err := json.Unmarshal(raw, &data); err != nil || data == nil {
		return "", "", nil, fmt.Errorf("项目不是 JSON 对象")
	}
	externalID = stringValue(data["external_id"])
	if externalID == "" {
		externalID = stringValue(data["id"])
	}
	if externalID == "" {
		return "", "", nil, fmt.Errorf("项目缺少 external_id/id")
	}
	name = stringValue(data["name"])
	if name == "" {
		name = stringValue(data["title"])
	}
	return externalID, name, data, nil
}

// parseAssetItem 解析一条外部资产。
//
// 支持两种形状：
//   - {"identities":[{"kind":"subdomain","key":"api.example.com"}], "label":"…"}；
//   - {"type":"root_domain","value":"example.com"}（label 缺省用 value）。
//
// 身份键统一走 domain/asset.Normalize，因此上游写 "Example.COM." 或带默认端口的
// URL 都会被收敛成与平台内部一致的键。
func parseAssetItem(raw json.RawMessage) (pg.AssetInput, error) {
	var item map[string]any
	if err := json.Unmarshal(raw, &item); err != nil || item == nil {
		return pg.AssetInput{}, fmt.Errorf("资产不是 JSON 对象")
	}
	label := stringValue(item["label"])

	var identities []asset.Identity
	if rawIdentities, ok := item["identities"].([]any); ok && len(rawIdentities) > 0 {
		for _, entry := range rawIdentities {
			mapping, ok := entry.(map[string]any)
			if !ok {
				return pg.AssetInput{}, fmt.Errorf("资产 identities 元素不是对象")
			}
			kind := asset.Kind(strings.TrimSpace(stringValue(mapping["kind"])))
			if !kind.Valid() {
				return pg.AssetInput{}, fmt.Errorf("资产身份类型非法 %q", stringValue(mapping["kind"]))
			}
			normalized, err := asset.Normalize(kind, stringValue(mapping["key"]))
			if err != nil {
				return pg.AssetInput{}, fmt.Errorf("资产身份不合法: %v", err)
			}
			identities = append(identities, normalized)
		}
	}

	if len(identities) == 0 {
		kind := asset.Kind(strings.TrimSpace(stringValue(item["type"])))
		if !kind.Valid() {
			return pg.AssetInput{}, fmt.Errorf("资产类型非法或缺失 %q", stringValue(item["type"]))
		}
		value := stringValue(item["value"])
		if value == "" {
			value = label
		}
		if value == "" {
			return pg.AssetInput{}, fmt.Errorf("资产缺少 value/label")
		}
		normalized, err := asset.Normalize(kind, value)
		if err != nil {
			return pg.AssetInput{}, fmt.Errorf("资产值不合法: %v", err)
		}
		identities = append(identities, normalized)
		if label == "" {
			label = value
		}
	}
	if label == "" {
		label = identities[0].Key
	}
	attrs, _ := item["attrs"].(map[string]any)
	return pg.AssetInput{
		Type:       identities[0].Kind,
		Label:      label,
		Attrs:      attrs,
		Identities: identities,
	}, nil
}

// stringValue 把上游 JSON 里的标量转成字符串（数字用无小数形式）。
func stringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case float64:
		if typed == float64(int64(typed)) {
			return strconv.FormatInt(int64(typed), 10)
		}
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case json.Number:
		return typed.String()
	}
	return ""
}

// redactedHost 返回不含凭据的 host，供统计信息使用。
//
// 统计会长期保存，只放 host（不含 userinfo / 查询串）既够排障又不泄漏凭据。
func redactedHost(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return parsed.Host
}
