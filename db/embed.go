// Package db embeds the SQL migrations so they ship inside the binary and can
// be applied without the goose CLI on the target host (see cmd/migrate).
package db

import "embed"

// Migrations holds the goose migration files.
//
//go:embed migrations/*.sql
var Migrations embed.FS
