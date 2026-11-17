#!/usr/bin/env bash
# One-shot launch-verification drill against its own isolated stack:
#   1. boot postgres + minio + api + worker with test secrets
#   2. abort smoke right after it creates a payment order, leaving it pending
#   3. paymentreplay: 100 sequential + 100 concurrent payment.succeeded -> 1 settlement
#   4. open a refund and replay refund.succeeded 200 times -> 1 success
#   5. loadtest: ordinary-api p95 gate and booking-200 (200 concurrent holds, 1 winner)
#
# Every port and the compose project are isolated, so it can run next to a dev
# stack. It only talks to 127.0.0.1 with sandbox secrets: never point it at a
# real payment channel. Artifacts (API logs, JSON reports) land in deploy/.drill.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
COMPOSE_FILE="$ROOT/deploy/docker-compose.yml"
SERVER="$ROOT/server"
ART="$ROOT/deploy/.drill"
mkdir -p "$ART" "$SERVER/bin"

PG_PORT="${PWF_DRILL_PG_PORT:-55433}"
MINIO_PORT="${PWF_DRILL_MINIO_PORT:-59002}"
MINIO_CONSOLE_PORT="${PWF_DRILL_MINIO_CONSOLE_PORT:-59003}"
HTTP_PORT="${PWF_DRILL_HTTP_PORT:-18086}"
PROJECT="pwf-drill-$$"
API="http://127.0.0.1:$HTTP_PORT"
ESIGN_SECRET="drill-esign-secret-0123456789abcdef"
PAYMENT_SECRET="drill-payment-secret-0123456789abcdef"
MERCHANT="drill-merchant-001"

compose() {
	PWF_PG_PORT="$PG_PORT" PWF_MINIO_PORT="$MINIO_PORT" PWF_MINIO_CONSOLE_PORT="$MINIO_CONSOLE_PORT" \
		docker compose -p "$PROJECT" -f "$COMPOSE_FILE" "$@"
}

API_PID=""
WORKER_PID=""
cleanup() {
	local status=$?
	[ -n "$WORKER_PID" ] && kill "$WORKER_PID" 2>/dev/null || true
	[ -n "$API_PID" ] && kill "$API_PID" 2>/dev/null || true
	compose logs --no-color >"$ART/compose.log" 2>&1 || true
	compose down -v --remove-orphans >/dev/null 2>&1 || true
	exit "$status"
}
trap cleanup EXIT INT TERM

wait_http() {
	for _ in $(seq 1 90); do
		curl -fsS "$1" >/dev/null 2>&1 && return 0
		sleep 2
	done
	echo "drill: $1 did not become ready" >&2
	return 1
}

json() { node -e "let s='';process.stdin.on('data',d=>s+=d).on('end',()=>{const j=JSON.parse(s);console.log($1)})"; }

echo "drill: starting postgres + minio ($PROJECT)"
compose up -d --wait postgres minio
wait_http "http://127.0.0.1:$MINIO_PORT/minio/health/live"

export PWF_DATABASE__URL="postgres://pwf_app:pwf-app@127.0.0.1:$PG_PORT/pwf?sslmode=disable"
export PWF_DATABASE__MIGRATE_URL="postgres://pwf:pwf@127.0.0.1:$PG_PORT/pwf?sslmode=disable"
export PWF_DATABASE__WORKER_URL="postgres://pwf_worker:pwf-worker@127.0.0.1:$PG_PORT/pwf?sslmode=disable"
export PWF_STORAGE__ENDPOINT="127.0.0.1:$MINIO_PORT"
export PWF_STORAGE__PUBLIC_BASE_URL="http://127.0.0.1:$MINIO_PORT/pwf"
export PWF_HTTP__ADDR=":$HTTP_PORT"
export PWF_ESIGN__CALLBACK_SECRET="$ESIGN_SECRET"
export PWF_PAYMENT__CALLBACK_SECRET="$PAYMENT_SECRET"
export PWF_PAYMENT__MERCHANT_ID="$MERCHANT"
export PWF_SCHEDULE__TIMEZONE="Asia/Shanghai"
export PWF_GALLERY__PUBLIC_BASE_URL="$API"

echo "drill: building binaries"
(
	cd "$SERVER"
	go build -o bin/api ./cmd/api
	go build -o bin/worker ./cmd/worker
	go build -o bin/migrate ./cmd/migrate
	go build -o bin/seed ./cmd/seed
	go build -o bin/smoke ./cmd/smoke
	go build -o bin/loadtest ./cmd/loadtest
	go build -o bin/paymentreplay ./cmd/paymentreplay
)

echo "drill: migrating and seeding"
(cd "$SERVER" && ./bin/migrate -dir up && ./bin/seed)

echo "drill: starting api + worker"
(cd "$SERVER" && ./bin/api >"$ART/api.log" 2>&1 & echo $! >"$ART/api.pid")
(cd "$SERVER" && ./bin/worker >"$ART/worker.log" 2>&1 & echo $! >"$ART/worker.pid")
API_PID="$(cat "$ART/api.pid")"
WORKER_PID="$(cat "$ART/worker.pid")"
wait_http "$API/healthz"

echo "drill: seeding a pending order (deliberately failing payment callbacks)"
set +e
(cd "$SERVER" && ./bin/smoke -base "$API" -esign-secret "$ESIGN_SECRET" \
	-payment-secret "wrong-payment-secret" -payment-merchant "$MERCHANT" \
	-job-timeout 60s -booking-race 0 >"$ART/smoke-abort.log" 2>&1)
set -e

TOKEN=$(curl -fsS -X POST "$API/api/v1/auth/login" -H 'content-type: application/json' \
	-d '{"email":"owner@example.com","password":"photographer123"}' | json 'j.access_token')
ORDERS=$(curl -fsS "$API/api/v1/payments/orders?status=pending&limit=50" -H "authorization: Bearer $TOKEN")
read -r ORDER_ID OUT_TRADE AMOUNT PROJECT_ID < <(echo "$ORDERS" | node -e '
let s="";process.stdin.on("data",d=>s+=d).on("end",()=>{
	const j=JSON.parse(s);const items=(j.items||[]).slice().sort((a,b)=>b.created_at.localeCompare(a.created_at));
	if(!items.length){console.error("no pending order");process.exit(1)}
	const o=items[0];console.log(o.id,o.out_trade_no,o.amount_cents,o.project_id)})')
echo "drill: pending order $OUT_TRADE amount=$AMOUNT project=$PROJECT_ID"

echo "drill: paymentreplay payment.succeeded x200 (expect 1 settlement)"
"$SERVER/bin/paymentreplay" -base "$API" -secret "$PAYMENT_SECRET" -merchant "$MERCHANT" \
	-scenario payment -out-trade-no "$OUT_TRADE" -amount-cents "$AMOUNT" \
	-project "$PROJECT_ID" -json-out "$ART/replay-payment.json"

PAYMENTS=$(curl -fsS "$API/api/v1/payments?project_id=$PROJECT_ID" -H "authorization: Bearer $TOKEN")
PAYMENT_ID=$(echo "$PAYMENTS" | node -e '
let s="";process.stdin.on("data",d=>s+=d).on("end",()=>{
	const j=JSON.parse(s);const items=j.items||[];if(items.length!==1){console.error("payments="+items.length);process.exit(1)}
	console.log(items[0].id)})')
echo "drill: payment $PAYMENT_ID settled once; opening a refund"
REFUND=$(curl -fsS -X POST "$API/api/v1/payments/$PAYMENT_ID/refunds" -H "authorization: Bearer $TOKEN" \
	-H 'content-type: application/json' -d '{"reason":"launch drill refund replay"}')
OUT_REFUND=$(echo "$REFUND" | json 'j.out_refund_no')
echo "drill: refund $OUT_REFUND pending"

echo "drill: paymentreplay refund.succeeded x200 (expect 1 success)"
"$SERVER/bin/paymentreplay" -base "$API" -secret "$PAYMENT_SECRET" -merchant "$MERCHANT" \
	-scenario refund -out-refund-no "$OUT_REFUND" -amount-cents "$AMOUNT" \
	-payment-id "$PAYMENT_ID" -json-out "$ART/replay-refund.json"

echo "drill: loadtest ordinary-api (10s, 10 clients)"
"$SERVER/bin/loadtest" -base "$API" -scenario ordinary-api -concurrency 10 -duration 10s \
	-json-out "$ART/loadtest-ordinary.json"

echo "drill: loadtest booking-200 (200 concurrent holds)"
"$SERVER/bin/loadtest" -base "$API" -scenario booking-200 -concurrency 200 \
	-json-out "$ART/loadtest-booking.json"

echo "drill: OK"
