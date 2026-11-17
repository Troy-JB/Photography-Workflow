#!/usr/bin/env bash
# Payment callback replay drill: proves a sandbox callback is idempotent under
# sequential and concurrent replay, and (for refunds) that the delivery gate
# closes immediately.
#
# Never run this against production or a channel that handles real money: it
# forges signed sandbox callbacks by design. The tool refuses non-loopback
# targets unless -allow-remote is passed for a sandbox host.
#
# Examples:
#   # payment.succeeded 100 + 100 concurrent for an unpaid order
#   PWF_REPLAY_OUT_TRADE_NO=PWF... PWF_REPLAY_AMOUNT_CENTS=50000 \
#     bash scripts/payment-replay.sh -project <project-uuid>
#   # refund.succeeded 100 + 100 concurrent, then expect 409 payment_required
#   PWF_REPLAY_SCENARIO=refund PWF_REPLAY_OUT_REFUND_NO=RF... \
#     PWF_REPLAY_AMOUNT_CENTS=50000 PWF_REPLAY_DELIVERY_TOKEN=<link token> \
#     bash scripts/payment-replay.sh -payment-id <payment-uuid>
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SERVER="$ROOT/server"

BASE="${PWF_REPLAY_BASE:-http://127.0.0.1:8080}"
SCENARIO="${PWF_REPLAY_SCENARIO:-payment}"
SECRET="${PWF_REPLAY_SECRET:-${PWF_PAYMENT__CALLBACK_SECRET:-dev-only-payment-secret-change-me}}"
MERCHANT="${PWF_REPLAY_MERCHANT:-${PWF_PAYMENT__MERCHANT_ID:-sandbox-merchant-001}}"
OUT_TRADE_NO="${PWF_REPLAY_OUT_TRADE_NO:-}"
AMOUNT_CENTS="${PWF_REPLAY_AMOUNT_CENTS:-0}"
OUT_REFUND_NO="${PWF_REPLAY_OUT_REFUND_NO:-}"
DELIVERY_TOKEN="${PWF_REPLAY_DELIVERY_TOKEN:-}"
JSON_OUT="${PWF_REPLAY_JSON_OUT:-}"

if [ "$SCENARIO" = "payment" ] && [ -z "$OUT_TRADE_NO" ]; then
	echo "payment-replay: set PWF_REPLAY_OUT_TRADE_NO to the order being replayed" >&2
	exit 2
fi
if [ "$SCENARIO" = "refund" ] && [ -z "$OUT_REFUND_NO" ]; then
	echo "payment-replay: set PWF_REPLAY_OUT_REFUND_NO to the refund being replayed" >&2
	exit 2
fi

mkdir -p "$SERVER/bin"
(cd "$SERVER" && go build -o bin/paymentreplay ./cmd/paymentreplay)

args=(
	-base "$BASE"
	-scenario "$SCENARIO"
	-secret "$SECRET"
	-merchant "$MERCHANT"
	-amount-cents "$AMOUNT_CENTS"
)
if [ -n "$OUT_TRADE_NO" ]; then
	args+=(-out-trade-no "$OUT_TRADE_NO")
fi
if [ -n "$OUT_REFUND_NO" ]; then
	args+=(-out-refund-no "$OUT_REFUND_NO")
fi
if [ -n "$DELIVERY_TOKEN" ]; then
	args+=(-delivery-token "$DELIVERY_TOKEN")
fi
if [ -n "$JSON_OUT" ]; then
	args+=(-json-out "$JSON_OUT")
fi

echo "payment-replay: $SCENARIO against $BASE (200 callbacks)"
exec "$SERVER/bin/paymentreplay" "${args[@]}" "$@"
