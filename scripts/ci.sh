#!/usr/bin/env bash
# End-to-end integration gate: fresh Postgres + MinIO, migrations, seed,
# api + worker, then the smoke suite. Safe to run next to a dev stack because
# every port and the compose project name are isolated.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
COMPOSE_FILE="$ROOT/deploy/docker-compose.yml"
ARTIFACTS="${PWF_CI_ARTIFACT_DIR:-$ROOT/deploy/.ci}"
SERVER="$ROOT/server"
BIN_DIR="${PWF_CI_BIN_DIR:-$ARTIFACTS/bin}"

PG_PORT="${PWF_CI_PG_PORT:-55432}"
MINIO_PORT="${PWF_CI_MINIO_PORT:-59000}"
MINIO_CONSOLE_PORT="${PWF_CI_MINIO_CONSOLE_PORT:-59001}"
HTTP_PORT="${PWF_CI_HTTP_PORT:-18080}"
ALERT_PORT="${PWF_CI_ALERT_PORT:-18081}"
MAILSINK_PORT="${PWF_CI_MAILSINK_PORT:-18082}"
PROJECT="${PWF_CI_PROJECT:-pwf-ci-$$}"

ESIGN_SECRET="${PWF_CI_ESIGN_SECRET:-ci-esign-secret-0123456789abcdef}"
PAYMENT_SECRET="${PWF_CI_PAYMENT_SECRET:-ci-payment-secret-0123456789abcdef}"
PAYMENT_MERCHANT="${PWF_CI_PAYMENT_MERCHANT:-ci-merchant-001}"
ALERT_SECRET="${PWF_CI_ALERT_SECRET:-ci-alerts-secret-0123456789abcdef}"
REMINDER_SECRET="${PWF_CI_REMINDER_SECRET:-ci-reminders-secret-0123456789abcdef}"

mkdir -p "$ARTIFACTS" "$BIN_DIR"

compose() {
	PWF_PG_PORT="$PG_PORT" \
		PWF_MINIO_PORT="$MINIO_PORT" \
		PWF_MINIO_CONSOLE_PORT="$MINIO_CONSOLE_PORT" \
		docker compose -p "$PROJECT" -f "$COMPOSE_FILE" "$@"
}

API_PID=""
WORKER_PID=""
MAILSINK_PID=""

kill_tree() {
	local pid="$1"
	[ -z "$pid" ] && return 0
	kill -0 "$pid" 2>/dev/null || return 0
	kill "$pid" 2>/dev/null || true
	for _ in 1 2 3 4 5 6 7 8 9 10; do
		kill -0 "$pid" 2>/dev/null || return 0
		sleep 0.2
	done
	kill -9 "$pid" 2>/dev/null || true
}

cleanup() {
	local status=$?
	kill_tree "$MAILSINK_PID"
	kill_tree "$WORKER_PID"
	kill_tree "$API_PID"
	compose logs --no-color >"$ARTIFACTS/compose.log" 2>&1 || true
	compose down -v --remove-orphans >/dev/null 2>&1 || true
	exit "$status"
}
trap cleanup EXIT INT TERM

wait_http() {
	local url="$1" name="$2"
	for _ in $(seq 1 60); do
		if curl -fsS "$url" >/dev/null 2>&1; then
			return 0
		fi
		sleep 2
	done
	echo "ci: $name did not become ready at $url" >&2
	return 1
}

wait_tcp() {
	local host="$1" port="$2" name="$3"
	for _ in $(seq 1 60); do
		if (echo >"/dev/tcp/$host/$port") >/dev/null 2>&1; then
			return 0
		fi
		sleep 0.5
	done
	echo "ci: $name did not listen on $host:$port" >&2
	return 1
}

echo "ci: starting postgres + minio (project $PROJECT)"
compose up -d --wait postgres minio
wait_http "http://127.0.0.1:$MINIO_PORT/minio/health/live" minio

export PWF_DATABASE__URL="postgres://pwf_app:pwf-app@127.0.0.1:$PG_PORT/pwf?sslmode=disable"
export PWF_DATABASE__MIGRATE_URL="postgres://pwf:pwf@127.0.0.1:$PG_PORT/pwf?sslmode=disable"
export PWF_DATABASE__WORKER_URL="postgres://pwf_worker:pwf-worker@127.0.0.1:$PG_PORT/pwf?sslmode=disable"
export PWF_STORAGE__ENDPOINT="127.0.0.1:$MINIO_PORT"
export PWF_STORAGE__PUBLIC_BASE_URL="http://127.0.0.1:$MINIO_PORT/pwf"
export PWF_HTTP__ADDR=":$HTTP_PORT"
export PWF_ESIGN__CALLBACK_SECRET="$ESIGN_SECRET"
export PWF_ESIGN__SIGN_BASE_URL="http://127.0.0.1:$HTTP_PORT/sign"
export PWF_PAYMENT__CALLBACK_SECRET="$PAYMENT_SECRET"
export PWF_PAYMENT__MERCHANT_ID="$PAYMENT_MERCHANT"
export PWF_PAYMENT__PAY_BASE_URL="http://127.0.0.1:$HTTP_PORT/pay"
export PWF_ALERTS__WEBHOOK_URL="http://127.0.0.1:$ALERT_PORT/hook"
export PWF_ALERTS__WEBHOOK_SECRET="$ALERT_SECRET"
export PWF_SCHEDULE__TIMEZONE="Asia/Shanghai"
# Short TTL so the worker expiry sweep is exercised inside the smoke run.
export PWF_SCHEDULE__HOLD_TTL="5s"
# Reminders fire inside the smoke window and share the alert listener port.
export PWF_REMINDERS__SHOOT_LEADS="1m"
export PWF_REMINDERS__AUTH_LEAD="1m"
export PWF_REMINDERS__HOLD_LEAD="1s"
export PWF_REMINDERS__WEBHOOK_URL="http://127.0.0.1:$ALERT_PORT/hook"
export PWF_REMINDERS__WEBHOOK_SECRET="$REMINDER_SECRET"
# Proof mail is delivered to the development SMTP sink on this host; the smoke
# suite then inspects both the captured message and the queue.
export PWF_NOTIFY__EMAIL__ENABLED="true"
export PWF_NOTIFY__EMAIL__HOST="127.0.0.1"
export PWF_NOTIFY__EMAIL__PORT="$MAILSINK_PORT"
export PWF_NOTIFY__EMAIL__FROM="proof@example.com"
export PWF_NOTIFY__EMAIL__TLS="none"

echo "ci: building binaries"
(
	cd "$SERVER"
	go build -o "$BIN_DIR/api" ./cmd/api
	go build -o "$BIN_DIR/worker" ./cmd/worker
	go build -o "$BIN_DIR/migrate" ./cmd/migrate
	go build -o "$BIN_DIR/seed" ./cmd/seed
	go build -o "$BIN_DIR/smoke" ./cmd/smoke
	go build -o "$BIN_DIR/mailsink" ./cmd/mailsink
)

binary() {
	local name="$1"
	# go build -o bin/<name> writes that exact name on every OS, so anything
	# with a .exe suffix is a leftover from an older build and must not win.
	if [ -f "$BIN_DIR/$name" ]; then
		echo "$BIN_DIR/$name"
	else
		echo "$BIN_DIR/$name.exe"
	fi
}

API_BIN="$(binary api)"
WORKER_BIN="$(binary worker)"
MIGRATE_BIN="$(binary migrate)"
SEED_BIN="$(binary seed)"
SMOKE_BIN="$(binary smoke)"
MAILSINK_BIN="$(binary mailsink)"

echo "ci: migrating and seeding"
(
	cd "$SERVER"
	"$MIGRATE_BIN" -dir up
	"$SEED_BIN"
	# A staff account proves the extra-round gate only accepts the owner.
	"$SEED_BIN" -role staff -email staff@example.com -password photographer123 -name "Smoke Staff"
)

echo "ci: starting api, worker and mail sink"
# Captures from earlier runs must not leak into this run: smoke asserts that
# every captured message belongs to the flow it just exercised.
rm -rf "$ARTIFACTS/mailsink"
mkdir -p "$ARTIFACTS/mailsink"
"$MAILSINK_BIN" -addr "127.0.0.1:$MAILSINK_PORT" -dir "$ARTIFACTS/mailsink" \
	>"$ARTIFACTS/mailsink.log" 2>&1 &
MAILSINK_PID=$!
wait_tcp 127.0.0.1 "$MAILSINK_PORT" mailsink
cd "$SERVER"
"$API_BIN" >"$ARTIFACTS/api.log" 2>&1 &
API_PID=$!
"$WORKER_BIN" >"$ARTIFACTS/worker.log" 2>&1 &
WORKER_PID=$!
wait_http "http://127.0.0.1:$HTTP_PORT/healthz" api

SMOKE_ARGS=(
	-base "http://127.0.0.1:$HTTP_PORT"
	-esign-secret "$ESIGN_SECRET"
	-payment-secret "$PAYMENT_SECRET"
	-payment-merchant "$PAYMENT_MERCHANT"
	-job-timeout 180s
	-alert-listener "127.0.0.1:$ALERT_PORT"
	-alert-secret "$ALERT_SECRET"
	-reminder-secret "$REMINDER_SECRET"
	-reminder-lead "1m"
	-schedule-timezone "Asia/Shanghai"
	-schedule-hold-ttl "5s"
	-booking-race 200
	-staff-email "staff@example.com"
	-staff-password "photographer123"
	-mail-dir "$ARTIFACTS/mailsink"
	-database-url "$PWF_DATABASE__MIGRATE_URL"
)

echo "ci: running smoke"
"$SMOKE_BIN" "${SMOKE_ARGS[@]}"

if [ "${PWF_CI_MULTIPART:-0}" = "1" ]; then
	echo "ci: running 40MiB multipart smoke"
	"$SMOKE_BIN" "${SMOKE_ARGS[@]}" -size 41943040 -bulk 0
fi

if [ "${PWF_CI_AGENT:-0}" = "1" ]; then
	echo "ci: running agent headless import E2E"
	PWF_AGENT_E2E_SERVER="http://127.0.0.1:$HTTP_PORT" \
		PWF_AGENT_E2E_EMAIL="${PWF_CI_EMAIL:-owner@example.com}" \
		PWF_AGENT_E2E_PASSWORD="${PWF_CI_PASSWORD:-photographer123}" \
		bash "$ROOT/scripts/agent-e2e.sh"
fi

if [ "${PWF_CI_WEB:-0}" = "1" ]; then
	echo "ci: running gallery browser E2E"
	PWF_E2E_INSTALL_BROWSER="${PWF_CI_INSTALL_BROWSER:-1}" bash "$ROOT/scripts/gallery-e2e.sh"
fi

echo "ci: OK"
