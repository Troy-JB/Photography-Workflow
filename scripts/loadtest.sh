#!/usr/bin/env bash
# Load-test entry point for the launch gate. It builds cmd/loadtest and runs
# one scenario against an existing stack (api + worker + seeded owner).
#
# Only ever point this at a test environment. booking-200 creates hundreds of
# projects and a resource; selection-write uploads a fixture and publishes a
# gallery, so both need a running worker and object storage.
#
# Examples:
#   bash scripts/loadtest.sh -scenario ordinary-api -concurrency 20 -duration 60s
#   bash scripts/loadtest.sh -scenario selection-write -concurrency 20 -duration 120s
#   bash scripts/loadtest.sh -scenario booking-200 -concurrency 200
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SERVER="$ROOT/server"

BASE="${PWF_LOADTEST_BASE:-http://127.0.0.1:8080}"
SCENARIO="${PWF_LOADTEST_SCENARIO:-ordinary-api}"
CONCURRENCY="${PWF_LOADTEST_CONCURRENCY:-20}"
DURATION="${PWF_LOADTEST_DURATION:-30s}"
EMAIL="${PWF_LOADTEST_EMAIL:-owner@example.com}"
PASSWORD="${PWF_LOADTEST_PASSWORD:-photographer123}"
JSON_OUT="${PWF_LOADTEST_JSON_OUT:-}"

mkdir -p "$SERVER/bin"
(cd "$SERVER" && go build -o bin/loadtest ./cmd/loadtest)

args=(
	-base "$BASE"
	-scenario "$SCENARIO"
	-concurrency "$CONCURRENCY"
	-duration "$DURATION"
	-email "$EMAIL"
	-password "$PASSWORD"
)
if [ -n "$JSON_OUT" ]; then
	args+=(-json-out "$JSON_OUT")
fi

echo "loadtest: $SCENARIO against $BASE (concurrency $CONCURRENCY)"
exec "$SERVER/bin/loadtest" "${args[@]}" "$@"
