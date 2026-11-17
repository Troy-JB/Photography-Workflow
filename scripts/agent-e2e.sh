#!/usr/bin/env bash
# Headless end-to-end gate for the capture agent: builds the CLI, imports a
# fixture card into two destinations, verifies byte-identical copies, checks
# duplicate detection and the status/self-test reporting.
#
# Runs on Linux CI and on Windows (Git Bash). PWF_AGENT_E2E_DIR keeps the
# scratch tree for inspection; otherwise it is removed on exit.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
AGENT="$ROOT/agent"
KEEP="${PWF_AGENT_E2E_DIR:-}"
WORK="${KEEP:-$(mktemp -d)}"
EXE=""
case "$(uname -s)" in
MINGW* | MSYS* | CYGWIN*) EXE=".exe" ;;
esac
BIN="$AGENT/build/bin/pwf-agent-ci$EXE"

cleanup() {
	local status=$?
	[ -n "$KEEP" ] || rm -rf "$WORK"
	exit "$status"
}
trap cleanup EXIT INT TERM

fail() {
	echo "agent-e2e: $*" >&2
	exit 1
}

sha_list() {
	# Hash multiset of every regular file below a directory.
	find "$1" -type f -exec sha256sum {} \; | cut -d' ' -f1 | sort
}

echo "agent-e2e: building CLI"
(
	cd "$AGENT"
	CGO_ENABLED=0 go build -trimpath -o "$BIN" ./cmd/pwf-agent
)

SRC="$WORK/card"
mkdir -p "$SRC/DCIM/100CANON" "$SRC/DCIM/101CANON"
head -c 1048576 /dev/urandom >"$SRC/DCIM/100CANON/IMG_0001.CR3"
head -c 262144 /dev/urandom >"$SRC/DCIM/100CANON/IMG_0001.JPG"
head -c 524288 /dev/urandom >"$SRC/DCIM/101CANON/IMG_0002.JPG"
printf 'sidecar' >"$SRC/DCIM/101CANON/IMG_0002.XMP"
head -c 65536 /dev/urandom >"$SRC/DCIM/101CANON/CLIP_0003.MP4"
printf 'subtitle' >"$SRC/DCIM/101CANON/CLIP_0003.SRT"

export PWF_AGENT_DATA="$WORK/data"
mkdir -p "$PWF_AGENT_DATA" "$WORK/A" "$WORK/B"

echo "agent-e2e: importing into A + B"
IMPORT_LOG="$WORK/import.log"
set +e
"$BIN" import -src "$SRC" -project E2E-CARD -date 2026-01-02 \
	-a "$WORK/A" -b "$WORK/B" -workers 2 >"$IMPORT_LOG" 2>&1
STATUS=$?
set -e
cat "$IMPORT_LOG"
[ "$STATUS" = 0 ] || fail "import exited $STATUS, want 0"
grep -q 'status=local_complete files=6 local_verified=6 failed=0' "$IMPORT_LOG" ||
	fail "unexpected import summary"

echo "agent-e2e: verifying dual copies"
SRC_SUM="$(sha_list "$SRC")"
[ "$(sha_list "$WORK/A")" = "$SRC_SUM" ] || fail "destination A differs from the card"
[ "$(sha_list "$WORK/B")" = "$SRC_SUM" ] || fail "destination B differs from the card"
if find "$WORK/A" "$WORK/B" -name '.pwf-*' -print -quit | grep -q .; then
	fail "temporary .pwf-* files were left behind"
fi

echo "agent-e2e: duplicate card is rejected"
set +e
"$BIN" import -src "$SRC" -project E2E-CARD -date 2026-01-02 \
	-a "$WORK/A" -b "$WORK/B" -workers 2 >"$WORK/duplicate.log" 2>&1
STATUS=$?
set -e
cat "$WORK/duplicate.log"
[ "$STATUS" = 4 ] || fail "duplicate import exited $STATUS, want 4"

echo "agent-e2e: status and list report the completed import"
IMPORT_ID="$(sed -n 's/^import \([0-9a-f-]\{36\}\) created.*/\1/p' "$IMPORT_LOG")"
[ -n "$IMPORT_ID" ] || fail "could not read the import id from the import log"
"$BIN" status -id "$IMPORT_ID" -json >"$WORK/status.json"
grep -q '"status": "local_complete"' "$WORK/status.json" ||
	fail "status does not report local_complete"
"$BIN" list -json >"$WORK/list.json"
grep -q 'E2E-CARD' "$WORK/list.json" || fail "list does not contain the import"

echo "agent-e2e: selftest"
"$BIN" selftest -json >"$WORK/selftest.json"
grep -q '"ok": true' "$WORK/selftest.json" || fail "selftest failed"
cat "$WORK/selftest.json"

# Live derivative upload: only runs when the caller provides a running API
# (scripts/ci.sh does). It proves the gallery preview is generated, uploaded,
# acknowledged and surfaced as derivative_ready on the server. The card uses
# real JPEGs because random bytes cannot be previewed.
if [ -n "${PWF_AGENT_E2E_SERVER:-}" ]; then
	API="${PWF_AGENT_E2E_SERVER%/}"
	EMAIL="${PWF_AGENT_E2E_EMAIL:-owner@example.com}"
	PASSWORD="${PWF_AGENT_E2E_PASSWORD:-photographer123}"
	STATE="$WORK/server-state/state.db"
	mkdir -p "$(dirname "$STATE")"

	echo "agent-e2e: live server at $API"
	LOGIN_JSON="$(curl -fsS -X POST "$API/api/v1/auth/login" \
		-H 'content-type: application/json' \
		-d "{\"email\":\"$EMAIL\",\"password\":\"$PASSWORD\"}")" ||
		fail "could not sign in to $API"
	TOKEN="$(printf '%s' "$LOGIN_JSON" | sed -n 's/.*"access_token":"\([^"]*\)".*/\1/p')"
	[ -n "$TOKEN" ] || fail "login response had no access_token"
	PROJECT_JSON="$(curl -fsS -X POST "$API/api/v1/projects" \
		-H "authorization: Bearer $TOKEN" -H 'content-type: application/json' \
		-d "{\"title\":\"Agent derivative E2E $(date +%s)\"}")" ||
		fail "could not create the server project"
	PROJECT_ID="$(printf '%s' "$PROJECT_JSON" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')"
	[ -n "$PROJECT_ID" ] || fail "project response had no id"

	JPEG_DIR="$WORK/server-card/DCIM/100CANON"
	mkdir -p "$JPEG_DIR" "$WORK/server-dest"
	cat >"$WORK/genjpeg.go" <<'EOF'
package main

import (
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"strconv"
)

func main() {
	n, _ := strconv.Atoi(os.Args[2])
	seed, _ := strconv.Atoi(os.Args[3])
	img := image.NewRGBA(image.Rect(0, 0, 900, 600))
	for y := 0; y < 600; y++ {
		for x := 0; x < 900; x++ {
			img.Set(x, y, color.RGBA{uint8(40 + n*60 + seed%41), uint8(200 - y/5), uint8(80 + x/8), 255})
		}
	}
	f, err := os.Create(os.Args[1])
	if err != nil {
		panic(err)
	}
	if err := jpeg.Encode(f, img, &jpeg.Options{Quality: 85}); err != nil {
		panic(err)
	}
	if err := f.Close(); err != nil {
		panic(err)
	}
}
EOF
	SEED="$(date +%s)"
	(cd "$AGENT" && go run "$WORK/genjpeg.go" "$JPEG_DIR/IMG_9001.JPG" 1 "$SEED" &&
		go run "$WORK/genjpeg.go" "$JPEG_DIR/IMG_9002.JPG" 2 "$SEED") ||
		fail "could not generate JPEG fixtures"

	echo "agent-e2e: importing + uploading to the live server"
	"$BIN" import -src "$WORK/server-card" -project AGENT-E2E -project-id "$PROJECT_ID" \
		-date 2026-01-03 -a "$WORK/server-dest" -no-backup -db "$STATE" >"$WORK/server-import.log" 2>&1 ||
		fail "server import failed: $(cat "$WORK/server-import.log")"
	"$BIN" login -server "$API" -email "$EMAIL" -password "$PASSWORD" -db "$STATE" \
		>"$WORK/server-login.log" 2>&1 ||
		fail "agent login failed: $(cat "$WORK/server-login.log")"
	"$BIN" upload -server "$API" -db "$STATE" -workers 2 >"$WORK/server-upload.log" 2>&1 ||
		fail "agent upload failed: $(cat "$WORK/server-upload.log")"
	cat "$WORK/server-upload.log"
	grep -q 'derivatives=2/0' "$WORK/server-upload.log" ||
		fail "expected both gallery previews to upload without failures"

	echo "agent-e2e: waiting for derivative_ready on the server"
	READY=0
	for _ in $(seq 1 30); do
		BODY="$(curl -fsS -H "authorization: Bearer $TOKEN" \
			"$API/api/v1/projects/$PROJECT_ID/assets?limit=200")" || true
		READY="$(printf '%s' "$BODY" | grep -o '"status":"derivative_ready"' | wc -l | tr -d ' ')"
		[ "$READY" = "2" ] && break
		sleep 1
	done
	[ "$READY" = "2" ] || fail "server reports $READY derivative_ready assets, want 2"
	echo "agent-e2e: live derivative upload OK"
fi

echo "agent-e2e: OK"
