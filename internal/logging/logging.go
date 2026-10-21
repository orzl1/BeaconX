// Package logging 构建进程级的结构化日志器。
package logging

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/orzl1/BeaconX/internal/config"
)

// New 根据 cfg 返回一个 slog 日志器。
func New(cfg config.Log) (*slog.Logger, error) {
	level, err := parseLevel(cfg.Level)
	if err != nil {
		return nil, err
	}
	opts := &slog.HandlerOptions{Level: level}

	var handler slog.Handler
	switch strings.ToLower(cfg.Format) {
	case "text":
		handler = slog.NewTextHandler(os.Stdout, opts)
	default:
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}
	return slog.New(handler), nil
}

func parseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("未知的日志级别 %q", s)
	}
}
