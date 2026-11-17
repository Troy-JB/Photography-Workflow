#!/usr/bin/env bash
# Builds and packages a Windows PWF Agent release.
#
# Inputs (environment):
#   VERSION             release version, e.g. 0.5.0                      (required)
#   CHANNEL             dev | stable                                      (default dev)
#   MANIFEST_URL        public manifest URL; enables online updates
#   PUBLIC_KEY          Ed25519 public key (base64) baked into the build
#   SIGNER_THUMBPRINT   pinned Authenticode signer SHA-1 (required for stable)
#   SIGNTOOL_CERT_SHA1  certificate thumbprint for signtool (stable signing)
#   SIGN_COMMAND        alternative signing command; receives the installer path
#   RELEASE_KEY         private key file for manifest signing (optional)
#   NOTES_FILE          release notes markdown (optional)
#   MIN_VERSION         minimum supported old version (optional, for manifest)
#   MAKENSIS            makensis executable (default: makensis)
#
# Outputs under agent/dist/<version>/:
#   PWF-Agent-Setup-<version>.exe, manifest.json (when RELEASE_KEY is set),
#   SHA256SUMS.txt, stage/ (the staged files)
#
# The stable channel fails closed: without a code-signing certificate the
# build stops, so an unsigned installer can never reach the stable feed.
set -euo pipefail

cd "$(dirname "$0")/.."

VERSION="${VERSION:-}"
CHANNEL="${CHANNEL:-dev}"
MANIFEST_URL="${MANIFEST_URL:-}"
PUBLIC_KEY="${PUBLIC_KEY:-}"
SIGNER_THUMBPRINT="${SIGNER_THUMBPRINT:-}"
MIN_VERSION="${MIN_VERSION:-}"
RELEASE_KEY="${RELEASE_KEY:-}"
NOTES_FILE="${NOTES_FILE:-}"
MAKENSIS="${MAKENSIS:-makensis}"

die() { echo "error: $*" >&2; exit 2; }
warn() { echo "warning: $*" >&2; }

[ -n "$VERSION" ] || die "VERSION is required"
case "$CHANNEL" in
  dev|stable) ;;
  *) die "CHANNEL must be dev or stable (got '$CHANNEL')" ;;
esac
if [ -n "$MANIFEST_URL" ] && [ -z "$PUBLIC_KEY" ]; then
  die "MANIFEST_URL requires PUBLIC_KEY so clients can verify the manifest"
fi
if [ "$CHANNEL" = stable ]; then
  [ -n "$SIGNER_THUMBPRINT" ] || die "stable builds require SIGNER_THUMBPRINT (fail closed)"
  if [ -z "${SIGNTOOL_CERT_SHA1:-}" ] && [ -z "${SIGN_COMMAND:-}" ]; then
    die "stable builds require SIGNTOOL_CERT_SHA1 or SIGN_COMMAND to code-sign the installer"
  fi
fi

LD="-s -w -X photography-workflow/agent/internal/api.Version=${VERSION}"
LD="$LD -X photography-workflow/agent/internal/updater.BuildChannel=${CHANNEL}"
[ -n "$MANIFEST_URL" ] && LD="$LD -X photography-workflow/agent/internal/updater.BuildManifestURL=${MANIFEST_URL}"
[ -n "$PUBLIC_KEY" ] && LD="$LD -X photography-workflow/agent/internal/updater.BuildPublicKey=${PUBLIC_KEY}"
[ -n "$SIGNER_THUMBPRINT" ] && LD="$LD -X photography-workflow/agent/internal/updater.BuildSignerThumbprint=${SIGNER_THUMBPRINT}"

echo "==> building frontend"
(cd frontend && npm install --no-audit --no-fund && npm run build)

echo "==> building GUI $VERSION ($CHANNEL)"
wails build -tags wails -clean -skipbindings -ldflags "$LD"

echo "==> building CLI, launcher and release tool"
CGO_ENABLED=0 go build -trimpath -ldflags "$LD" -o build/bin/pwf-agent.exe ./cmd/pwf-agent
CGO_ENABLED=0 go build -trimpath -ldflags "$LD" -o build/bin/pwf-release.exe ./cmd/pwf-release
CGO_ENABLED=0 go build -trimpath -ldflags "$LD -H=windowsgui" -o build/bin/launcher.exe ./cmd/pwf-launcher

DIST="dist/${VERSION}"
STAGE="${DIST}/stage"
rm -rf "$DIST"
mkdir -p "$STAGE/tools" "$STAGE/licenses"

cp "build/bin/PWF Agent.exe" "$STAGE/"
cp build/bin/pwf-agent.exe "$STAGE/"
cp build/bin/launcher.exe "$STAGE/"

VENDOR_TOOLS="build/vendor/tools"
VENDOR_LICENSES="build/vendor/licenses"
bundled=0
for tool in exiftool ffmpeg; do
  if [ -f "${VENDOR_TOOLS}/${tool}.exe" ]; then
    cp "${VENDOR_TOOLS}/${tool}.exe" "$STAGE/tools/"
    bundled=1
  else
    warn "tools/${tool}.exe is not staged under ${VENDOR_TOOLS}; EXIF/preview support will degrade"
  fi
done
if [ "$bundled" = 1 ]; then
  for license in THIRD-PARTY-NOTICES.txt exiftool-LICENSE.txt ffmpeg-LICENSE.txt; do
    if [ -f "${VENDOR_LICENSES}/${license}" ]; then
      cp "${VENDOR_LICENSES}/${license}" "$STAGE/licenses/"
    else
      warn "missing license file ${VENDOR_LICENSES}/${license} (bundled tools require notices)"
    fi
  done
  [ -f "${STAGE}/licenses/THIRD-PARTY-NOTICES.txt" ] || die "bundled third-party binaries need licenses/THIRD-PARTY-NOTICES.txt"
fi
if [ -f build/vendor/MicrosoftEdgeWebview2Setup.exe ]; then
  cp build/vendor/MicrosoftEdgeWebview2Setup.exe "$STAGE/"
else
  warn "MicrosoftEdgeWebview2Setup.exe is not staged; the installer will ask the operator to install WebView2"
fi

winpath() {
  if command -v cygpath >/dev/null 2>&1; then
    cygpath -w "$1"
  else
    printf '%s' "$1"
  fi
}

echo "==> building installer"
if ! command -v "$MAKENSIS" >/dev/null 2>&1; then
  for candidate in \
    "/c/Program Files (x86)/NSIS/makensis.exe" \
    "/c/Program Files/NSIS/makensis.exe" \
    "C:/Program Files (x86)/NSIS/makensis.exe" \
    "C:/Program Files/NSIS/makensis.exe"; do
    if [ -x "$candidate" ]; then
      MAKENSIS="$candidate"
      break
    fi
  done
fi
command -v "$MAKENSIS" >/dev/null 2>&1 || [ -x "$MAKENSIS" ] || die "$MAKENSIS not found; install NSIS 3 (choco install nsis) and retry"
SETUP_ABS="$PWD/${DIST}/PWF-Agent-Setup-${VERSION}.exe"
NSIS_ARGS=(
  "-DAPP_VERSION=${VERSION}"
  "-DSOURCE_DIR=$(winpath "$PWD/${STAGE}")"
  "-DOUT_FILE=$(winpath "$SETUP_ABS")"
)
if [ -f build/windows/icon.ico ]; then
  NSIS_ARGS+=("-DAPP_ICON=$(winpath "$PWD/build/windows/icon.ico")")
fi
(cd build && "$MAKENSIS" "${NSIS_ARGS[@]}" installer.nsi)
[ -f "$SETUP_ABS" ] || die "installer was not produced at $SETUP_ABS"

if [ "$CHANNEL" = stable ]; then
  echo "==> code-signing installer"
  if [ -n "${SIGN_COMMAND:-}" ]; then
    eval "$SIGN_COMMAND \"$SETUP_ABS\""
  else
    signtool sign /sha1 "${SIGNTOOL_CERT_SHA1}" /fd SHA256 \
      /tr http://timestamp.digicert.com /td SHA256 "$SETUP_ABS"
  fi
  echo "==> verifying Authenticode against the pinned signer"
  ./build/bin/pwf-release.exe check-signature -file "$SETUP_ABS" -thumbprint "$SIGNER_THUMBPRINT"
else
  echo "==> dev build left unsigned (SmartScreen will warn; never publish to stable)"
fi

if [ -n "$RELEASE_KEY" ]; then
  [ -n "$MANIFEST_URL" ] || die "RELEASE_KEY requires MANIFEST_URL to build a manifest"
  echo "==> signing update manifest"
  MANIFEST_ARGS=(
    "-key" "$RELEASE_KEY"
    "-installer" "$SETUP_ABS"
    "-version" "$VERSION"
    "-channel" "$CHANNEL"
    "-url" "$MANIFEST_URL"
    "-out" "${DIST}/manifest.json"
  )
  [ -n "$MIN_VERSION" ] && MANIFEST_ARGS+=("-min-version" "$MIN_VERSION")
  if [ -n "$NOTES_FILE" ]; then
    MANIFEST_ARGS+=("-notes-file" "$NOTES_FILE")
  else
    MANIFEST_ARGS+=("-notes" "PWF Agent ${VERSION}")
  fi
  ./build/bin/pwf-release.exe sign "${MANIFEST_ARGS[@]}"
fi

echo "==> checksums"
(cd "$DIST" && sha256sum "PWF-Agent-Setup-${VERSION}.exe" > SHA256SUMS.txt)
if [ -f "${DIST}/manifest.json" ]; then
  (cd "$DIST" && sha256sum manifest.json >> SHA256SUMS.txt)
fi

echo
echo "artifacts in agent/dist/${VERSION}:"
ls -l "$DIST"
cat "${DIST}/SHA256SUMS.txt"
