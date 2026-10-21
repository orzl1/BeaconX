package state

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/orzl1/BeaconX/internal/tenant"
)

func TestEnsureCreatesTenantScopedTree(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	if err := s.Ensure(); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	want := []string{
		root,
		filepath.Join(root, "skills"),
		filepath.Join(root, "cache"),
		s.TenantDir(tenant.DefaultID),
		filepath.Join(s.TenantDir(tenant.DefaultID), "sessions"),
		filepath.Join(s.TenantDir(tenant.DefaultID), "evidence"),
		filepath.Join(s.TenantDir(tenant.DefaultID), "traffic"),
	}
	for _, dir := range want {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("missing %s: %v", dir, err)
		}
		if !info.IsDir() {
			t.Fatalf("%s is not a directory", dir)
		}
	}
}

func TestTenantDirShape(t *testing.T) {
	s := New("/srv/beaconx")
	if got, want := s.TenantDir(tenant.ID(7)), filepath.Join("/srv/beaconx", "tenants", "7"); got != want {
		t.Fatalf("TenantDir = %q, want %q", got, want)
	}
}

func TestEnsureIsIdempotent(t *testing.T) {
	s := New(t.TempDir())
	if err := s.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := s.Ensure(); err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
}

func TestEnsureTenant(t *testing.T) {
	s := New(t.TempDir())
	if err := s.EnsureTenant(tenant.ID(3)); err != nil {
		t.Fatalf("EnsureTenant: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.TenantDir(3), "sessions")); err != nil {
		t.Fatalf("tenant sessions dir: %v", err)
	}
}

func TestPermissionsOnUnix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not meaningful on Windows")
	}
	root := t.TempDir()
	s := New(root)
	if err := s.Ensure(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(s.TenantDir(tenant.DefaultID))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("tenant dir perm = %o, want 700", perm)
	}
}
