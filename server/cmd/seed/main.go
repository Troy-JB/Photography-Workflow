package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"photography-workflow/server/internal/auth"
	"photography-workflow/server/internal/config"
	"photography-workflow/server/internal/db"
)

func main() {
	cfgPath := flag.String("config", "config/config.yaml", "path to config file")
	tenantSlug := flag.String("tenant", "demo", "tenant slug")
	tenantName := flag.String("tenant-name", "Demo Studio", "tenant display name")
	email := flag.String("email", "owner@example.com", "owner login email")
	password := flag.String("password", "photographer123", "owner password (min 8 chars)")
	name := flag.String("name", "Demo Owner", "owner display name")
	role := flag.String("role", "owner", "membership role: owner, staff, assistant or retoucher")
	flag.Parse()

	if err := run(*cfgPath, *tenantSlug, *tenantName, *email, *password, *name, *role); err != nil {
		fmt.Fprintf(os.Stderr, "seed: %v\n", err)
		os.Exit(1)
	}
}

var validRoles = map[string]bool{"owner": true, "staff": true, "assistant": true, "retoucher": true}

func run(cfgPath, tenantSlug, tenantName, email, password, name, role string) error {
	if !validRoles[role] {
		return fmt.Errorf("invalid role %q (want owner, staff, assistant or retoucher)", role)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := db.Open(ctx, cfg.Database.MigrateURL, cfg.Database.MaxConns, cfg.Database.MinConns)
	if err != nil {
		return err
	}
	defer pool.Close()

	var tenantID uuid.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO tenants (slug, name) VALUES ($1, $2)
		ON CONFLICT (slug) DO UPDATE SET name = EXCLUDED.name
		RETURNING id`, tenantSlug, tenantName).Scan(&tenantID); err != nil {
		return fmt.Errorf("upsert tenant: %w", err)
	}

	var userID uuid.UUID
	created := false
	err = pool.QueryRow(ctx, `SELECT id FROM users WHERE lower(email) = lower($1)`, email).Scan(&userID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		hash, err := auth.HashPassword(password)
		if err != nil {
			return err
		}
		if err := pool.QueryRow(ctx, `
			INSERT INTO users (email, password_hash, name, photographer_code)
			VALUES ($1, $2, $3, 'PH' || upper(substr(replace(gen_random_uuid()::text, '-', ''), 1, 10)))
			RETURNING id`, email, hash, name).Scan(&userID); err != nil {
			return fmt.Errorf("insert user: %w", err)
		}
		created = true
	case err != nil:
		return fmt.Errorf("find user: %w", err)
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO memberships (tenant_id, user_id, role) VALUES ($1, $2, $3)
		ON CONFLICT (tenant_id, user_id) DO UPDATE SET role = EXCLUDED.role`,
		tenantID, userID, role); err != nil {
		return fmt.Errorf("upsert membership: %w", err)
	}

	fmt.Printf("tenant  %s (%s)\nuser    %s\nrole    %s\n", tenantSlug, tenantID, email, role)
	if created {
		fmt.Printf("login   %s / %s\n", email, password)
	} else {
		fmt.Println("login   existing account, password unchanged")
	}
	return nil
}
