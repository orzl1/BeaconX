package httpapi

import (
	"net/http"
)

// M6：自更新接口。

func (o Options) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	if o.Updater == nil {
		writeError(w, r, http.StatusServiceUnavailable, "update_unavailable", "自更新未配置")
		return
	}
	rel, has, err := o.Updater.Check(r.Context())
	if err != nil {
		writeError(w, r, http.StatusBadGateway, "update_check_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"has_update": has,
		"release":    rel,
	})
}

func (o Options) handleUpdateApply(w http.ResponseWriter, r *http.Request) {
	if o.Updater == nil {
		writeError(w, r, http.StatusServiceUnavailable, "update_unavailable", "自更新未配置")
		return
	}
	rel, has, err := o.Updater.Check(r.Context())
	if err != nil {
		writeError(w, r, http.StatusBadGateway, "update_check_failed", err.Error())
		return
	}
	if !has {
		writeJSON(w, http.StatusOK, map[string]any{"updated": false, "version": rel.Version, "message": "当前已是最新版本"})
		return
	}
	if err := o.Updater.Apply(r.Context(), rel); err != nil {
		writeError(w, r, http.StatusConflict, "update_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"updated": true, "version": rel.Version, "message": "更新完成，需重启生效"})
}

func (o Options) handleUpdateRollback(w http.ResponseWriter, r *http.Request) {
	if o.Updater == nil {
		writeError(w, r, http.StatusServiceUnavailable, "update_unavailable", "自更新未配置")
		return
	}
	if err := o.Updater.Rollback(); err != nil {
		writeError(w, r, http.StatusConflict, "rollback_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rolled_back": true, "message": "已回滚，需重启生效"})
}
