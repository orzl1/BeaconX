package migrate

import (
	"context"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/orzl1/BeaconX/migrations"
)

func TestLoadOrdersAndHashes(t *testing.T) {
	fsys := fstest.MapFS{
		"0002_second.sql": {Data: []byte("SELECT 2;")},
		"0001_first.sql":  {Data: []byte("SELECT 1;")},
	}
	ms, err := Load(fsys)
	if err != nil {
		t.Fatalf("Load：%v", err)
	}
	if len(ms) != 2 {
		t.Fatalf("len = %d", len(ms))
	}
	if ms[0].Version != 1 || ms[1].Version != 2 {
		t.Fatalf("未按版本排序：%+v", ms)
	}
	if len(ms[0].Checksum) != 64 {
		t.Fatalf("校验和 = %q", ms[0].Checksum)
	}
	if Latest(ms) != 2 {
		t.Fatalf("最新版本 = %d", Latest(ms))
	}
}

func TestLoadChecksumChangesWithContent(t *testing.T) {
	a, err := Load(fstest.MapFS{"0001_a.sql": {Data: []byte("SELECT 1;")}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := Load(fstest.MapFS{"0001_a.sql": {Data: []byte("SELECT 2;")}})
	if err != nil {
		t.Fatal(err)
	}
	if a[0].Checksum == b[0].Checksum {
		t.Fatal("校验和必须取决于文件内容")
	}
}

func TestLoadRejectsBadNames(t *testing.T) {
	cases := map[string]fs.FS{
		"缺少版本号": fstest.MapFS{"init.sql": {Data: []byte("--")}},
		"版本重复":  fstest.MapFS{"0001_a.sql": {Data: []byte("--")}, "0001_b.sql": {Data: []byte("--")}},
		"非 SQL": fstest.MapFS{"README.md": {Data: []byte("x")}},
		"空集合":   fstest.MapFS{},
	}
	for name, fsys := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(fsys); err == nil {
				t.Fatal("应当返回错误")
			}
		})
	}
}

func TestEmbeddedMigrationsAreValid(t *testing.T) {
	// 内嵌的迁移集合必须始终可以解析；这可以防止格式错误的文件在构建时
	// 进入生产环境。
	ms, err := Load(migrations.FS)
	if err != nil {
		t.Fatalf("内嵌迁移无效：%v", err)
	}
	if Latest(ms) < 1 {
		t.Fatalf("期望至少有一个迁移，实际为 %d", Latest(ms))
	}
	for _, m := range ms {
		if strings.TrimSpace(m.SQL) == "" {
			t.Fatalf("迁移 %s 为空", m.Name)
		}
	}
}

func TestShortChecksumNeverPanics(t *testing.T) {
	for _, in := range []string{"", "abc", "deadbeef", strings.Repeat("a", 64)} {
		if out := short(in); len(out) > 12 {
			t.Fatalf("short(%q) = %q", in, out)
		}
	}
}

func TestApplyRequiresPool(t *testing.T) {
	if _, err := Apply(context.Background(), nil, fstest.MapFS{"0001_a.sql": {Data: []byte("--")}}); err == nil {
		t.Fatal("连接池为 nil 时应当返回错误")
	}
}
