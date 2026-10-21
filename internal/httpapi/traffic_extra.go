package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/orzl1/BeaconX/internal/app/evidence"
	"github.com/orzl1/BeaconX/internal/infra/pg"
)

// B5：流量导出（HAR）、重放与任务级清理（P-B5-41..43）。
//
// 路由通过 RegisterFeatureRoutes 在 init() 注册，NewHandler 构造时用 Build(Options)
// 注入 Store/Evidence/TenantID。
//
// 安全边界：
//   - 导出/重放都需要 Bearer 鉴权（统一中间件）；
//   - 重放**必须经过 executor**（策略门 + 审批 + 审计 + 采集），Options.Executor
//     未接线时返回 503，绝不静默降级为直接调用工具；仅允许同租户、tool=http_probe、
//     请求参数为合法 JSON 的采集记录；
//   - 重放产生的采集行由 executor 的 Evidence 配置负责落库；这里只做 best-effort
//     查询最新 traffic_id 返回给调用方。

const (
	// harSizeLimit 是 HAR 导出允许的最大请求+响应正文字节数（含解码后）。
	harSizeLimit = 20 << 20
	// replayTool 是当前唯一支持重放的工具。
	replayTool = "http_probe"
)

func init() {
	RegisterFeatureRoutes(
		FeatureRoute{
			Method: http.MethodGet, Pattern: "/api/v1/traffic/{id}/export",
			Summary: "导出流量为 HAR 1.2（?format=har）",
			Build:   func(o Options) http.HandlerFunc { return o.handleTrafficExportHAR },
		},
		FeatureRoute{
			Method: http.MethodPost, Pattern: "/api/v1/traffic/{id}/replay",
			Summary: "重放 http_probe 采集记录（生成新采集行）",
			Build:   func(o Options) http.HandlerFunc { return o.handleTrafficReplay },
		},
		FeatureRoute{
			Method: http.MethodDelete, Pattern: "/api/v1/missions/{id}/traffic",
			Summary: "清理任务的全部流量采集行（证据快照保留）",
			Build:   func(o Options) http.HandlerFunc { return o.handleDeleteMissionTraffic },
		},
	)
}

// ---------- HAR 导出 ----------

// HAR 1.2 最小结构（只保留可复现所需字段；body 以文本形式内联）。
type harFile struct {
	Log harLog `json:"log"`
}

type harLog struct {
	Version string     `json:"version"`
	Creator harCreator `json:"creator"`
	Entries []harEntry `json:"entries"`
}

type harCreator struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type harEntry struct {
	StartedDateTime string      `json:"startedDateTime"`
	Time            float64     `json:"time"`
	Request         harRequest  `json:"request"`
	Response        harResponse `json:"response"`
	Cache           struct{}    `json:"cache"`
	Timings         harTimings  `json:"timings"`
}

type harNameValue struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type harRequest struct {
	Method      string         `json:"method"`
	URL         string         `json:"url"`
	HTTPVersion string         `json:"httpVersion"`
	Headers     []harNameValue `json:"headers"`
	QueryString []harNameValue `json:"queryString"`
	Cookies     []harNameValue `json:"cookies"`
	HeadersSize int            `json:"headersSize"`
	BodySize    int64          `json:"bodySize"`
	PostData    *harPostData   `json:"postData,omitempty"`
}

type harPostData struct {
	MimeType string `json:"mimeType"`
	Text     string `json:"text"`
}

type harResponse struct {
	Status      int            `json:"status"`
	StatusText  string         `json:"statusText"`
	HTTPVersion string         `json:"httpVersion"`
	Headers     []harNameValue `json:"headers"`
	Cookies     []harNameValue `json:"cookies"`
	Content     harContent     `json:"content"`
	RedirectURL string         `json:"redirectURL"`
	HeadersSize int            `json:"headersSize"`
	BodySize    int64          `json:"bodySize"`
}

type harContent struct {
	Size     int64  `json:"size"`
	MimeType string `json:"mimeType"`
	Text     string `json:"text,omitempty"`
}

type harTimings struct {
	Send    float64 `json:"send"`
	Wait    float64 `json:"wait"`
	Receive float64 `json:"receive"`
}

// httpProbeArgs 是重放/HAR 解析所需的 http_probe 参数子集。
type httpProbeArgs struct {
	URL            string            `json:"url"`
	Method         string            `json:"method"`
	Headers        map[string]string `json:"headers"`
	Body           string            `json:"body"`
	TimeoutSeconds int               `json:"timeout_seconds"`
}

// handleTrafficExportHAR 把一条采集记录导出为 HAR 1.2。
//
// 错误语义：format 非 har → 400；记录不存在 → 404；Evidence 未装配 → 503；
// 正文缺失/被篡改/密钥不匹配 → 409；超过 20MB → 413。
func (o Options) handleTrafficExportHAR(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	if o.Evidence == nil {
		writeError(w, r, http.StatusServiceUnavailable, "evidence_unavailable", "证据存储未就绪")
		return
	}
	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	if format == "" {
		format = "har"
	}
	if format != "har" {
		writeError(w, r, http.StatusBadRequest, "bad_request", "format 只支持 har")
		return
	}
	id, err := pathInt64Param(r, "id", "流量记录")
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	record, err := o.Store.GetTraffic(r.Context(), o.TenantID, id)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	if record.RequestSize+record.ResponseSize > harSizeLimit {
		writeError(w, r, http.StatusRequestEntityTooLarge, "payload_too_large", "流量正文超过 20MB，无法导出 HAR")
		return
	}

	reqBody, err := o.readTrafficBlob(r, record.RequestHash, record.RequestSize, "请求")
	if err != nil {
		writeTrafficBlobError(w, r, err)
		return
	}
	respBody, err := o.readTrafficBlob(r, record.ResponseHash, record.ResponseSize, "响应")
	if err != nil {
		writeTrafficBlobError(w, r, err)
		return
	}

	var args httpProbeArgs
	if len(reqBody) > 0 {
		if err := json.Unmarshal(reqBody, &args); err != nil {
			writeError(w, r, http.StatusConflict, "conflict", "请求正文不是可解析的 http_probe 参数 JSON")
			return
		}
	}
	out, err := buildHAR(record, args, reqBody, respBody)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal_error", "生成 HAR 失败")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"traffic-%d.har\"", record.ID))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// readTrafficBlob 读取并解密一条采集记录的正文；空哈希视为空正文。
func (o Options) readTrafficBlob(r *http.Request, hash string, size int64, label string) ([]byte, error) {
	if hash == "" {
		return nil, nil
	}
	plain, err := o.Evidence.GetBlob(r.Context(), evidence.BlobRef{Hash: hash, Size: size})
	if err != nil {
		if errors.Is(err, evidence.ErrTampered) {
			return nil, fmt.Errorf("%s正文校验失败：%w", label, evidence.ErrTampered)
		}
		return nil, err
	}
	return plain, nil
}

// writeTrafficBlobError 把正文读取错误映射为 HTTP 状态。
func writeTrafficBlobError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, evidence.ErrTampered) {
		writeError(w, r, http.StatusConflict, "tampered", "流量正文校验失败：文件缺失、被篡改或密钥不匹配")
		return
	}
	writeStoreError(w, r, err)
}

// buildHAR 把采集记录渲染成 HAR 结构。
func buildHAR(record pg.TrafficExchange, args httpProbeArgs, reqBody, respBody []byte) ([]byte, error) {
	method := strings.ToUpper(strings.TrimSpace(args.Method))
	if method == "" {
		method = http.MethodGet
	}
	headers := make([]harNameValue, 0, len(args.Headers))
	keys := make([]string, 0, len(args.Headers))
	for key := range args.Headers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		headers = append(headers, harNameValue{Name: key, Value: args.Headers[key]})
	}

	query := make([]harNameValue, 0, 4)
	if parsed, err := url.Parse(strings.TrimSpace(args.URL)); err == nil {
		for key, values := range parsed.Query() {
			for _, value := range values {
				query = append(query, harNameValue{Name: key, Value: value})
			}
		}
	}
	sort.Slice(query, func(i, j int) bool { return query[i].Name < query[j].Name })

	status := 0
	if record.StatusCode != nil {
		status = *record.StatusCode
	}
	respHeaders, respContentType, respText := parseToolOutput(string(respBody))
	content := harContent{Size: record.ResponseSize, MimeType: respContentType}
	if status >= 0 && respText != "" {
		content.Text = respText
	}

	entry := harEntry{
		StartedDateTime: record.CreatedAt.UTC().Format(time.RFC3339Nano),
		Request: harRequest{
			Method: method, URL: args.URL, HTTPVersion: "HTTP/1.1",
			Headers: headers, QueryString: query, Cookies: []harNameValue{},
			HeadersSize: -1, BodySize: record.RequestSize,
		},
		Response: harResponse{
			Status: status, HTTPVersion: "HTTP/1.1",
			Headers: respHeaders, Cookies: []harNameValue{},
			Content: content, HeadersSize: -1, BodySize: record.ResponseSize,
		},
		Timings: harTimings{},
	}
	if args.Body != "" {
		entry.Request.PostData = &harPostData{MimeType: "text/plain; charset=utf-8", Text: args.Body}
	}
	// reqBody 只参与“请求是否可解析”的判断；HAR 的 body 用 postData 表达。
	_ = reqBody
	payload := harFile{Log: harLog{
		Version: "1.2",
		Creator: harCreator{Name: "BeaconX", Version: "1.0"},
		Entries: []harEntry{entry},
	}}
	return json.Marshal(payload)
}

// parseToolOutput 从 http_probe 的工具输出（"HTTP 200\nKey: Value\n\nbody"）中
// 拆出响应头、Content-Type 与正文片段。
func parseToolOutput(output string) ([]harNameValue, string, string) {
	headers := make([]harNameValue, 0, 4)
	contentType := "text/plain; charset=utf-8"
	head, body, ok := strings.Cut(output, "\n\n")
	if !ok {
		return headers, contentType, strings.TrimSpace(output)
	}
	lines := strings.Split(head, "\n")
	for _, line := range lines[1:] { // 第一行是 "HTTP <code>"
		name, value, found := strings.Cut(line, ": ")
		if !found {
			continue
		}
		headers = append(headers, harNameValue{Name: name, Value: value})
		if strings.EqualFold(name, "Content-Type") {
			contentType = value
		}
	}
	return headers, contentType, body
}

// ---------- 重放 ----------

// handleTrafficReplay 重放一条 http_probe 采集记录，经 executor 策略门执行。
//
// 返回 {policy_gated:true, output, traffic_id?, status?}；若 Options.Executor 未接线
// 则 503（不允许绕过策略门直接调用工具）。执行被拒绝/待审批按 executor 语义映射：
// 策略 deny → 403；人工拒绝 → 403；审批超时 → 409；其它执行失败 → 502。
func (o Options) handleTrafficReplay(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	if o.Executor == nil {
		writeError(w, r, http.StatusServiceUnavailable, "executor_unavailable", "执行器未接线，禁止绕过策略门重放")
		return
	}
	if o.Evidence == nil {
		writeError(w, r, http.StatusServiceUnavailable, "evidence_unavailable", "证据存储未就绪")
		return
	}
	id, err := pathInt64Param(r, "id", "流量记录")
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	record, err := o.Store.GetTraffic(r.Context(), o.TenantID, id)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	if record.Tool != replayTool {
		writeError(w, r, http.StatusConflict, "conflict", "只有 http_probe 采集记录支持重放")
		return
	}
	if record.RequestHash == "" {
		writeError(w, r, http.StatusConflict, "conflict", "采集记录没有请求正文，无法重放")
		return
	}
	reqBody, err := o.readTrafficBlob(r, record.RequestHash, record.RequestSize, "请求")
	if err != nil {
		writeTrafficBlobError(w, r, err)
		return
	}
	var args httpProbeArgs
	if err := json.Unmarshal(reqBody, &args); err != nil || strings.TrimSpace(args.URL) == "" {
		writeError(w, r, http.StatusConflict, "conflict", "请求正文不是可重放的 http_probe 参数 JSON（缺少 url）")
		return
	}

	nodeID := int64(0)
	if record.NodeID != nil {
		nodeID = *record.NodeID
	}
	result, err := o.Executor.Execute(r.Context(), record.MissionID, nodeID, replayTool, reqBody)
	if err != nil {
		writeReplayExecutorError(w, r, err)
		return
	}
	status := 0
	if v, ok := result.Meta["status"]; ok {
		if code, ok := v.(int); ok {
			status = code
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"policy_gated": true,
		"traffic_id":   o.latestReplayTrafficID(r.Context(), record.MissionID),
		"output":       result.Output,
		"status":       status,
	})
}

// writeReplayExecutorError 把 executor 的执行错误映射成 HTTP 语义。
//
// executor 当前以普通 error 文本表达裁决（策略/审批层没有导出哨兵错误），
// 这里按稳定的中文前缀映射；后续 executor 提供 typed error 后应替换为 errors.Is。
func writeReplayExecutorError(w http.ResponseWriter, r *http.Request, err error) {
	message := err.Error()
	switch {
	case strings.Contains(message, "策略拒绝执行"):
		writeError(w, r, http.StatusForbidden, "policy_denied", message)
	case strings.Contains(message, "未获人工批准"):
		writeError(w, r, http.StatusForbidden, "approval_rejected", message)
	case strings.Contains(message, "审批超时"):
		writeError(w, r, http.StatusConflict, "approval_timeout", message)
	case strings.Contains(message, "未知工具"):
		writeError(w, r, http.StatusBadGateway, "executor_unavailable", "执行器未注册 http_probe 工具")
	default:
		writeError(w, r, http.StatusBadGateway, "replay_failed", "重放失败："+message)
	}
}

// latestReplayTrafficID 返回任务最近一条 http_probe 采集行 ID。
//
// executor 在自己的采集流程里写 traffic_exchanges；这里只做 best-effort 查询，
// 查不到时返回 0（响应里的 traffic_id 是提示字段，不作为执行成功依据）。
func (o Options) latestReplayTrafficID(ctx context.Context, missionID int64) int64 {
	list, err := o.Store.ListTrafficPage(ctx, o.TenantID, pg.TrafficPageFilter{
		MissionID: missionID,
		Tool:      replayTool,
		Limit:     1,
	})
	if err != nil || len(list) == 0 {
		return 0
	}
	return list[0].ID
}

// ---------- 任务级流量清理 ----------

// handleDeleteMissionTraffic 删除任务的全部采集行（证据快照与正文保留）。
//
// 返回 204，并在 X-Deleted-Count 头返回删除数量（204 不带正文）。
func (o Options) handleDeleteMissionTraffic(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	missionID, ok := o.missionPathID(w, r)
	if !ok {
		return
	}
	deleted, err := o.Store.DeleteMissionTraffic(r.Context(), o.TenantID, missionID)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	w.Header().Set("X-Deleted-Count", fmt.Sprintf("%d", deleted))
	w.WriteHeader(http.StatusNoContent)
}
