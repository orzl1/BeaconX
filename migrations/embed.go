// Package migrations embeds the SQL migration set into the binary.
//
// Files are applied in lexical order by version prefix and recorded in
// schema_migrations with a checksum, so an edited historical migration is
// detected as drift instead of being silently applied.
package migrations

import "embed"

// FS holds every *.sql migration in this directory.
//
//go:embed *.sql
var FS embed.FS
