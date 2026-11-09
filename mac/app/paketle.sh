#!/bin/sh
# Builds FootOps.app (panel-tasarim §29.2): the SwiftUI app, the Go core beside it in
# Contents/MacOS, the icon the app draws itself, an ad-hoc signature (for this Mac only:
# nothing is downloaded, so no quarantine and no notarization).
#
#   mac/app/paketle.sh <footops-mac> <sürüm> <hedef dizin>     (make mac)
set -eu

core=$1
version=$2
out=$3
here=$(cd "$(dirname "$0")" && pwd)
id=io.github.swbilisim.footops

swift build -c release --arch arm64 --package-path "$here"
bin=$(swift build -c release --arch arm64 --package-path "$here" --show-bin-path)

app="$out/FootOps.app"
rm -rf "$app"
mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"
cp "$bin/FootOps" "$app/Contents/MacOS/FootOps"
cp "$core" "$app/Contents/MacOS/footops-mac"
sed "s|@VERSION@|$version|" "$here/Info.plist" > "$app/Contents/Info.plist"
plutil -lint -s "$app/Contents/Info.plist"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
"$bin/FootOps" --simge "$tmp/AppIcon.iconset"
iconutil -c icns "$tmp/AppIcon.iconset" -o "$app/Contents/Resources/AppIcon.icns"

# The core first: the app's signature seals what is inside it.
codesign --force --sign - --identifier "$id.footops-mac" "$app/Contents/MacOS/footops-mac"
codesign --force --sign - --identifier "$id" "$app"
codesign --verify --strict "$app"
echo "✓ $app ($version)"
