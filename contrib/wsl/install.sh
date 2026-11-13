#!/bin/sh
# Run as root in a WSL2 distro, with the other Windows bundle files beside it.
# The engine serves a Unix socket; kiri-win reaches it via kiri bridge.
set -eu
mode=${1:-install}
case "$mode" in install|--sync|--restart) ;; *) echo "usage: install.sh [--sync|--restart]" >&2; exit 2 ;; esac
here=$(cd "$(dirname "$0")" && pwd)
for name in kiri kiri-init kiri-start kiri-go-daemon kiri-go-init; do
  [ -f "$here/$name" ] || { echo "Missing WSL bundle file: $name" >&2; exit 1; }
done
install -d /usr/local/bin /usr/libexec/kiri /etc/kiri
# Preserve an explicit choice. Auto checks for an imported or existing store.
[ -e /etc/kiri/wsl-engine ] || printf 'auto\n' > /etc/kiri/wsl-engine
copied=false
copy() {
  if ! cmp -s "$here/$1" "$2/$1"; then
    install -m 755 "$here/$1" "$2/$1.new"
    mv -f "$2/$1.new" "$2/$1"
    copied=true
  fi
}
for name in kiri kiri-init kiri-start; do copy "$name" /usr/local/bin; done
for name in kiri-go-daemon kiri-go-init; do copy "$name" /usr/libexec/kiri; done
if [ "$mode" = --sync ]; then
  if "$copied"; then echo copied; fi
  exit 0
fi

if [ -d /run/systemd/system ]; then
  cat > /etc/systemd/system/kiri.service <<UNIT
[Unit]
Description=Kiri container engine
After=network-online.target
[Service]
ExecStart=/usr/local/bin/kiri daemon
Environment=HOME=/var/lib/kiri-daemon
StateDirectory=kiri-daemon
Delegate=yes
KillMode=mixed
Restart=on-failure
UMask=0027
[Install]
WantedBy=multi-user.target
UNIT
  systemctl daemon-reload
  systemctl enable kiri
  systemctl restart kiri
else
  # Kiri Linux has its own startup path. Keep existing boot entries in
  # other distros; they now select the engine through the same launcher.
  if [ "$mode" = install ] && ! grep -Eq 'kiri daemon|/usr/local/bin/kiri-start' /etc/wsl.conf 2>/dev/null; then
    printf '\n[boot]\ncommand = /usr/local/bin/kiri-start\n' >> /etc/wsl.conf
  fi
  /usr/local/bin/kiri-start restart
fi
echo "Kiri runs in WSL on /var/lib/kiri-daemon/.kiri/run/docker.sock"
