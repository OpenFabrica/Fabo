#!/bin/sh
# Run as root in a WSL2 distro, with the other Windows bundle files beside it.
# The engine serves a Unix socket; fabo-win reaches it via fabo bridge.
set -eu
mode=${1:-install}
case "$mode" in install|--sync|--restart) ;; *) echo "usage: install.sh [--sync|--restart]" >&2; exit 2 ;; esac
here=$(cd "$(dirname "$0")" && pwd)
for name in fabo fabo-init fabo-start fabo-daemon fabo-init; do
  [ -f "$here/$name" ] || { echo "Missing WSL bundle file: $name" >&2; exit 1; }
done
install -d /usr/local/bin /usr/libexec/fabo /etc/fabo
# Preserve an explicit choice. Auto checks for an imported or existing store.
[ -e /etc/fabo/wsl-engine ] || printf 'auto\n' > /etc/fabo/wsl-engine
copied=false
copy() {
  if ! cmp -s "$here/$1" "$2/$1"; then
    install -m 755 "$here/$1" "$2/$1.new"
    mv -f "$2/$1.new" "$2/$1"
    copied=true
  fi
}
for name in fabo fabo-init fabo-start; do copy "$name" /usr/local/bin; done
for name in fabo-daemon fabo-init; do copy "$name" /usr/libexec/fabo; done
if [ "$mode" = --sync ]; then
  if "$copied"; then echo copied; fi
  exit 0
fi

if [ -d /run/systemd/system ]; then
  cat > /etc/systemd/system/fabo.service <<UNIT
[Unit]
Description=Fabo container engine
After=network-online.target
[Service]
ExecStart=/usr/local/bin/fabo daemon
Environment=HOME=/var/lib/fabo-daemon
StateDirectory=fabo-daemon
Delegate=yes
KillMode=mixed
Restart=on-failure
UMask=0027
[Install]
WantedBy=multi-user.target
UNIT
  systemctl daemon-reload
  systemctl enable fabo
  systemctl restart fabo
else
  # Fabo Linux has its own startup path. Keep existing boot entries in
  # other distros; they now select the engine through the same launcher.
  if [ "$mode" = install ] && ! grep -Eq 'fabo daemon|/usr/local/bin/fabo-start' /etc/wsl.conf 2>/dev/null; then
    printf '\n[boot]\ncommand = /usr/local/bin/fabo-start\n' >> /etc/wsl.conf
  fi
  /usr/local/bin/fabo-start restart
fi
echo "Fabo runs in WSL on /var/lib/fabo-daemon/.fabo/run/docker.sock"
