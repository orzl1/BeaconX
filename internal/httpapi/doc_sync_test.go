package httpapi

import (
	"os"
	"strings"
	"testing"
)

// TestProjectMapDocumentsAllRoutes 强制 docs/project-map.md 与路由表同步：
// 新增/修改路由却没有更新唯一真源文档时，CI 直接失败。
func TestProjectMapDocumentsAllRoutes(t *testing.T) {
	raw, err := os.ReadFile("../../docs/project-map.md")
	if err != nil {
		t.Fatalf("读取 docs/project-map.md 失败（唯一真源文档必须存在）: %v", err)
	}
	doc := string(raw)
	all := append(append([]Route(nil), Routes()...), featureRouteSpecs()...)
	for _, rt := range all {
		if !strings.Contains(doc, rt.Pattern) {
			t.Errorf("docs/project-map.md 未记录路由 %s %s；请同步更新唯一真源文档", rt.Method, rt.Pattern)
		}
	}
}
