// Package dbmigrations embeds the SQL migrations that live next to this file.
package dbmigrations

import "embed"

//go:embed migrations/*.sql
var FS embed.FS
