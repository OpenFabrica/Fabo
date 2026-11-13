#!/bin/bash
# The macOS app bundle: the launcher, the Go engine and its guest, the
# desktop window, and the docker CLI, Compose and buildx at their pins.
#
#   scripts/macos-app.sh <arm64|x86_64> <version> [flavor]
#
# Without a flavor: target/Kiri.app (target/intel/Kiri.app for x86_64),
# signed for distribution when a Developer ID identity is available.
# With one (dev, go, ...): target/dev/Kiri <Flavor>.app with the bundle
# identifier dev.kiri.app.<flavor>, which gives it its own state directory,
# socket and Finder folder beside the installed app (engine/cmd/kiri/names.go)
# and a labelled band on its icon. A flavored app is never notarized.
#
# Needs the guest first: make guest, make kernel, make disk.
# KIRI_SIGN_IDENTITY picks the signing identity; "-" signs ad hoc.
set -euo pipefail
arch=${1:?arch} version=${2:?version} flavor=${3:-}
root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"

case $arch in
  arm64) goarch=arm64 guest=guest/out kernel=Image out=target cache=target/cli-cache pin= docker_arch=aarch64 ;;
  x86_64) goarch=amd64 guest=guest/out/x86_64 kernel=bzImage out=target/intel cache=target/cli-cache/x86_64 pin=X86_ docker_arch=x86_64 ;;
  *) echo "arch is arm64 or x86_64, not $arch" >&2; exit 2 ;;
esac
name=Kiri id=dev.kiri.app
if [ -n "$flavor" ]; then
  [[ $flavor =~ ^[a-z0-9]+$ ]] || { echo "a flavor is lowercase letters and digits, not $flavor" >&2; exit 2; }
  name="Kiri $(printf %s "${flavor:0:1}" | tr a-z A-Z)${flavor:1}" id=dev.kiri.app.$flavor out=$out/dev
fi
for f in "$guest/$kernel" "$guest/initramfs-go.cpio" guest/out/data.img; do
  [ -f "$f" ] || { echo "no $f: run make guest, make kernel and make disk first" >&2; exit 1; }
done

app="$out/$name.app"
macos="$app/Contents/MacOS" res="$app/Contents/Resources"
rm -rf "$app"
mkdir -p "$macos" "$res/guest" "$res/bin/cli-plugins" "$cache"

# The launcher and the window are pure Go. The engine links Apple's
# Virtualization framework through cgo.
export GOOS=darwin GOARCH=$goarch
CGO_ENABLED=0 go -C engine build -trimpath -o "$root/$macos/kiri" ./cmd/kiri
CGO_ENABLED=0 go -C desktop build -trimpath -o "$root/$macos/kiri-desktop" .
CGO_ENABLED=1 CC="clang -arch $arch" MACOSX_DEPLOYMENT_TARGET=14.0 \
  go -C engine build -trimpath -ldflags "-X main.engineRelease=$version" -o "$root/$macos/kiri-go-daemon" ./cmd/kiri-go-daemon
unset GOOS GOARCH

# In Resources, not loose in Contents: codesign refuses a file there.
cp LICENSE NOTICE "$res/"
cp guest/kernel/SOURCE_OFFER.md "$res/KERNEL_SOURCE_OFFER.md"
cp "$guest/$kernel" "$guest/initramfs-go.cpio" "$res/guest/"
# The disk template is 64 GiB sparse. As a sparse tarball it is a few
# hundred KB, and signing, notarization and downloads never see the 64 GiB.
# The engine unpacks it on first boot.
tar -cSzf "$res/guest/data.img.tgz" -C guest/out data.img

# fetch <pin name> <cached file name> <url>: downloads once, then checks the
# pinned SHA-256 every time. A mismatch deletes the file.
. packaging/cli-pins.env
fetch() {
  local pin=${pin}$1 file=$cache/$2
  read -r _ sum <<<"${!pin}"
  if [ ! -f "$file" ]; then
    curl -fsSL --proto '=https' --proto-redir '=https' --retry 3 -o "$file.part" "$3"
    mv "$file.part" "$file"
  fi
  [ "$(shasum -a 256 "$file" | cut -d' ' -f1)" = "$sum" ] || { rm -f "$file"; echo "$2 does not match its pinned SHA-256" >&2; exit 1; }
}
version_of() { local pin=${pin}$1; read -r v _ <<<"${!pin}"; printf %s "$v"; }
dv=$(version_of DOCKER_CLI) cv=$(version_of COMPOSE) bv=$(version_of BUILDX)
fetch DOCKER_CLI "docker-$dv.tgz" "https://download.docker.com/mac/static/stable/$docker_arch/docker-$dv.tgz"
fetch COMPOSE "docker-compose-darwin-$docker_arch-$cv" "https://github.com/docker/compose/releases/download/$cv/docker-compose-darwin-$docker_arch"
fetch BUILDX "buildx-$bv.darwin-$goarch" "https://github.com/docker/buildx/releases/download/$bv/buildx-$bv.darwin-$goarch"
tar -xzf "$cache/docker-$dv.tgz" -C "$cache" docker/docker
install -m 755 "$cache/docker/docker" "$res/bin/docker"
install -m 755 "$cache/docker-compose-darwin-$docker_arch-$cv" "$res/bin/cli-plugins/docker-compose"
install -m 755 "$cache/buildx-$bv.darwin-$goarch" "$res/bin/cli-plugins/docker-buildx"

# The icon, from the brand PNGs. A flavor's gets its band drawn first.
iconset=target/Kiri.iconset
rm -rf "$iconset"
mkdir -p "$iconset"
if [ -n "$flavor" ]; then
  swift scripts/dev-icon.swift brand/icon/png/kiri-app-icon-1024.png target/kiri-flavor-icon.png "$(printf %s "$flavor" | tr a-z A-Z)"
fi
for size in 16 32 128 256 512; do
  for scale in 1 2; do
    px=$((size * scale)) file=icon_${size}x${size}.png
    [ $scale = 2 ] && file=icon_${size}x${size}@2x.png
    if [ -n "$flavor" ]; then
      sips -z $px $px target/kiri-flavor-icon.png --out "$iconset/$file" >/dev/null
    else
      cp "brand/icon/png/kiri-app-icon-$px.png" "$iconset/$file"
    fi
  done
done
iconutil -c icns -o "$res/Kiri.icns" "$iconset"
rm -rf "$iconset"

# Info.plist's versions are numbers only: 0.1.3-2-g1a2b3c4 is 0.1.3 there.
plain=${version%%-*}
cat > "$app/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleName</key><string>$name</string>
  <key>CFBundleDisplayName</key><string>$name</string>
  <key>CFBundleIdentifier</key><string>$id</string>
  <key>CFBundleExecutable</key><string>kiri</string>
  <key>CFBundleIconFile</key><string>Kiri</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleShortVersionString</key><string>$plain</string>
  <key>CFBundleVersion</key><string>$plain</string>
  <key>LSMinimumSystemVersion</key><string>14.0</string>
  <key>LSApplicationCategoryType</key><string>public.app-category.developer-tools</string>
  <key>NSHighResolutionCapable</key><true/>
  <key>NSLocalNetworkUsageDescription</key><string>Kiri lets containers reach devices and services on your network, and serves the ports they publish.</string>
  <key>NSDesktopFolderUsageDescription</key><string>Kiri shares this folder with containers you mount it into.</string>
  <key>NSDocumentsFolderUsageDescription</key><string>Kiri shares this folder with containers you mount it into.</string>
  <key>NSDownloadsFolderUsageDescription</key><string>Kiri saves files you download from containers here, and shares the folder with containers you mount it into.</string>
  <key>NSRemovableVolumesUsageDescription</key><string>Kiri shares this drive with containers you mount it into.</string>
  <key>NSNetworkVolumesUsageDescription</key><string>Kiri shares this network drive with containers you mount it into.</string>
</dict>
</plist>
PLIST

# KIRI_SIGN_IDENTITY, else the keychain's Developer ID Application identity,
# else ad hoc ("-"), which runs only on this Mac.
identity=${KIRI_SIGN_IDENTITY:-$(security find-identity -v -p codesigning 2>/dev/null | sed -n 's/.*"\(Developer ID Application:[^"]*\)".*/\1/p' | head -1)}
identity=${identity:--}
# The hardened runtime and a secure timestamp are what notarization requires.
# A flavored app is never notarized, and the timestamp asks Apple's server on
# every build, so it gets neither; nor does an ad hoc signature.
sign=(codesign --force --sign "$identity")
if [ "$identity" != - ] && [ -z "$flavor" ]; then
  sign+=(--options runtime --timestamp)
fi
# Inside out: the helpers first, then the engine with the entitlement
# Virtualization.framework requires, then the bundle and its launcher.
for f in bin/docker bin/cli-plugins/docker-compose bin/cli-plugins/docker-buildx; do
  "${sign[@]}" "$res/$f"
done
"${sign[@]}" "$macos/kiri-desktop"
"${sign[@]}" --entitlements packaging/macos/kiri.entitlements "$macos/kiri-go-daemon"
"${sign[@]}" "$app"
codesign --verify --deep --strict "$app"
if [ "$identity" = - ]; then echo "signed: ad hoc"; else echo "signed: $identity"; fi
echo "$app ($(du -sh "$app" | cut -f1))"
