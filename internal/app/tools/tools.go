// Package tools 是 Agent 可调用能力的注册表与内置工具实现。
//
// 每个工具声明 JSON Schema、风险级别与可用角色；执行入口在 internal/app/executor，
// 所有调用都必须先经过策略门（见 M4 设计）。
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"github.com/orzl1/BeaconX/internal/domain/policy"
)

// Definition 是工具的静态描述。
type Definition struct {
	Name        string
	Description string
	Parameters  json.RawMessage
	Risk        policy.Risk
	Roles       []string
}

// Result 是工具执行结果。
type Result struct {
	Output string
	Meta   map[string]any
}

// Tool 是可执行工具。
type Tool interface {
	Definition() Definition
	Execute(ctx context.Context, args json.RawMessage) (Result, error)
}

// Registry 保存工具定义，按角色过滤可见性。
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

// NewRegistry 创建并注册工具。
func NewRegistry(list ...Tool) (*Registry, error) {
	r := &Registry{tools: map[string]Tool{}}
	for _, t := range list {
		if err := r.Register(t); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// Register 注册工具；名称重复或定义非法时报错。
func (r *Registry) Register(t Tool) error {
	def := t.Definition()
	if def.Name == "" {
		return fmt.Errorf("工具名不能为空")
	}
	if !def.Risk.Valid() {
		return fmt.Errorf("工具 %s 的风险级别非法: %q", def.Name, def.Risk)
	}
	if len(def.Parameters) > 0 && !json.Valid(def.Parameters) {
		return fmt.Errorf("工具 %s 的参数 schema 不是合法 JSON", def.Name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.tools[def.Name]; exists {
		return fmt.Errorf("工具 %s 重复注册", def.Name)
	}
	r.tools[def.Name] = t
	return nil
}

// Get 按名称取工具。
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// List 返回指定角色可见的工具定义（角色为空时返回全部）。
func (r *Registry) List(role string) []Definition {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Definition, 0, len(r.tools))
	for _, t := range r.tools {
		def := t.Definition()
		if role != "" && !contains(def.Roles, role) {
			continue
		}
		out = append(out, def)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
