#!/bin/sh
# Build EAM.app, the bundle that owns eam:// on this Mac.
#
# A URL scheme belongs to a bundle, not to a website: the OS finds a handler by asking
# LaunchServices which installed .app declared the scheme. That is why "the web app
# registers eam://" cannot exist, and why this exists instead — one tiny app whose only
# behaviour is forwarding eam://<handle>/<path> to the drive's https address.
#
# Usage: ./build.sh [output.app]     (default: ./EAM.app next to this script)
set -eu

here=$(cd "$(dirname "$0")" && pwd)
out=${1:-$here/EAM.app}

if ! command -v swiftc >/dev/null 2>&1; then
  echo "swiftc not found: install the Xcode command line tools (xcode-select --install)" >&2
  exit 1
fi

rm -rf "$out"
mkdir -p "$out/Contents/MacOS"
# -O keeps the launch-under-a-second property: a link that takes a second to open feels
# broken even when it works.
# A sandboxed build host cannot write clang's default module cache (~/Library/Caches), so
# put it under TMPDIR: it is scratch either way.
swiftc -O -module-cache-path "${TMPDIR:-/tmp}/eam-swift-module-cache" \
  "$here/main.swift" -o "$out/Contents/MacOS/EAM"
cp "$here/Info.plist" "$out/Contents/Info.plist"

# Ad-hoc signature: enough for LaunchServices to accept a locally built bundle on current
# macOS, and it keeps Gatekeeper from treating the binary as damaged after a copy.
codesign --force --sign - "$out" >/dev/null 2>&1 || true

# Register now so `open eam://...` works without a logout.
lsregister=/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister
[ -x "$lsregister" ] && "$lsregister" -f "$out" >/dev/null 2>&1 || true

echo "built $out"
echo "install: cp -R \"$out\" /Applications/ && open /Applications/$(basename "$out")"
echo "test:    open 'eam://<handle>/'"
