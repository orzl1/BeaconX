package httpapi

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/orzl1/BeaconX/internal/infra/pg"
)

// B6：全局活动流（GET /api/v1/activity）。
//
// 数据来源是 mission_events（任务事件流的事实表），跨任务按 id 倒序返回，
// 支持 before_id 游标、mission_id 与 type 过滤。路由通过 init() 的
// RegisterFeatureRoutes + Build 注册，不修改 handler.go。

func init() {
	RegisterFeatureRoutes(
		FeatureRoute{
			Method:  http.MethodGet,
			Pattern: "/api/v1/activity",
			Summary: "全局活动流（?limit=&before_id=&mission_id=&type=）",
			Build:   func(o Options) http.HandlerFunc { return o.handleListActivity },
		},
	)
}

// activityItem 是一条活动流记录。
//
// 只暴露前端需要的字段：payload 为空时不输出（避免每条都带一个 {}），
// actor/reason 属于审计细节，需要时走任务事件接口（SSE / 任务事件）。
type activityItem struct {
	ID        int64          `json:"id"`
	MissionID int64          `json:"mission_id"`
	Type      string         `json:"type"`
	Payload   map[string]any `json:"payload,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
}

// handleListActivity 跨任务列出活动事件（id 倒序）。
//
// 过滤不存在的任务返回空数组而不是 404：活动流是「观察面」，按一个已删除的
// 任务过滤只是没有数据，不应让整页报错。
func (o Options) handleListActivity(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	limit, ok := parseStrictLimit(w, r, defaultHTTPLimit, maxHTTPLimit)
	if !ok {
		return
	}
	missionID, ok := parseOptionalQueryID(w, r, "mission_id", "mission_id")
	if !ok {
		return
	}
	var beforeID int64
	if raw := strings.TrimSpace(r.URL.Query().Get("before_id")); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value < 0 {
			writeError(w, r, http.StatusBadRequest, "bad_request", "before_id 必须为非负整数")
			return
		}
		beforeID = value
	}
	eventType := strings.TrimSpace(r.URL.Query().Get("type"))
	if len(eventType) > 64 {
		writeError(w, r, http.StatusBadRequest, "bad_request", "type 长度不能超过 64 字节")
		return
	}

	events, err := o.Store.ListMissionEventsGlobal(r.Context(), o.TenantID, pg.GlobalEventFilter{
		MissionID: missionID,
		Type:      eventType,
		BeforeID:  beforeID,
		Limit:     limit,
	})
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	items := make([]activityItem, 0, len(events))
	for _, event := range events {
		item := activityItem{
			ID:        event.ID,
			MissionID: event.MissionID,
			Type:      event.Type,
			CreatedAt: event.CreatedAt,
		}
		if len(event.Payload) > 0 {
			item.Payload = event.Payload
		}
		items = append(items, item)
	}
	// 本页取满才继续翻页；next_before_id 即最后一条（最小）id。
	var nextBeforeID int64
	if len(items) >= limit && len(items) > 0 {
		nextBeforeID = items[len(items)-1].ID
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"activity":       items,
		"next_before_id": nextBeforeID,
	})
}
