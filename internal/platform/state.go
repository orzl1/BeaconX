// Package state manages the on-disk home directory.
//
// Layout (tenant-scoped from day one):
//
//	<root>/
//	  tenants/<id>/sessions/
//	  tenants/<id>/evidence/
//	  tenants/<id>/traffic/
//	  skills/
//	  cache/
package state

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/orzl1/BeaconX/internal/tenant"
)

// State is a resolved home directory.
type State struct {
	Root string
}

// New returns a State rooted at root.
func New(root string) *State {
	return &State{Root: root}
}

// TenantDir returns the per-tenant directory.
func (s *State) TenantDir(id tenant.ID) string {
	return filepath.Join(s.Root, "tenants", fmt.Sprintf("%d", int64(id)))
}

// Ensure creates the directory tree with restrictive permissions.
//
// On platforms where permission bits are not honoured the call still succeeds;
// Windows ACLs are documented as a deployment step.
func (s *State) Ensure() error {
	dirs := []string{
		s.Root,
		filepath.Join(s.Root, "skills"),
		filepath.Join(s.Root, "cache"),
	}
	// Every tenant that has data on disk gets its subtree. v1 is single-tenant,
	// but the directory shape is fixed now so enabling multi-tenancy later does
	// not require moving files.
	for _, id := range []tenant.ID{tenant.DefaultID} {
		base := s.TenantDir(id)
		dirs = append(dirs,
			base,
			filepath.Join(base, "sessions"),
			filepath.Join(base, "evidence"),
			filepath.Join(base, "traffic"),
		)
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create state dir %s: %w", dir, err)
		}
	}
	return nil
}

// EnsureTenant creates the subtree for one tenant on demand.
func (s *State) EnsureTenant(id tenant.ID) error {
	base := s.TenantDir(id)
	for _, dir := range []string{
		base,
		filepath.Join(base, "sessions"),
		filepath.Join(base, "evidence"),
		filepath.Join(base, "traffic"),
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create tenant dir %s: %w", dir, err)
		}
	}
	return nil
}
