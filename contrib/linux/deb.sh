#!/bin/sh
# Builds target/fabo_<version>_<arch>.deb on a Debian or Ubuntu machine:
# the engine and CLI (/usr/bin/fabo, fabo-init), the fabo system service,
# and the window (/usr/lib/fabo/fabo-window) with its launcher and icon.
#
# Needs: Rust, dpkg-deb, and libwebkit2gtk-4.1-dev for the window. Run it
# on the oldest release the package should install on: the window links
# that release's glibc, GTK and WebKitGTK.
set -eu
cd "$(dirname "$0")/../.."
root=$(pwd)
version=$(sed -n 's/^version = "\(.*\)"/\1/p' Cargo.toml | head -1)
arch=$(dpkg --print-architecture)

cargo build --release -p fabo -p fabo-init
# Its own target dir: the same crate with another feature set would
# otherwise rebuild over the engine's binary each time.
CARGO_TARGET_DIR=target/window cargo build --release -p fabo --features window

pkg=target/deb/fabo_${version}_${arch}
rm -rf "$pkg"
install -d "$pkg/DEBIAN" "$pkg/usr/bin" "$pkg/usr/lib/fabo" "$pkg/usr/lib/systemd/system" "$pkg/usr/share/applications"
install -m 755 target/release/fabo target/release/fabo-init "$pkg/usr/bin/"
install -m 755 target/window/release/fabo "$pkg/usr/lib/fabo/fabo-window"
sed 's|^ExecStart=/usr/local/bin/fabo daemon|ExecStart=/usr/bin/fabo daemon|' contrib/linux/fabo.service >"$pkg/usr/lib/systemd/system/fabo.service"
install -m 644 contrib/linux/fabo.desktop "$pkg/usr/share/applications/fabo.desktop"
for s in 16 32 64 128 256 512; do
    install -D -m 644 "brand/icon/png/fabo-app-icon-$s.png" "$pkg/usr/share/icons/hicolor/${s}x${s}/apps/fabo.png"
done
install -m 755 contrib/linux/deb/postinst contrib/linux/deb/prerm contrib/linux/deb/postrm "$pkg/DEBIAN/"

size=$(du -sk "$pkg/usr" | cut -f1)
cat >"$pkg/DEBIAN/control" <<EOF
Package: fabo
Version: $version
Architecture: $arch
Maintainer: Fabo
Homepage: https://runkiri.com
Section: admin
Priority: optional
Installed-Size: $size
Depends: libc6, libgtk-3-0t64 | libgtk-3-0, libwebkit2gtk-4.1-0, docker.io | docker-ce-cli
Recommends: docker-buildx | docker-buildx-plugin, docker-compose-v2 | docker-compose-plugin
Description: Run, build and share containers
 Fabo is a small container engine with its own window. Plain docker
 commands work against it through the "fabo" docker context, and the
 window shows containers, images, volumes, builds and Kubernetes, and
 shares a container on a public https link.
EOF
dpkg-deb --root-owner-group --build "$pkg" "target/fabo_${version}_${arch}.deb"
echo "$root/target/fabo_${version}_${arch}.deb"
