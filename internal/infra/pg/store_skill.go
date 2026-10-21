package pg

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// 本文件实现 B5-2 的技能元数据存储（migration 0011）。
//
// 设计边界：
//   - 技能的正文存放在租户状态目录（<state>/skills/<tenant_id>/<name>/），
//     数据库只保存元数据与 file_count，避免磁盘与数据库两套真源；
//   - name 同时是目录名，因此在 store 层就强制正则，HTTP 层与上传路径共用
//     同一个校验函数，避免绕过；
//   - 同租户内重名返回 ErrConflict；不存在返回 ErrNotFound；
//   - 列表返回的切片一律非 nil（HTTP 层输出 [] 而不是 null）。

// skillNamePattern 与 HTTP 层、磁盘目录名共用；长度上限 64 字符。
var skillNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

const (
	// DefaultSkillVersion 是新建技能时的默认版本号。
	DefaultSkillVersion = "0.1.0"
	// maxSkillDescriptionRunes 限制描述长度，避免把大段文档塞进元数据。
	maxSkillDescriptionRunes = 2_000
	// maxSkillVersionRunes 限制版本号长度。
	maxSkillVersionRunes = 64
)

// ValidateSkillName 校验技能名：小写字母/数字开头，允许 - 与 _，≤64 字符。
func ValidateSkillName(name string) error {
	if !skillNamePattern.MatchString(name) {
		return fmt.Errorf("%w：技能名必须匹配 ^[a-z0-9][a-z0-9_-]{0,63}$", ErrInvalidInput)
	}
	return nil
}

// Skill 是一条技能元数据。
type Skill struct {
	TenantID    int64     `json:"tenant_id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Version     string    `json:"version"`
	FileCount   int       `json:"file_count"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// SkillInput 是创建技能（或上传后落库）的输入。
type SkillInput struct {
	Name        string
	Description string
	Version     string
	FileCount   int
}

// SkillUpdate 是技能元数据的局部更新输入：nil 指针表示不修改。
type SkillUpdate struct {
	Description *string
	Version     *string
	FileCount   *int
}

const skillColumns = `tenant_id, name, description, version, file_count, created_at, updated_at`

func scanSkill(row pgx.Row) (Skill, error) {
	var skill Skill
	err := row.Scan(&skill.TenantID, &skill.Name, &skill.Description, &skill.Version,
		&skill.FileCount, &skill.CreatedAt, &skill.UpdatedAt)
	return skill, err
}

// normalizeSkillInput 校验并填充默认值。
func normalizeSkillInput(in SkillInput) (SkillInput, error) {
	in.Name = strings.TrimSpace(in.Name)
	in.Description = strings.TrimSpace(in.Description)
	in.Version = strings.TrimSpace(in.Version)
	if err := ValidateSkillName(in.Name); err != nil {
		return in, err
	}
	if len([]rune(in.Description)) > maxSkillDescriptionRunes {
		return in, fmt.Errorf("%w：技能描述不能超过 %d 字符", ErrInvalidInput, maxSkillDescriptionRunes)
	}
	if in.Version == "" {
		in.Version = DefaultSkillVersion
	}
	if len([]rune(in.Version)) > maxSkillVersionRunes {
		return in, fmt.Errorf("%w：版本号不能超过 %d 字符", ErrInvalidInput, maxSkillVersionRunes)
	}
	if in.FileCount < 0 {
		return in, fmt.Errorf("%w：文件数不能为负数", ErrInvalidInput)
	}
	return in, nil
}

// ListSkills 列出本租户全部技能（按名称升序）。
func (s *Store) ListSkills(ctx context.Context, tenantID int64) ([]Skill, error) {
	if err := checkTenant(tenantID); err != nil {
		return nil, err
	}
	const q = `SELECT ` + skillColumns + ` FROM skills
		WHERE tenant_id = $1 ORDER BY name`
	rows, err := s.db.pool.Query(ctx, q, tenantID)
	if err != nil {
		return nil, fmt.Errorf("查询技能列表失败：%w", err)
	}
	defer rows.Close()
	out := make([]Skill, 0, 8)
	for rows.Next() {
		skill, err := scanSkill(rows)
		if err != nil {
			return nil, fmt.Errorf("读取技能失败：%w", err)
		}
		out = append(out, skill)
	}
	return out, rows.Err()
}

// CreateSkill 创建技能元数据；同租户内重名返回 ErrConflict。
func (s *Store) CreateSkill(ctx context.Context, tenantID int64, in SkillInput) (Skill, error) {
	if err := checkTenant(tenantID); err != nil {
		return Skill{}, err
	}
	validated, err := normalizeSkillInput(in)
	if err != nil {
		return Skill{}, err
	}
	const q = `INSERT INTO skills (tenant_id, name, description, version, file_count)
		VALUES ($1, $2, $3, $4, $5) RETURNING ` + skillColumns
	skill, err := scanSkill(s.db.pool.QueryRow(ctx, q, tenantID,
		validated.Name, validated.Description, validated.Version, validated.FileCount))
	if isUniqueViolation(err) {
		return Skill{}, fmt.Errorf("%w：技能 %q 已存在", ErrConflict, validated.Name)
	}
	if err != nil {
		return Skill{}, fmt.Errorf("创建技能失败：%w", err)
	}
	return skill, nil
}

// UpsertSkill 创建或覆盖技能元数据（上传路径使用：目录内容整体替换）。
func (s *Store) UpsertSkill(ctx context.Context, tenantID int64, in SkillInput) (Skill, error) {
	if err := checkTenant(tenantID); err != nil {
		return Skill{}, err
	}
	validated, err := normalizeSkillInput(in)
	if err != nil {
		return Skill{}, err
	}
	const q = `INSERT INTO skills (tenant_id, name, description, version, file_count)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (tenant_id, name) DO UPDATE SET
			description = EXCLUDED.description,
			version = EXCLUDED.version,
			file_count = EXCLUDED.file_count
		RETURNING ` + skillColumns
	skill, err := scanSkill(s.db.pool.QueryRow(ctx, q, tenantID,
		validated.Name, validated.Description, validated.Version, validated.FileCount))
	if err != nil {
		return Skill{}, fmt.Errorf("写入技能失败：%w", err)
	}
	return skill, nil
}

// GetSkill 读取技能详情。
func (s *Store) GetSkill(ctx context.Context, tenantID int64, name string) (Skill, error) {
	if err := checkTenant(tenantID); err != nil {
		return Skill{}, err
	}
	if err := ValidateSkillName(name); err != nil {
		return Skill{}, err
	}
	const q = `SELECT ` + skillColumns + ` FROM skills WHERE tenant_id = $1 AND name = $2`
	skill, err := scanSkill(s.db.pool.QueryRow(ctx, q, tenantID, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return Skill{}, fmt.Errorf("%w：技能 %q 不存在", ErrNotFound, name)
	}
	if err != nil {
		return Skill{}, fmt.Errorf("读取技能失败：%w", err)
	}
	return skill, nil
}

// UpdateSkill 局部更新技能元数据。
func (s *Store) UpdateSkill(ctx context.Context, tenantID int64, name string, upd SkillUpdate) (Skill, error) {
	if err := checkTenant(tenantID); err != nil {
		return Skill{}, err
	}
	current, err := s.GetSkill(ctx, tenantID, name)
	if err != nil {
		return Skill{}, err
	}
	in := SkillInput{
		Name:        current.Name,
		Description: current.Description,
		Version:     current.Version,
		FileCount:   current.FileCount,
	}
	if upd.Description != nil {
		in.Description = *upd.Description
	}
	if upd.Version != nil {
		in.Version = *upd.Version
	}
	if upd.FileCount != nil {
		in.FileCount = *upd.FileCount
	}
	validated, err := normalizeSkillInput(in)
	if err != nil {
		return Skill{}, err
	}
	const q = `UPDATE skills SET description = $3, version = $4, file_count = $5
		WHERE tenant_id = $1 AND name = $2 RETURNING ` + skillColumns
	skill, err := scanSkill(s.db.pool.QueryRow(ctx, q, tenantID, name,
		validated.Description, validated.Version, validated.FileCount))
	if errors.Is(err, pgx.ErrNoRows) {
		return Skill{}, fmt.Errorf("%w：技能 %q 不存在", ErrNotFound, name)
	}
	if err != nil {
		return Skill{}, fmt.Errorf("更新技能失败：%w", err)
	}
	return skill, nil
}

// SetSkillFileCount 只更新文件数（文件增删后同步统计）。
func (s *Store) SetSkillFileCount(ctx context.Context, tenantID int64, name string, count int) error {
	if err := checkTenant(tenantID); err != nil {
		return err
	}
	if count < 0 {
		return fmt.Errorf("%w：文件数不能为负数", ErrInvalidInput)
	}
	if err := ValidateSkillName(name); err != nil {
		return err
	}
	tag, err := s.db.pool.Exec(ctx,
		`UPDATE skills SET file_count = $3 WHERE tenant_id = $1 AND name = $2`, tenantID, name, count)
	if err != nil {
		return fmt.Errorf("更新技能文件数失败：%w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w：技能 %q 不存在", ErrNotFound, name)
	}
	return nil
}

// DeleteSkill 删除技能元数据（磁盘目录由 HTTP 层负责清理）。
func (s *Store) DeleteSkill(ctx context.Context, tenantID int64, name string) error {
	if err := checkTenant(tenantID); err != nil {
		return err
	}
	if err := ValidateSkillName(name); err != nil {
		return err
	}
	tag, err := s.db.pool.Exec(ctx,
		`DELETE FROM skills WHERE tenant_id = $1 AND name = $2`, tenantID, name)
	if err != nil {
		return fmt.Errorf("删除技能失败：%w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w：技能 %q 不存在", ErrNotFound, name)
	}
	return nil
}
