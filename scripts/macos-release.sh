#!/bin/bash
# Every file a release attaches besides the Windows installers: both DMGs,
# both Linux archives, the kernel source and SHA256SUMS, in target/.
#
#   scripts/macos-release.sh <version>
#
# Faster than `make ... dmg` once per architecture:
# - the kernels and the data disk template are built only when missing
#   (FABO_REBUILD_KERNEL=1 builds them again, as after a kernel change);
# - both DMGs are signed first and then notarized at the same time, so
#   Apple's queue is waited on once rather than twice.
#
# FABO_SHARE_SERVER must name the default share relay: a release without
# it makes every user run `fabo share login --server` first.
# FABO_NOTARY_PROFILE names the notarytool keychain profile.
set -euo pipefail
version=${1:?version}
root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"
: "${FABO_SHARE_SERVER:?set FABO_SHARE_SERVER to the default share relay}"
: "${FABO_NOTARY_PROFILE:?set FABO_NOTARY_PROFILE to a notarytool keychain profile}"
export FABO_SHARE_SERVER

# Guest, kernel and disk per architecture; the kernel build in Docker is the
# slow part, so an existing kernel is kept unless asked otherwise.
for arch in arm64 x86_64; do
  case $arch in arm64) out=guest/out kernel=Image ;; x86_64) out=guest/out/x86_64 kernel=bzImage ;; esac
  make ARCH=$arch guest
  if [ "${FABO_REBUILD_KERNEL:-}" = 1 ] || [ ! -f "$out/$kernel" ]; then
    make ARCH=$arch kernel
  fi
done
[ -f guest/out/data.img ] || make disk

# Apps and signed DMGs, without notarizing yet.
for arch in arm64 x86_64; do
  make ARCH=$arch app
  FABO_NOTARY_PROFILE= scripts/macos-dmg.sh $arch "$version"
done

# Both submissions wait in Apple's queue together.
pids=()
for arch in arm64 x86_64; do
  xcrun notarytool submit "target/Fabo-$version-$arch.dmg" --keychain-profile "$FABO_NOTARY_PROFILE" --wait \
    > "target/notarize-$arch.log" 2>&1 &
  pids+=($!)
done
for i in "${!pids[@]}"; do
  if ! wait "${pids[$i]}"; then
    cat "target/notarize-$([ "$i" = 0 ] && echo arm64 || echo x86_64).log" >&2
    exit 1
  fi
done
# notarytool exits 0 even when Apple rejects the DMG, so the status is checked.
for arch in arm64 x86_64; do
  grep -q "status: Accepted" "target/notarize-$arch.log" || { cat "target/notarize-$arch.log" >&2; exit 1; }
  case $arch in arm64) app=target/Fabo.app ;; x86_64) app=target/intel/Fabo.app ;; esac
  xcrun stapler staple "target/Fabo-$version-$arch.dmg"
  xcrun stapler staple "$app"
done

# Linux archives: the three static binaries, as earlier releases ship them.
for arch in amd64 arm64; do
  rm -rf target/linux/release
  make linux LINUX_ARCH=$arch
  tar -C target/linux/release -czf "target/fabo-$version-linux-$arch.tar.gz" .
done

scripts/ci/kernel-source.sh "target/Fabo-$version-kernel-source.tar.xz"
(cd target && shasum -a 256 "Fabo-$version-arm64.dmg" "Fabo-$version-x86_64.dmg" "Fabo-$version-kernel-source.tar.xz" \
  "fabo-$version-linux-amd64.tar.gz" "fabo-$version-linux-arm64.tar.gz" > SHA256SUMS)
cat target/SHA256SUMS
