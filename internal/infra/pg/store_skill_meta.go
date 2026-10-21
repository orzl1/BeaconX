package pg

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// 本文件提供「技能元数据回滚」所需的最小 store 方法。
//
// 为什么单独一个文件：版本历史（Skills restore）是 B7 硬化新增的能力，只需要
// 一个「按 name 恢复 description/version」的写入口；放在独立文件里可以避免
// 修改 store_skill.go 的既有 CRUD（并行开发时减少冲突面）。
//
// 语义与 UpdateSkill 一致：name 是主键的一部分（同时是磁盘目录名），因此恢复
// 版本时不允许改名——版本快照里的 name 只作为一致性校验。

// RestoreSkillMeta 把技能元数据恢复成历史版本。
//
// 只更新 description 与 version；file_count 由 HTTP 层在文件恢复完成后用
// SetSkillFileCount 同步。技能不存在时返回 ErrNotFound。
func (s *Store) RestoreSkillMeta(ctx context.Context, tenantID int64, name, description, version string) (Skill, error) {
	if err := checkTenant(tenantID); err != nil {
		return Skill{}, err
	}
	if err := ValidateSkillName(name); err != nil {
		return Skill{}, err
	}
	in, err := normalizeSkillInput(SkillInput{
		Name:        name,
		Description: description,
		Version:     version,
	})
	if err != nil {
		return Skill{}, err
	}
	const q = `UPDATE skills SET description = $3, version = $4
		WHERE tenant_id = $1 AND name = $2 RETURNING ` + skillColumns
	skill, err := scanSkill(s.db.pool.QueryRow(ctx, q, tenantID, name, in.Description, in.Version))
	if errors.Is(err, pgx.ErrNoRows) {
		return Skill{}, fmt.Errorf("%w：技能 %q 不存在", ErrNotFound, name)
	}
	if err != nil {
		return Skill{}, fmt.Errorf("恢复技能元数据失败：%w", err)
	}
	return skill, nil
}
