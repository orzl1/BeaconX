package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/orzl1/BeaconX/internal/app/events"
	"github.com/orzl1/BeaconX/internal/app/runner"
	"github.com/orzl1/BeaconX/internal/domain/task"
	"github.com/orzl1/BeaconX/internal/infra/pg"
)

// M2 任务接口：创建、查询、暂停/恢复/取消/删除与 SSE 事件流。

type missionCreateRequest struct {
	Name        string `json:"name"`
	Goal        string `json:"goal"`
	Description string `json:"description"`
}

func (o Options) missionReady(w http.ResponseWriter, r *http.Request) bool {
	if o.Store == nil || o.Engine == nil {
		writeError(w, r, http.StatusServiceUnavailable, "runtime_unavailable", "任务运行时未就绪")
		return false
	}
	return true
}

func missionIDFrom(r *http.Request) (int64, error) {
	raw := strings.TrimSpace(r.PathValue("id"))
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, pgx.ErrNoRows
	}
	return id, nil
}

func writeMissionError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeError(w, r, http.StatusNotFound, "not_found", "任务不存在")
	case errors.Is(err, runner.ErrNotRunning):
		writeError(w, r, http.StatusConflict, "conflict", "任务当前状态不允许该操作")
	case errors.Is(err, pg.ErrInvalidInput):
		writeError(w, r, http.StatusBadRequest, "bad_request", err.Error())
	default:
		writeError(w, r, http.StatusInternalServerError, "internal_error", "任务操作失败")
	}
}

func (o Options) handleCreateMission(w http.ResponseWriter, r *http.Request) {
	if !o.missionReady(w, r) {
		return
	}
	var req missionCreateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, "bad_request", "请求体不是合法 JSON")
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeError(w, r, http.StatusBadRequest, "bad_request", "任务名称不能为空")
		return
	}
	mission, err := o.Store.CreateMission(r.Context(), o.TenantID, pg.MissionInput{
		Name:        strings.TrimSpace(req.Name),
		Goal:        req.Goal,
		Description: req.Description,
	})
	if err != nil {
		writeMissionError(w, r, err)
		return
	}
	ev, err := o.Store.AppendMissionEvent(r.Context(), o.TenantID, mission.ID, task.EvCreated, "user", "任务创建", nil)
	if err == nil {
		o.publishMissionEvent(ev)
	}
	writeJSON(w, http.StatusCreated, mission)
}

func (o Options) handleListMissions(w http.ResponseWriter, r *http.Request) {
	if !o.missionReady(w, r) {
		return
	}
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 200 {
		limit = 200
	}
	missions, err := o.Store.ListMissions(r.Context(), o.TenantID, status, limit)
	if err != nil {
		writeMissionError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"missions": missions})
}

func (o Options) handleGetMission(w http.ResponseWriter, r *http.Request) {
	if !o.missionReady(w, r) {
		return
	}
	id, err := missionIDFrom(r)
	if err != nil {
		writeMissionError(w, r, err)
		return
	}
	mission, err := o.Store.GetMission(r.Context(), o.TenantID, id)
	if err != nil {
		writeMissionError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, mission)
}

func (o Options) handlePauseMission(w http.ResponseWriter, r *http.Request) {
	o.missionAction(w, r, o.Engine.Pause)
}

func (o Options) handleResumeMission(w http.ResponseWriter, r *http.Request) {
	o.missionAction(w, r, o.Engine.Resume)
}

func (o Options) handleCancelMission(w http.ResponseWriter, r *http.Request) {
	o.missionAction(w, r, o.Engine.Cancel)
}

func (o Options) missionAction(w http.ResponseWriter, r *http.Request, action func(context.Context, int64) error) {
	if !o.missionReady(w, r) {
		return
	}
	id, err := missionIDFrom(r)
	if err != nil {
		writeMissionError(w, r, err)
		return
	}
	if err := action(r.Context(), id); err != nil {
		writeMissionError(w, r, err)
		return
	}
	mission, err := o.Store.GetMission(r.Context(), o.TenantID, id)
	if err != nil {
		writeMissionError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, mission)
}

func (o Options) handleDeleteMission(w http.ResponseWriter, r *http.Request) {
	if !o.missionReady(w, r) {
		return
	}
	id, err := missionIDFrom(r)
	if err != nil {
		writeMissionError(w, r, err)
		return
	}
	if err := o.Engine.Delete(r.Context(), id); err != nil {
		writeMissionError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (o Options) handleMissionEvents(w http.ResponseWriter, r *http.Request) {
	if o.Store == nil || o.Broker == nil {
		writeError(w, r, http.StatusServiceUnavailable, "sse_unavailable", "事件流未就绪")
		return
	}
	missionID, err := missionIDFrom(r)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "bad_request", "任务标识不合法")
		return
	}
	if _, err := o.Store.GetMission(r.Context(), o.TenantID, missionID); err != nil {
		writeMissionError(w, r, err)
		return
	}

	sse := SSEOptions{
		MissionID: func(*http.Request) (int64, error) { return missionID, nil },
		Replay: func(ctx context.Context, id, afterID int64, limit int) ([]events.Event, error) {
			rows, err := o.Store.ListMissionEvents(ctx, o.TenantID, id, afterID, limit)
			if err != nil {
				return nil, err
			}
			out := make([]events.Event, 0, len(rows))
			for _, ev := range rows {
				out = append(out, missionEventToSSE(ev))
			}
			return out, nil
		},
		Subscribe: o.Broker.Subscribe,
	}
	sse.Handler()(w, r)
}

// publishMissionEvent 把任务事件投递到 SSE 总线；payload 以 JSON 形式下发。
func (o Options) publishMissionEvent(ev pg.MissionEvent) {
	if o.Broker == nil {
		return
	}
	o.Broker.Publish(missionEventToSSE(ev))
}

func missionEventToSSE(ev pg.MissionEvent) events.Event {
	var payload json.RawMessage
	if ev.Payload != nil {
		if raw, err := json.Marshal(ev.Payload); err == nil {
			payload = raw
		}
	}
	return events.Event{
		ID:        ev.ID,
		MissionID: ev.MissionID,
		Type:      ev.Type,
		Payload:   payload,
		CreatedAt: ev.CreatedAt,
	}
}
