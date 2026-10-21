package httpapi

import "net/http"

// FeatureRoute 是功能模块自行注册的路由，用于让不同功能在各自文件里
// 独立新增接口，避免所有改动都集中修改 handler.go（并行开发时的冲突点）。
//
// 约定：feature 文件在 init() 中调用 RegisterFeatureRoutes；NewHandler 在
// 基础路由之后注册这些路由，并复用同一套鉴权中间件。
type FeatureRoute struct {
	Method  string
	Pattern string
	Summary string
	Public  bool
	// Build 在 NewHandler 时调用：feature 文件在 init 阶段注册路由，
	// 那时还没有 Options（Store/Tenant/Evidence 等），因此用工厂把
	// Options 延迟注入处理器。
	Build func(opts Options) http.HandlerFunc
}

var featureRoutes []FeatureRoute

// RegisterFeatureRoutes 注册一组功能路由（幂等与否由调用方保证：同一
// Method+Pattern 重复注册会 panic，便于尽早发现冲突）。
func RegisterFeatureRoutes(routes ...FeatureRoute) {
	for _, rt := range routes {
		if rt.Build == nil {
			panic("feature 路由缺少 Build 工厂: " + rt.Method + " " + rt.Pattern)
		}
		for _, existing := range featureRoutes {
			if existing.Method == rt.Method && existing.Pattern == rt.Pattern {
				panic("feature 路由重复注册: " + rt.Method + " " + rt.Pattern)
			}
		}
		featureRoutes = append(featureRoutes, rt)
	}
}

// featureRouteList 返回功能路由副本。
func featureRouteList() []FeatureRoute {
	return append([]FeatureRoute(nil), featureRoutes...)
}

// featureRouteSpecs 把功能路由转成基础 Route，用于 404/405 兜底与文档校验。
func featureRouteSpecs() []Route {
	out := make([]Route, 0, len(featureRoutes))
	for _, rt := range featureRoutes {
		out = append(out, Route{Method: rt.Method, Pattern: rt.Pattern, Summary: rt.Summary, Public: rt.Public})
	}
	return out
}
