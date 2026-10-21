// Package webui 通过 go:embed 提供前端静态资源。
//
// 真实前端由 web/ 构建并复制到 internal/webui/dist（见 Makefile 的 web 目标与
// 发布流水线）。仓库内保留一个占位 index.html，保证未构建前端时也能启动并给出提示。
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:dist
var files embed.FS

// Handler 返回前端处理器：
//   - 已存在的静态文件按原样返回；
//   - 其余路径回退到 index.html（静态导出路由）；
//   - /api/ 下的未知路径返回 JSON 404，避免把接口 404 误当页面。
func Handler() http.Handler {
	sub, err := fs.Sub(files, "dist")
	if err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "前端资源不可用", http.StatusInternalServerError)
		})
	}
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if strings.HasPrefix(path, "api/") {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"接口不存在"}}`))
			return
		}
		if path != "" {
			if _, err := fs.Stat(sub, path); err == nil {
				fileServer.ServeHTTP(w, r)
				return
			}
		}
		index, err := fs.ReadFile(sub, "index.html")
		if err != nil {
			http.Error(w, "前端资源不可用", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(index)
	})
}
