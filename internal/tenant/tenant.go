// Package tenant 将租户范围贯穿请求与任务上下文。
//
// BeaconX v1 以单租户部署运行，但从第一天起，租户标识就是每个请求上下文、
// 每次存储调用与每条磁盘路径的一部分。之后启用真正的隔离，只需从已认证
// 主体解析租户，而不再使用配置中的默认值。
package tenant

import "context"

// ID 是租户标识。
type ID int64

// DefaultID 在未显式配置租户时使用。
const DefaultID ID = 1

type ctxKey struct{}

// With 返回携带租户标识的子上下文。
func With(ctx context.Context, id ID) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// From 从上下文中提取租户标识。
func From(ctx context.Context) (ID, bool) {
	id, ok := ctx.Value(ctxKey{}).(ID)
	return id, ok
}

// MustFrom 提取租户标识，缺失时回退到 DefaultID。
//
// 无法在没有租户的情况下继续的调用方不应依赖它；它只是为了在中间件保证
// 租户存在的前提下让叶子辅助函数保持易用。
func MustFrom(ctx context.Context) ID {
	if id, ok := From(ctx); ok {
		return id
	}
	return DefaultID
}
