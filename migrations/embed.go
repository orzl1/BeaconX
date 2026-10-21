// Package migrations 将 SQL 迁移集嵌入二进制。
//
// 文件按版本前缀的字典序应用，并连同校验和记录到 schema_migrations；
// 因此被修改过的历史迁移会被识别为漂移，而不是被静默应用。
package migrations

import "embed"

// FS 保存本目录下的所有 *.sql 迁移文件。
//
//go:embed *.sql
var FS embed.FS
