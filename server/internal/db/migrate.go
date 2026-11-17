package db

import (
	"database/sql"
	"fmt"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	dbmigrations "photography-workflow/server/db"
)

func Migrate(databaseURL, direction string) error {
	sqlDB, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer sqlDB.Close()

	goose.SetBaseFS(dbmigrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set dialect: %w", err)
	}

	switch direction {
	case "up":
		return goose.Up(sqlDB, "migrations")
	case "down":
		return goose.Down(sqlDB, "migrations")
	case "status":
		return goose.Status(sqlDB, "migrations")
	case "version":
		return goose.Version(sqlDB, "migrations")
	default:
		return fmt.Errorf("unknown direction %q (up|down|status|version)", direction)
	}
}
