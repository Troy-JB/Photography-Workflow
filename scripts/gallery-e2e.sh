#!/usr/bin/env bash
# Browser end-to-end gate for M5: real Postgres + MinIO + api + worker, both
# production frontend builds served behind a same-origin proxy, then a
# Playwright walkthrough of the whole selection lifecycle.
#
# Isolation: its own compose project, ports and artifact directory, so it can
# run next to ci.sh and a developer stack. Network is required on first run
# (docker images, npm packages, Playwright Chromium when
# PWF_E2E_INSTALL_BROWSER=1).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
COMPOSE_FILE="$ROOT/deploy/docker-compose.yml"
ARTIFACTS="${PWF_GALLERY_ARTIFACT_DIR:-$ROOT/deploy/.ci-gallery}"
SERVER="$ROOT/server"
BIN_DIR="${PWF_GALLERY_BIN_DIR:-$ARTIFACTS/bin}"

PG_PORT="${PWF_GALLERY_PG_PORT:-55434}"
MINIO_PORT="${PWF_GALLERY_MINIO_PORT:-59006}"
MINIO_CONSOLE_PORT="${PWF_GALLERY_MINIO_CONSOLE_PORT:-59007}"
HTTP_PORT="${PWF_GALLERY_HTTP_PORT:-18084}"
ADMIN_PORT="${PWF_GALLERY_ADMIN_PORT:-4175}"
GALLERY_PORT="${PWF_GALLERY_WEB_PORT:-4174}"
MAILSINK_PORT="${PWF_GALLERY_MAILSINK_PORT:-19025}"
PROJECT="${PWF_GALLERY_PROJECT:-pwf-gallery-$$}"

mkdir -p "$ARTIFACTS" "$BIN_DIR"

compose() {
	PWF_PG_PORT="$PG_PORT" \
		PWF_MINIO_PORT="$MINIO_PORT" \
		PWF_MINIO_CONSOLE_PORT="$MINIO_CONSOLE_PORT" \
		docker compose -p "$PROJECT" -f "$COMPOSE_FILE" "$@"
}

ADMIN_PID=""
GALLERY_PID=""
MAILSINK_PID=""
WEB_ADMIN_PID=""
WEB_GALLERY_PID=""

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
	kill_tree "$GALLERY_PID"
	kill_tree "$ADMIN_PID"
	kill_tree "$WEB_ADMIN_PID"
	kill_tree "$WEB_GALLERY_PID"
	compose logs --no-color >"$ARTIFACTS/compose.log" 2>&1 || true
	compose down -v --remove-orphans >/dev/null 2>&1 || true
	exit "$status"
}
trap cleanup EXIT INT TERM

wait_tcp() {
	local host="$1" port="$2" name="$3"
	for _ in $(seq 1 60); do
		if (echo >"/dev/tcp/$host/$port") >/dev/null 2>&1; then
			return 0
		fi
		sleep 0.5
	done
	echo "gallery-e2e: $name did not listen on $host:$port" >&2
	return 1
}

wait_http() {
	local url="$1" name="$2"
	for _ in $(seq 1 90); do
		if curl -fsS "$url" >/dev/null 2>&1; then
			return 0
		fi
		sleep 2
	done
	echo "gallery-e2e: $name did not become ready at $url" >&2
	return 1
}

binary() {
	local name="$1"
	if [ -f "$BIN_DIR/$name" ]; then
		echo "$BIN_DIR/$name"
	else
		echo "$BIN_DIR/$name.exe"
	fi
}

echo "gallery-e2e: starting postgres + minio (project $PROJECT)"
compose up -d --wait postgres minio
wait_http "http://127.0.0.1:$MINIO_PORT/minio/health/live" minio

export PWF_DATABASE__URL="postgres://pwf_app:pwf-app@127.0.0.1:$PG_PORT/pwf?sslmode=disable"
export PWF_DATABASE__MIGRATE_URL="postgres://pwf:pwf@127.0.0.1:$PG_PORT/pwf?sslmode=disable"
export PWF_DATABASE__WORKER_URL="postgres://pwf_worker:pwf-worker@127.0.0.1:$PG_PORT/pwf?sslmode=disable"
export PWF_STORAGE__ENDPOINT="127.0.0.1:$MINIO_PORT"
export PWF_STORAGE__PUBLIC_BASE_URL="http://127.0.0.1:$MINIO_PORT/pwf"
export PWF_HTTP__ADDR=":$HTTP_PORT"
export PWF_GALLERY__PUBLIC_BASE_URL="http://127.0.0.1:$GALLERY_PORT"
export PWF_SCHEDULE__TIMEZONE="Asia/Shanghai"
# Proof mail lands in the bundled SMTP sink; the proof E2E inspects the capture.
export PWF_NOTIFY__EMAIL__ENABLED="true"
export PWF_NOTIFY__EMAIL__HOST="127.0.0.1"
export PWF_NOTIFY__EMAIL__PORT="$MAILSINK_PORT"
export PWF_NOTIFY__EMAIL__FROM="proof@example.com"
export PWF_NOTIFY__EMAIL__TLS="none"

echo "gallery-e2e: building server binaries"
(
	cd "$SERVER"
	go build -o "$BIN_DIR/api" ./cmd/api
	go build -o "$BIN_DIR/worker" ./cmd/worker
	go build -o "$BIN_DIR/migrate" ./cmd/migrate
	go build -o "$BIN_DIR/seed" ./cmd/seed
	go build -o "$BIN_DIR/mailsink" ./cmd/mailsink
)

API_BIN="$(binary api)"
WORKER_BIN="$(binary worker)"
MIGRATE_BIN="$(binary migrate)"
SEED_BIN="$(binary seed)"
MAILSINK_BIN="$(binary mailsink)"

echo "gallery-e2e: migrating and seeding"
(
	cd "$SERVER"
	"$MIGRATE_BIN" -dir up
	"$SEED_BIN"
)

echo "gallery-e2e: starting api, worker and mail sink"
# Stale captures from an earlier run would break the proof/delivery mail checks.
rm -rf "$ARTIFACTS/mailsink"
mkdir -p "$ARTIFACTS/mailsink"
cd "$SERVER"
"$MAILSINK_BIN" -addr "127.0.0.1:$MAILSINK_PORT" -dir "$ARTIFACTS/mailsink" \
	>"$ARTIFACTS/mailsink.log" 2>&1 &
MAILSINK_PID=$!
wait_tcp 127.0.0.1 "$MAILSINK_PORT" mailsink
"$API_BIN" >"$ARTIFACTS/api.log" 2>&1 &
API_PID=$!
"$WORKER_BIN" >"$ARTIFACTS/worker.log" 2>&1 &
WORKER_PID=$!
wait_http "http://127.0.0.1:$HTTP_PORT/healthz" api

echo "gallery-e2e: building frontends"
for app in web-admin web-gallery; do
	(
		cd "$ROOT/$app"
		if [ "${PWF_E2E_SKIP_INSTALL:-0}" != "1" ]; then
			npm ci --no-audit --no-fund
		fi
		npm run build
	)
done

if [ "${PWF_E2E_INSTALL_BROWSER:-0}" = "1" ]; then
	echo "gallery-e2e: installing Playwright Chromium"
	(cd "$ROOT/web-gallery" && npx playwright install --with-deps chromium)
fi

echo "gallery-e2e: serving builds on $ADMIN_PORT / $GALLERY_PORT"
# Git Bash rewrites arguments that look like POSIX paths; only the proxy
# prefixes must reach Node verbatim, the script path still needs conversion.
MSYS2_ARG_CONV_EXCL='/api/;/g/' \
	node "$ROOT/scripts/e2e-server.mjs" "$ROOT/web-admin/dist" "$ADMIN_PORT" "http://127.0.0.1:$HTTP_PORT" "/api/" \
	>"$ARTIFACTS/web-admin.log" 2>&1 &
WEB_ADMIN_PID=$!
MSYS2_ARG_CONV_EXCL='/api/;/g/' \
	node "$ROOT/scripts/e2e-server.mjs" "$ROOT/web-gallery/dist" "$GALLERY_PORT" "http://127.0.0.1:$HTTP_PORT" "/g/" \
	>"$ARTIFACTS/web-gallery.log" 2>&1 &
WEB_GALLERY_PID=$!
wait_http "http://127.0.0.1:$ADMIN_PORT/" web-admin
wait_http "http://127.0.0.1:$GALLERY_PORT/" web-gallery

echo "gallery-e2e: running Playwright walkthrough"
(
	cd "$ROOT/web-gallery"
	PWF_E2E_API="http://127.0.0.1:$HTTP_PORT" \
		PWF_E2E_ADMIN="http://127.0.0.1:$ADMIN_PORT" \
		PWF_E2E_GALLERY="http://127.0.0.1:$GALLERY_PORT" \
		PWF_E2E_EMAIL="${PWF_E2E_EMAIL:-owner@example.com}" \
		PWF_E2E_PASSWORD="${PWF_E2E_PASSWORD:-photographer123}" \
		PWF_E2E_PHOTOS="${PWF_E2E_PHOTOS:-4}" \
		PWF_E2E_SHOTS="${PWF_E2E_SHOTS:-}" \
		node e2e/gallery.e2e.mjs
)

echo "gallery-e2e: running proof walkthrough"
(
	cd "$ROOT/web-gallery"
	PWF_E2E_API="http://127.0.0.1:$HTTP_PORT" \
		PWF_E2E_ADMIN="http://127.0.0.1:$ADMIN_PORT" \
		PWF_E2E_GALLERY="http://127.0.0.1:$GALLERY_PORT" \
		PWF_E2E_EMAIL="${PWF_E2E_EMAIL:-owner@example.com}" \
		PWF_E2E_PASSWORD="${PWF_E2E_PASSWORD:-photographer123}" \
		PWF_E2E_MAIL_DIR="${PWF_E2E_MAIL_DIR:-$ARTIFACTS/mailsink}" \
		PWF_E2E_SHOTS="${PWF_E2E_SHOTS:-}" \
		node e2e/proof.e2e.mjs
)

echo "gallery-e2e: running delivery walkthrough"
(
	cd "$ROOT/web-gallery"
	PWF_E2E_API="http://127.0.0.1:$HTTP_PORT" \
		PWF_E2E_ADMIN="http://127.0.0.1:$ADMIN_PORT" \
		PWF_E2E_GALLERY="http://127.0.0.1:$GALLERY_PORT" \
		PWF_E2E_EMAIL="${PWF_E2E_EMAIL:-owner@example.com}" \
		PWF_E2E_PASSWORD="${PWF_E2E_PASSWORD:-photographer123}" \
		PWF_E2E_MAIL_DIR="${PWF_E2E_MAIL_DIR:-$ARTIFACTS/mailsink}" \
		PWF_E2E_SHOTS="${PWF_E2E_SHOTS:-}" \
		PWF_E2E_ESIGN_SECRET="${PWF_E2E_ESIGN_SECRET:-dev-only-esign-secret-change-me}" \
		PWF_E2E_PAYMENT_SECRET="${PWF_E2E_PAYMENT_SECRET:-dev-only-payment-secret-change-me}" \
		PWF_E2E_MERCHANT="${PWF_E2E_MERCHANT:-sandbox-merchant-001}" \
		node e2e/delivery.e2e.mjs
)

echo "gallery-e2e: OK"
