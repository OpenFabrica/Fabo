#!/usr/bin/env bash
# Fabo for Windows: the folder the MSI installs into %LOCALAPPDATA%\Fabo,
# built on macOS or Linux (only `wix build` needs Windows: see
# scripts/windows-msi.ps1 and docs/windows.md).
#
#   scripts/windows-bundle.sh <x86_64|arm64> <version>
#
# Makes target/windows/fabo-windows-<arch>/:
#   fabo-win.exe       the host: Docker API pipes, WSL bridge, setup, updates
#   fabo-desktop.exe   the window and tray, with ghostty-vt.dll beside it
#   bin\fabo.exe, bin\docker.exe, bin\cli-plugins\docker-{compose,buildx}.exe
#   fabo-linux.tar.gz  Fabo Linux, the WSL distro (scripts/fabo-linux.sh)
#   fabo, fabo-daemon, fabo-init, fabo-start, install.sh  the engine for
#                      another distro (Ubuntu) and for updates
#   fabo.ps1, setup-tools.ps1, fabo-admin.ps1, LICENSE, NOTICE
# fabo-win.exe is built last, with the SHA-256 of every file it imports,
# copies or runs, so setup refuses a file changed in the install folder.
# FABO_UPDATE_URL sets the update feed fabo-win reads (none when empty);
# FABO_SHARE_SERVER the default share relay, as for the other builds.
set -euo pipefail

arch=${1:?usage: windows-bundle.sh <x86_64|arm64> <version>}
version=${2:?usage: windows-bundle.sh <x86_64|arm64> <version>}
root=$(cd "$(dirname "$0")/.." && pwd)
case "$arch" in
  x86_64) goarch=amd64 alpine=x86_64 pin=WIN_ ;;
  arm64) goarch=arm64 alpine=aarch64 pin=WIN_ARM64_ ;;
  *) echo "arch is x86_64 or arm64, not $arch" >&2; exit 2 ;;
esac
out=$root/target/windows/fabo-windows-$arch
linux=$root/target/windows/linux-$arch
cache=$root/target/cli-cache
rm -rf "$out"
mkdir -p "$out/bin/cli-plugins" "$linux" "$cache"
# shellcheck source=../packaging/cli-pins.env
source "$root/packaging/cli-pins.env"

share=""
[ -n "${FABO_SHARE_SERVER:-}" ] && share="-X github.com/OpenFabrica/Fabo/engine/sharetunnel.DefaultServer=$FABO_SHARE_SERVER"

# sha256 prints a file's SHA-256.
sha256() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1"; else shasum -a 256 "$1"; fi | cut -d' ' -f1
}

# fetch NAME FILE URL downloads URL into the cache once and checks it
# against the pin NAME ("<version> <sha256>") from packaging/cli-pins.env.
fetch() {
  local var=$1 file=$cache/$2 url=$3 sum
  read -r _ sum <<<"${!var}"
  if [ ! -f "$file" ]; then
    curl -fsSL --proto '=https' --proto-redir '=https' --retry 3 -o "$file.$$.part" "$url"
    mv -f "$file.$$.part" "$file"
  fi
  [ "$(sha256 "$file")" = "$sum" ] || { rm -f "$file"; echo "$2 does not match its pinned SHA-256" >&2; exit 1; }
}
version_of() { local var=$1 v; read -r v _ <<<"${!var}"; printf %s "$v"; }

# The .syso resource objects are written into the package directories
# only for their builds; none may be left behind.
sysos=()
cleanup() { [ ${#sysos[@]} -eq 0 ] || rm -f "${sysos[@]}"; }
trap cleanup EXIT

# winbuild DIR PKG OUT NAME DESC GUI LDFLAGS: a Windows exe with Fabo's
# icon and version resource.
winbuild() {
  local module=$1 pkg=$2 dest=$3 name=$4 desc=$5 gui=$6 ldflags=$7
  local syso=$root/$module/${pkg#./}/rsrc_windows_$goarch.syso
  [ "$pkg" = . ] && syso=$root/$module/rsrc_windows_$goarch.syso
  sysos+=("$syso")
  go -C "$root/scripts/winres" run . -icon "$root/brand/icon/windows/fabo.ico" -version "$version" \
    -name "$name" -desc "$desc" -arch "$goarch" -o "$syso"
  [ "$gui" = gui ] && ldflags="$ldflags -H windowsgui"
  GOOS=windows GOARCH=$goarch CGO_ENABLED=0 go -C "$root/$module" build -trimpath -ldflags "-s -w $ldflags" -o "$dest" "$pkg"
  rm -f "$syso"
}

echo "== the Linux engine ($alpine)"
for cmd in fabo fabo-daemon fabo-init; do
  flags="-s -w"
  case $cmd in
    fabo) flags="$flags -X main.release=$version" ;;
    fabo-daemon) flags="$flags -X main.engineRelease=$version $share" ;;
  esac
  GOOS=linux GOARCH=$goarch CGO_ENABLED=0 go -C "$root/engine" build -trimpath -ldflags "$flags" -o "$linux/$cmd" "./cmd/$cmd"
done
"$root/scripts/fabo-linux.sh" "$root" "$linux" "$out/fabo-linux.tar.gz" "$alpine"
cp "$linux/fabo" "$linux/fabo-daemon" "$linux/fabo-init" "$out/"
cp "$root/contrib/wsl/install.sh" "$root/contrib/wsl/fabo-linux/fabo-start" "$out/"
cp "$root/contrib/windows/fabo.ps1" "$root/contrib/windows/setup-tools.ps1" "$root/contrib/windows/fabo-admin.ps1" "$out/"
cp "$root/LICENSE" "$root/NOTICE" "$out/"

echo "== docker CLI, Compose, buildx"
cv=$(version_of "${pin}COMPOSE") bv=$(version_of "${pin}BUILDX") gv=$(version_of "${pin}GHOSTTY_VT")
if [ "$arch" = x86_64 ]; then
  dv=$(version_of WIN_DOCKER_CLI)
  fetch WIN_DOCKER_CLI "docker-$dv-windows.zip" "https://download.docker.com/win/static/stable/x86_64/docker-$dv.zip"
  unzip -p "$cache/docker-$dv-windows.zip" docker/docker.exe > "$out/bin/docker.exe"
  fetch WIN_COMPOSE "docker-compose-windows-x86_64-$cv.exe" "https://github.com/docker/compose/releases/download/$cv/docker-compose-windows-x86_64.exe"
  fetch WIN_BUILDX "buildx-$bv.windows-amd64.exe" "https://github.com/docker/buildx/releases/download/$bv/buildx-$bv.windows-amd64.exe"
  cp "$cache/docker-compose-windows-x86_64-$cv.exe" "$out/bin/cli-plugins/docker-compose.exe"
  cp "$cache/buildx-$bv.windows-amd64.exe" "$out/bin/cli-plugins/docker-buildx.exe"
  fetch WIN_GHOSTTY_VT "ghostty-vt-windows-amd64-$gv.dll" "https://github.com/egoist/mygo/releases/download/libghostty-vt-$gv/libghostty-vt-windows-amd64.dll"
  cp "$cache/ghostty-vt-windows-amd64-$gv.dll" "$out/ghostty-vt.dll"
else
  # Docker publishes no arm64 docker.exe: it is built from docker/cli's
  # release commit with the pinned Go, reproducibly, and must hash to the pin.
  dv=$WIN_ARM64_DOCKER_SRC_VERSION
  exe=$cache/docker-$dv-windows-arm64.exe
  if [ ! -f "$exe" ] || [ "$(sha256 "$exe")" != "$WIN_ARM64_DOCKER_SRC_SHA256" ]; then
    src=$cache/docker-cli-$dv
    [ -d "$src/.git" ] || git -c advice.detachedHead=false clone -q --config core.autocrlf=false --depth 1 --branch "v$dv" https://github.com/docker/cli "$src"
    [ "$(git -C "$src" rev-parse HEAD)" = "$WIN_ARM64_DOCKER_SRC_COMMIT" ] || { echo "$src is not docker/cli v$dv's commit; delete it and run again" >&2; exit 1; }
    cp "$src/vendor.mod" "$src/go.mod"
    cp "$src/vendor.sum" "$src/go.sum"
    (cd "$src" && GOTOOLCHAIN=$WIN_ARM64_DOCKER_SRC_GO GOOS=windows GOARCH=arm64 GOARM64=v8.0 CGO_ENABLED=0 GO111MODULE=on GOFLAGS=-mod=vendor GOWORK=off GOEXPERIMENT= \
      go build -trimpath -buildvcs=false -tags grpcnotrace \
      -ldflags "-buildid= -X \"github.com/docker/cli/cli/version.GitCommit=${WIN_ARM64_DOCKER_SRC_COMMIT:0:7}\" -X \"github.com/docker/cli/cli/version.BuildTime=$WIN_ARM64_DOCKER_SRC_TIME\" -X \"github.com/docker/cli/cli/version.Version=$dv\"" \
      -o "$exe.part" ./cmd/docker)
    [ "$(sha256 "$exe.part")" = "$WIN_ARM64_DOCKER_SRC_SHA256" ] || { rm -f "$exe.part"; echo "docker.exe for arm64 does not match its pinned SHA-256" >&2; exit 1; }
    mv -f "$exe.part" "$exe"
  fi
  cp "$exe" "$out/bin/docker.exe"
  fetch WIN_ARM64_COMPOSE "docker-compose-windows-aarch64-$cv.exe" "https://github.com/docker/compose/releases/download/$cv/docker-compose-windows-aarch64.exe"
  fetch WIN_ARM64_BUILDX "buildx-$bv.windows-arm64.exe" "https://github.com/docker/buildx/releases/download/$bv/buildx-$bv.windows-arm64.exe"
  cp "$cache/docker-compose-windows-aarch64-$cv.exe" "$out/bin/cli-plugins/docker-compose.exe"
  cp "$cache/buildx-$bv.windows-arm64.exe" "$out/bin/cli-plugins/docker-buildx.exe"
  fetch WIN_ARM64_GHOSTTY_VT "ghostty-vt-windows-arm64-$gv.dll" "https://github.com/egoist/mygo/releases/download/libghostty-vt-$gv/libghostty-vt-windows-arm64.dll"
  cp "$cache/ghostty-vt-windows-arm64-$gv.dll" "$out/ghostty-vt.dll"
fi

echo "== fabo.exe, fabo-desktop.exe"
winbuild engine ./cmd/fabo "$out/bin/fabo.exe" fabo "Fabo command" console "-X main.release=$version"
winbuild desktop . "$out/fabo-desktop.exe" fabo-desktop "Fabo" gui "-X main.release=$version"

echo "== fabo-win.exe"
manifest=""
for f in fabo fabo-daemon fabo-init fabo-start fabo-linux.tar.gz install.sh fabo.ps1 setup-tools.ps1 fabo-admin.ps1 \
  bin/docker.exe bin/fabo.exe bin/cli-plugins/docker-compose.exe bin/cli-plugins/docker-buildx.exe; do
  manifest="$manifest${manifest:+;}$f=$(sha256 "$out/$f")"
done
winbuild engine ./cmd/fabo-win "$out/fabo-win.exe" fabo-win "Fabo" gui \
  "-X main.release=$version -X main.releaseBuild=1 -X main.manifest=$manifest -X main.updateURL=${FABO_UPDATE_URL:-}"

size=$(du -sk "$out" | cut -f1)
echo "$out ($((size / 1024)) MB)"
