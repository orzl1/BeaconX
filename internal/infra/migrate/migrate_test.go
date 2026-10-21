package migrate

import (
	"context"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/beaconx/beaconx/migrations"
)

func TestLoadOrdersAndHashes(t *testing.T) {
	fsys := fstest.MapFS{
		"0002_second.sql": {Data: []byte("SELECT 2;")},
		"0001_first.sql":  {Data: []byte("SELECT 1;")},
	}
	ms, err := Load(fsys)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(ms) != 2 {
		t.Fatalf("len = %d", len(ms))
	}
	if ms[0].Version != 1 || ms[1].Version != 2 {
		t.Fatalf("not ordered: %+v", ms)
	}
	if len(ms[0].Checksum) != 64 {
		t.Fatalf("checksum = %q", ms[0].Checksum)
	}
	if Latest(ms) != 2 {
		t.Fatalf("latest = %d", Latest(ms))
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
		t.Fatal("checksum must depend on content")
	}
}

func TestLoadRejectsBadNames(t *testing.T) {
	cases := map[string]fs.FS{
		"no version": fstest.MapFS{"init.sql": {Data: []byte("--")}},
		"duplicate":  fstest.MapFS{"0001_a.sql": {Data: []byte("--")}, "0001_b.sql": {Data: []byte("--")}},
		"non sql":    fstest.MapFS{"README.md": {Data: []byte("x")}},
		"empty set":  fstest.MapFS{},
	}
	for name, fsys := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(fsys); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestEmbeddedMigrationsAreValid(t *testing.T) {
	// The embedded set must always parse; this guards against a malformed file
	// reaching production at build time.
	ms, err := Load(migrations.FS)
	if err != nil {
		t.Fatalf("embedded migrations invalid: %v", err)
	}
	if Latest(ms) < 1 {
		t.Fatalf("expected at least one migration, got %d", Latest(ms))
	}
	for _, m := range ms {
		if strings.TrimSpace(m.SQL) == "" {
			t.Fatalf("migration %s is empty", m.Name)
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
		t.Fatal("expected error for nil pool")
	}
}
