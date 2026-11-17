#!/usr/bin/env bash
# Migration rollback matrix on an isolated database: latest -> 0015 -> latest,
# plus the data guards in 0018/0017/0015 down. Runs its own compose project so
# it cannot touch a developer stack.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
COMPOSE_FILE="$ROOT/deploy/docker-compose.yml"
SERVER="$ROOT/server"
ARTIFACTS="$ROOT/deploy/.ci-migrations"
PG_PORT="${PWF_MATRIX_PG_PORT:-55436}"
PROJECT="${PWF_MATRIX_PROJECT:-pwf-migrations-$$}"

mkdir -p "$ARTIFACTS" "$SERVER/bin"

fail() {
	echo "migration-matrix: $*" >&2
	exit 1
}

compose() {
	PWF_PG_PORT="$PG_PORT" docker compose -p "$PROJECT" -f "$COMPOSE_FILE" "$@"
}

cleanup() {
	local status=$?
	compose down -v --remove-orphans >/dev/null 2>&1 || true
	exit "$status"
}
trap cleanup EXIT INT TERM

psql_sql() {
	docker exec -i "$PROJECT-postgres-1" psql -U pwf -d pwf -tAc "$1"
}

has_grant() {
	[ "$(psql_sql "SELECT count(*) FROM information_schema.role_table_grants
		WHERE grantee='pwf_worker' AND table_name='$1' AND privilege_type='$2'")" != "0" ] ||
		fail "pwf_worker lacks $2 on $1"
}

echo "migration-matrix: starting postgres (project $PROJECT)"
compose up -d --wait postgres

export PWF_DATABASE__MIGRATE_URL="postgres://pwf:pwf@127.0.0.1:$PG_PORT/pwf?sslmode=disable"
export PWF_DATABASE__URL="$PWF_DATABASE__MIGRATE_URL"
export PWF_DATABASE__WORKER_URL="$PWF_DATABASE__MIGRATE_URL"

(
	cd "$SERVER"
	go build -o bin/migrate ./cmd/migrate
	go build -o bin/seed ./cmd/seed
)

MIGRATE_BIN="$SERVER/bin/migrate"
[ -f "$MIGRATE_BIN" ] || MIGRATE_BIN="$SERVER/bin/migrate.exe"
SEED_BIN="$SERVER/bin/seed"
[ -f "$SEED_BIN" ] || SEED_BIN="$SERVER/bin/seed.exe"

migrate() {
	(cd "$SERVER" && "$MIGRATE_BIN" -dir "$1")
}

version() {
	migrate version 2>&1 | awk '{print $NF}'
}

echo "migration-matrix: up"
migrate up >"$ARTIFACTS/migrate-up.log" 2>&1 || fail "initial up failed"
VERSION="$(version)"
[ "$VERSION" = "20" ] || fail "version after up = $VERSION, want 20"

echo "migration-matrix: down 0020..0015 on an empty database"
migrate down >"$ARTIFACTS/down-0020.log" 2>&1 || fail "down 0020 failed"
migrate down >"$ARTIFACTS/down-0019.log" 2>&1 || fail "down 0019 failed"
migrate down >"$ARTIFACTS/down-0018.log" 2>&1 || fail "down 0018 failed"
migrate down >"$ARTIFACTS/down-0017.log" 2>&1 || fail "down 0017 failed"
migrate down >"$ARTIFACTS/down-0016.log" 2>&1 || fail "down 0016 failed"
migrate down >"$ARTIFACTS/down-0015.log" 2>&1 || fail "down 0015 failed"
# galleries predates 0015 (0001); only the objects added from 0015 on disappear.
for table in gallery_assets gallery_tokens gallery_selections \
	proof_rounds proof_round_items proof_round_tokens comments \
	retouch_tasks retouch_task_items asset_revisions asset_revision_derivatives \
	integration_jobs email_messages \
	deliveries delivery_items delivery_packages download_tokens \
	archives archive_items retention_policies; do
	[ "$(psql_sql "SELECT to_regclass('public.$table')")" = "" ] || fail "$table survived the empty rollback"
done
[ "$(psql_sql "SELECT to_regprocedure('public.delivery_token_lookup(text)')")" = "" ] ||
	fail "delivery_token_lookup survived the empty rollback"
[ "$(psql_sql "SELECT count(*) FROM information_schema.columns WHERE table_name='galleries' AND column_name IN ('version','manifest_key','selection_version')")" = "0" ] ||
	fail "galleries still has 0015 columns after down"
[ "$(psql_sql "SELECT count(*) FROM information_schema.columns WHERE table_name='clients' AND column_name='email'")" = "0" ] ||
	fail "clients.email survived down 0018"
[ "$(psql_sql "SELECT count(*) FROM information_schema.columns WHERE table_name='leads' AND column_name='email'")" = "0" ] ||
	fail "leads.email survived down 0020"
[ "$(psql_sql "SELECT count(*) FROM information_schema.columns WHERE table_name='contracts' AND column_name='included_rounds'")" = "0" ] ||
	fail "contracts.included_rounds survived down 0017"

echo "migration-matrix: up again"
migrate up >"$ARTIFACTS/up-again.log" 2>&1 || fail "re-up failed"
VERSION="$(version)"
[ "$VERSION" = "20" ] || fail "version after re-up = $VERSION, want 20"
[ "$(psql_sql "SELECT count(*) FROM information_schema.columns WHERE table_name='galleries' AND column_name='watermark_text'")" = "1" ] ||
	fail "watermark_text was not restored"
[ "$(psql_sql "SELECT count(*) FROM information_schema.columns WHERE table_name='clients' AND column_name='email'")" = "1" ] ||
	fail "clients.email was not restored"
[ "$(psql_sql "SELECT count(*) FROM information_schema.columns WHERE table_name='leads' AND column_name='email'")" = "1" ] ||
	fail "leads.email was not restored"
[ "$(psql_sql "SELECT count(*) FROM information_schema.columns WHERE table_name='contracts' AND column_name='included_rounds'")" = "1" ] ||
	fail "contracts.included_rounds was not restored"
[ "$(psql_sql "SELECT to_regclass('public.proof_rounds')")" = "proof_rounds" ] || fail "proof_rounds was not restored"
for table in deliveries delivery_items delivery_packages download_tokens \
	archives archive_items retention_policies; do
	[ "$(psql_sql "SELECT to_regclass('public.$table')")" = "$table" ] || fail "$table was not restored"
done
[ "$(psql_sql "SELECT to_regprocedure('public.delivery_token_lookup(text)')")" = "delivery_token_lookup(text)" ] ||
	fail "delivery_token_lookup was not restored"

migrate down >"$ARTIFACTS/down-clean-0020.log" 2>&1 || fail "down 0020 before guard checks failed"
VERSION="$(version)"
[ "$VERSION" = "19" ] || fail "version before guard checks = $VERSION, want 19"

echo "migration-matrix: data guards"
(
	cd "$SERVER"
	"$SEED_BIN" >/dev/null
)
psql_sql "WITH t AS (SELECT id FROM tenants LIMIT 1),
	p AS (INSERT INTO projects (tenant_id, code, title) SELECT id, 'MIG-MATRIX', 'Migration matrix' FROM t RETURNING id, tenant_id)
	INSERT INTO galleries (tenant_id, project_id, title) SELECT tenant_id, id, 'Guard row' FROM p" >/dev/null ||
	fail "could not insert the guard row"

psql_sql "INSERT INTO proof_rounds (tenant_id, project_id, gallery_id, round_no, due_at)
	SELECT g.tenant_id, g.project_id, g.id, 1, now() + interval '7 days' FROM galleries g LIMIT 1" >/dev/null ||
	fail "could not insert the proof guard row"
psql_sql "INSERT INTO deliveries (tenant_id, project_id, proof_round_id, status)
	SELECT r.tenant_id, r.project_id, r.id, 'pending' FROM proof_rounds r LIMIT 1" >/dev/null ||
	fail "could not insert the delivery guard row"
if migrate down >"$ARTIFACTS/down-guard-0019.log" 2>&1; then
	fail "down 0019 must refuse to drop delivery tables that contain rows"
fi
grep -q "delivery or archive tables contain rows" "$ARTIFACTS/down-guard-0019.log" ||
	fail "guard message missing from $(cat "$ARTIFACTS/down-guard-0019.log")"
VERSION="$(version)"
[ "$VERSION" = "19" ] || fail "version after refused down 0019 = $VERSION, want 19"

psql_sql "DELETE FROM deliveries" >/dev/null || fail "could not clear the delivery guard row"
migrate down >"$ARTIFACTS/down-clean-0019.log" 2>&1 || fail "down 0019 after clearing deliveries failed"
VERSION="$(version)"
[ "$VERSION" = "18" ] || fail "version after down 0019 = $VERSION, want 18"

psql_sql "INSERT INTO email_messages (tenant_id, to_email, subject) SELECT id, 'guard@example.com', 'Guard' FROM tenants LIMIT 1" >/dev/null ||
	fail "could not insert the email guard row"
if migrate down >"$ARTIFACTS/down-guard-0018.log" 2>&1; then
	fail "down 0018 must refuse to drop the queue while messages exist"
fi
grep -q "integration_jobs or email_messages contain rows" "$ARTIFACTS/down-guard-0018.log" ||
	fail "guard message missing from $(cat "$ARTIFACTS/down-guard-0018.log")"
VERSION="$(version)"
[ "$VERSION" = "18" ] || fail "version after refused down 0018 = $VERSION, want 18"

psql_sql "DELETE FROM email_messages" >/dev/null || fail "could not clear the email guard row"
migrate down >"$ARTIFACTS/down-clean-0018.log" 2>&1 || fail "down 0018 after clearing the queue failed"
VERSION="$(version)"
[ "$VERSION" = "17" ] || fail "version after down 0018 = $VERSION, want 17"

if migrate down >"$ARTIFACTS/down-guard-0017.log" 2>&1; then
	fail "down 0017 must refuse to drop proof tables that contain rows"
fi
grep -q "proof tables contain rows" "$ARTIFACTS/down-guard-0017.log" ||
	fail "guard message missing from $(cat "$ARTIFACTS/down-guard-0017.log")"
VERSION="$(version)"
[ "$VERSION" = "17" ] || fail "version after refused down 0017 = $VERSION, want 17"

psql_sql "DELETE FROM proof_rounds" >/dev/null || fail "could not clear the proof guard row"
migrate down >"$ARTIFACTS/down-clean-0017.log" 2>&1 || fail "down 0017 after clearing proof rows failed"
migrate down >"$ARTIFACTS/down-guard-0016.log" 2>&1 || fail "down 0016 with data should succeed"
if migrate down >"$ARTIFACTS/down-guard-0015.log" 2>&1; then
	fail "down 0015 must refuse to drop galleries that contain rows"
fi
grep -q "gallery tables contain rows" "$ARTIFACTS/down-guard-0015.log" ||
	fail "guard message missing from $(cat "$ARTIFACTS/down-guard-0015.log")"
VERSION="$(version)"
[ "$VERSION" = "15" ] || fail "version after refused down = $VERSION, want 15"

echo "migration-matrix: restoring latest"
migrate up >"$ARTIFACTS/up-restore.log" 2>&1 || fail "restore up failed"
VERSION="$(version)"
[ "$VERSION" = "20" ] || fail "final version = $VERSION, want 20"
# The re-run of 0019 must backfill a policy for every tenant the seed created.
[ "$(psql_sql "SELECT count(*) FROM retention_policies")" = "$(psql_sql "SELECT count(*) FROM tenants")" ] ||
	fail "retention_policies was not backfilled for the seeded tenants"
[ "$(psql_sql "SELECT count(*) FROM pg_constraint WHERE conname='notifications_type_check' AND pg_get_constraintdef(oid) LIKE '%delivery_ready%'")" = "1" ] ||
	fail "notifications CHECK was not extended with the M7 types"

for table in deliveries delivery_items delivery_packages download_tokens archives archive_items; do
	has_grant "$table" SELECT
	has_grant "$table" INSERT
	has_grant "$table" UPDATE
done
# Rebuilds reset items/packages; archives keep tombstones (no DELETE granted).
for table in delivery_items delivery_packages; do
	has_grant "$table" DELETE
done
for table in projects contracts invoices retention_policies; do
	has_grant "$table" SELECT
done
[ "$(psql_sql "SELECT count(*) FROM information_schema.role_table_grants
	WHERE grantee='pwf_worker' AND table_name IN ('archives','archive_items') AND privilege_type='DELETE'")" = "0" ] ||
	fail "pwf_worker must not hold DELETE on archives/archive_items"
# Finalize locks the project row with FOR SHARE, and PostgreSQL only allows that
# with UPDATE privilege; it must be column-scoped so the worker cannot write.
[ "$(psql_sql "SELECT count(*) FROM information_schema.role_table_grants
	WHERE grantee='pwf_worker' AND table_name='projects' AND privilege_type='UPDATE'")" = "0" ] ||
	fail "pwf_worker must not hold table-level UPDATE on projects"
[ "$(psql_sql "SELECT count(*) FROM information_schema.role_column_grants
	WHERE grantee='pwf_worker' AND table_name='projects' AND column_name='id'
	  AND privilege_type='UPDATE'")" = "1" ] ||
	fail "pwf_worker needs column-level UPDATE on projects.id for the finalize row lock"
docker exec -i "$PROJECT-postgres-1" psql -U pwf -d pwf -v ON_ERROR_STOP=1 -tAc \
	"BEGIN; SET LOCAL ROLE pwf_worker; SELECT id FROM projects LIMIT 1 FOR SHARE; ROLLBACK;" >/dev/null ||
	fail "pwf_worker cannot take a FOR SHARE lock on projects"

echo "migration-matrix: OK"
