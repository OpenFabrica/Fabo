#!/bin/bash
# The drag-to-install disk image for the app scripts/macos-app.sh built:
# a fixed Finder window with the app on the left, Applications on the right
# and an arrow between them (dmg-background.swift draws it, dmg-settings.py
# places the icons). dmgbuild writes the window's .DS_Store directly, with
# no Finder or AppleScript; it is installed, pinned, in a throwaway
# virtualenv under target/.
#
#   scripts/macos-dmg.sh <arm64|x86_64> <version>
#
# Leaves target/Kiri-<version>-<arch>.dmg, signed. When KIRI_NOTARY_PROFILE
# names credentials stored with `xcrun notarytool store-credentials`, the DMG
# is notarized in one submission and the ticket stapled to the DMG and the
# app, so Gatekeeper opens both offline. Unset or empty skips that.
set -euo pipefail
arch=${1:?arch} version=${2:?version}
root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"
DMGBUILD=1.6.7

case $arch in
  arm64) app=target/Kiri.app volume=Kiri ;;
  x86_64) app=target/intel/Kiri.app volume="Kiri for Intel" ;;
  *) echo "arch is arm64 or x86_64, not $arch" >&2; exit 2 ;;
esac
[ -d "$app" ] || { echo "no $app: run make app first" >&2; exit 1; }

venv=target/dmgbuild-venv-$DMGBUILD
if ! "$venv/bin/dmgbuild" --help >/dev/null 2>&1; then
  rm -rf "$venv"
  python3 -m venv "$venv"
  "$venv/bin/pip" install --quiet --disable-pip-version-check "dmgbuild==$DMGBUILD"
fi
mkdir -p target/dmg-background
swift scripts/dmg-background.swift target/dmg-background/background

dmg=target/Kiri-$version-$arch.dmg
rm -f "$dmg"
"$venv/bin/dmgbuild" -s scripts/dmg-settings.py -D "app=$app" -D "background=target/dmg-background/background.png" "$volume" "$dmg"

identity=${KIRI_SIGN_IDENTITY:-$(security find-identity -v -p codesigning 2>/dev/null | sed -n 's/.*"\(Developer ID Application:[^"]*\)".*/\1/p' | head -1)}
identity=${identity:--}
if [ "$identity" = - ]; then
  codesign --force --sign - "$dmg"
else
  codesign --force --sign "$identity" --options runtime --timestamp "$dmg"
fi

# Sign first, then notarize, then staple: signing again afterwards would
# invalidate the ticket.
if [ -n "${KIRI_NOTARY_PROFILE:-}" ]; then
  xcrun notarytool submit "$dmg" --keychain-profile "$KIRI_NOTARY_PROFILE" --wait
  xcrun stapler staple "$dmg"
  xcrun stapler staple "$app"
  echo "notarized and stapled: $dmg"
fi
echo "$dmg"
