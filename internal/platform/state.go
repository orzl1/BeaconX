// Package state 管理磁盘上的家目录。
//
// 目录布局（从第一天起即按租户隔离）：
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

// State 是解析后的家目录。
type State struct {
	Root string
}

// New 返回以 root 为根目录的 State。
func New(root string) *State {
	return &State{Root: root}
}

// TenantDir 返回单个租户的目录。
func (s *State) TenantDir(id tenant.ID) string {
	return filepath.Join(s.Root, "tenants", fmt.Sprintf("%d", int64(id)))
}

// Ensure 以受限权限创建目录树。
//
// 在不遵守权限位的平台上，该调用仍会成功；Windows ACL 将作为部署步骤
// 另行说明。
func (s *State) Ensure() error {
	dirs := []string{
		s.Root,
		filepath.Join(s.Root, "skills"),
		filepath.Join(s.Root, "cache"),
	}
	// 凡是在磁盘上有数据的租户都会获得自己的子树。v1 是单租户，
	// 但目录形态现在就固定下来，这样以后启用多租户时无需搬迁文件。
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
			return fmt.Errorf("创建状态目录 %s 失败：%w", dir, err)
		}
	}
	return nil
}

// EnsureTenant 按需创建一个租户的子树。
func (s *State) EnsureTenant(id tenant.ID) error {
	base := s.TenantDir(id)
	for _, dir := range []string{
		base,
		filepath.Join(base, "sessions"),
		filepath.Join(base, "evidence"),
		filepath.Join(base, "traffic"),
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("创建租户目录 %s 失败：%w", dir, err)
		}
	}
	return nil
}
