#!/bin/sh
# Fabo Linux, the WSL2 distro Fabo runs in on Windows: Alpine's minirootfs
# (pinned, checked), the static Linux engine and contrib/wsl/fabo-linux.
# Runs on macOS or Linux as an ordinary user: engine/cmd/fabo-mkrootfs
# streams Alpine's archive into the new one, so every file stays root's and
# setuid bits survive. x86_64 for x86-64 PCs, aarch64 for Windows on Arm
# (the binaries in <dir> must be the same architecture). The SHA-256s are
# Alpine's own .sha256 files.
#   scripts/fabo-linux.sh <repo> <dir with fabo, fabo-daemon, fabo-init> <out.tar.gz> [x86_64|aarch64]
# All paths absolute: the archive is written by `go -C engine run`.
set -eu
repo=$1 bin=$2 out=$3 arch=${4:-x86_64}
case "$arch" in
  x86_64) ALPINE_SHA256=f8e1cafba9b1b283b5d30d06f9885c8d2bc161ffe9083ade8e2e157813a53407 ;;
  aarch64) ALPINE_SHA256=4d3f80afaebd447c5591a63a73bde380a149fe8b2d85488cafabb2ff97551f50 ;;
  *) echo "arch is x86_64 or aarch64, not $arch" >&2; exit 2 ;;
esac
ALPINE=alpine-minirootfs-3.24.3-$arch.tar.gz
ALPINE_URL=https://dl-cdn.alpinelinux.org/alpine/v3.24/releases/$arch/$ALPINE
cache=${XDG_CACHE_HOME:-$HOME/.cache}/fabo-build
mkdir -p "$cache"

# sha256 prints a file's SHA-256 (sha256sum on Linux, shasum on older macOS).
sha256() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1"; else shasum -a 256 "$1"; fi | cut -d' ' -f1
}

# The .part is this process's own: CI jobs on one machine share the cache.
[ -f "$cache/$ALPINE" ] || { curl -fsSL --retry 3 -o "$cache/$ALPINE.$$.part" "$ALPINE_URL" && mv -f "$cache/$ALPINE.$$.part" "$cache/$ALPINE"; } || rm -f "$cache/$ALPINE.$$.part"
[ "$(sha256 "$cache/$ALPINE")" = "$ALPINE_SHA256" ] || { rm -f "$cache/$ALPINE"; echo "$ALPINE: SHA-256 mismatch" >&2; exit 1; }

wsl="$repo/contrib/wsl/fabo-linux"
go -C "$repo/engine" run ./cmd/fabo-mkrootfs -base "$cache/$ALPINE" -o "$out" \
  -add "usr/local/bin/fabo=$bin/fabo:755" \
  -add "usr/local/bin/fabo-start=$wsl/fabo-start:755" \
  -add "usr/libexec/fabo/fabo-daemon=$bin/fabo-daemon:755" \
  -add "usr/libexec/fabo/fabo-init=$bin/fabo-init:755" \
  -add "etc/wsl.conf=$wsl/wsl.conf:644" \
  -text 'etc/fabo/wsl-engine=auto\n:644'
