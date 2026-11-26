#!/usr/bin/env bash
# Builds PGBot.app: the SwiftUI app plus the pgbot Go engine, bundled together.
#
#   desktop/macos/scripts/build-app.sh            # release build → desktop/macos/build/PGBot.app
#   CONFIG=debug desktop/macos/scripts/build-app.sh
#   PGBOT_AUTH_BROKER_URL=https://auth.pgbot.dev desktop/macos/scripts/build-app.sh
#
# The engine is built from this repository (same flags as the Makefile) for
# arm64 and amd64 and merged with lipo, so the bundle runs on Apple silicon and
# Intel. The Swift app is built universal when the toolchain supports it (full
# Xcode); Command Line Tools build the host architecture only.
set -euo pipefail

HERE="$(cd "$(dirname "$0")/.." && pwd)"
REPO="$(cd "$HERE/../.." && pwd)"
CONFIG="${CONFIG:-release}"
OUT="$HERE/build"
APP="$OUT/PGBot.app"
VERSION="${VERSION:-$(git -C "$REPO" describe --tags --always 2>/dev/null | sed 's/^v//' || echo dev)}"
SHORT_VERSION="$(echo "$VERSION" | sed -E 's/^([0-9]+\.[0-9]+\.[0-9]+).*/\1/')"
[[ "$SHORT_VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || SHORT_VERSION="0.0.0"

echo "→ Building engine $VERSION (arm64 + amd64)"
ENGINE_TMP="$(mktemp -d)"
trap 'rm -rf "$ENGINE_TMP"' EXIT
for arch in arm64 amd64; do
  (cd "$REPO" && CGO_ENABLED=0 GOOS=darwin GOARCH=$arch go build -trimpath \
     -ldflags "-s -w -X main.version=$VERSION" -o "$ENGINE_TMP/pgbot-$arch" ./cmd/pgbot)
done
lipo -create -output "$ENGINE_TMP/pgbot" "$ENGINE_TMP/pgbot-arm64" "$ENGINE_TMP/pgbot-amd64"

echo "→ Building app ($CONFIG)"
cd "$HERE"
if swift build -c "$CONFIG" --product PGBot --arch arm64 --arch x86_64 >/dev/null 2>&1; then
  BIN_DIR="$(swift build -c "$CONFIG" --product PGBot --arch arm64 --arch x86_64 --show-bin-path)"
else
  echo "  (universal Swift build unavailable with this toolchain; building $(uname -m) only)"
  if ! LOG="$(swift build -c "$CONFIG" --product PGBot 2>&1)"; then
    echo "$LOG" | grep -E "error:" | grep -v "xcrun: error" >&2; exit 1
  fi
  BIN_DIR="$(swift build -c "$CONFIG" --product PGBot --show-bin-path)"
fi

echo "→ Assembling $APP"
rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources/engine"
cp "$BIN_DIR/PGBot" "$APP/Contents/MacOS/PGBot"
cp "$ENGINE_TMP/pgbot" "$APP/Contents/Resources/engine/pgbot"
chmod 755 "$APP/Contents/Resources/engine/pgbot"
cp "$REPO/internal/monitor/assets/PgBot.icns" "$APP/Contents/Resources/AppIcon.icns"

cat > "$APP/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleIdentifier</key><string>dev.pgbot.desktop</string>
  <key>CFBundleName</key><string>PGBot</string>
  <key>CFBundleDisplayName</key><string>PGBot</string>
  <key>CFBundleExecutable</key><string>PGBot</string>
  <key>CFBundleIconFile</key><string>AppIcon</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleShortVersionString</key><string>$SHORT_VERSION</string>
  <key>CFBundleVersion</key><string>$VERSION</string>
  <key>LSMinimumSystemVersion</key><string>14.0</string>
  <key>LSApplicationCategoryType</key><string>public.app-category.developer-tools</string>
  <key>NSHighResolutionCapable</key><true/>
  <key>NSPrincipalClass</key><string>NSApplication</string>
  <key>NSHumanReadableCopyright</key><string>Apache-2.0 · pgbot.dev</string>
  <key>CFBundleURLTypes</key>
  <array><dict>
    <key>CFBundleURLName</key><string>dev.pgbot.desktop.auth</string>
    <key>CFBundleURLSchemes</key><array><string>pgbot</string></array>
  </dict></array>
  <key>PGBotAuthBrokerURL</key><string>${PGBOT_AUTH_BROKER_URL:-}</string>
</dict>
</plist>
PLIST

echo "→ Signing (ad hoc; use a Developer ID identity for distribution)"
codesign --force --options runtime --sign "${CODESIGN_IDENTITY:--}" "$APP/Contents/Resources/engine/pgbot"
codesign --force --options runtime --sign "${CODESIGN_IDENTITY:--}" "$APP"
codesign --verify --deep --strict "$APP"

echo "✓ $APP"
echo "  engine: $("$APP/Contents/Resources/engine/pgbot" --version) ($(lipo -archs "$APP/Contents/Resources/engine/pgbot"))"
echo "  app:    $(lipo -archs "$APP/Contents/MacOS/PGBot")"
