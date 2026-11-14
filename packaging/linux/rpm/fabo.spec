# Fabo on a Linux host as an RPM: `fabo` (engine + docker CLI, Compose,
# buildx) or `fabo-lite` (engine only), chosen by packaging/linux/build.sh
# through fabo_name and with_cli. Static (musl) binaries: no runtime
# dependencies, no debuginfo. With with_window, `fabo` also carries the
# window (fabo built against GTK 3 and WebKitGTK 4.1), which needs those.
%global debug_package %{nil}
%global __strip /bin/true
%global __brp_mangle_shebangs %{nil}

Name:           %{fabo_name}
Version:        %{fabo_version}
Release:        1%{?dist}
Summary:        Tiny, fast container runtime (Docker API)%{!?with_cli:, engine only}
License:        Apache-2.0
URL:            https://runkiri.com
Source0:        fabo
Source1:        fabo-init
Source2:        fabo.service
Source3:        LICENSE
Source4:        NOTICE
Source18:       dev.fabo.host-access.policy
%if 0%{?with_go_engine}
Source21:       fabo-daemon
Source22:       fabo-init
Requires:       /usr/bin/getent
%endif
%if 0%{?with_cli}
Source5:        docker
Source6:        docker-compose
Source7:        docker-buildx
Source8:        docker-wrapper
Conflicts:      fabo-lite
%if 0%{?with_window}
Source9:        fabo-window
%if 0%{?with_desktop}
Source20:       fabo-desktop
%endif
Source10:       fabo.desktop
Source12:       fabo-app-icon-16.png
Source13:       fabo-app-icon-32.png
Source14:       fabo-app-icon-64.png
Source15:       fabo-app-icon-128.png
Source16:       fabo-app-icon-256.png
Source17:       fabo-app-icon-512.png
Source19:       com.openfabrica.Fabo.metainfo.xml
Requires:       gtk3
Requires:       webkit2gtk4.1
# The window's setup checklist asks for the password through pkexec.
Recommends:     polkit
%endif
%else
Conflicts:      fabo
%endif
BuildArch:      %{fabo_arch}
# Static binaries: nothing to find by scanning them.
AutoReq:        no
Requires(pre):  shadow-utils
%if 0%{?with_cli}
Requires(post): %{_sbindir}/update-alternatives
Requires(preun): %{_sbindir}/update-alternatives
Requires(posttrans): %{_sbindir}/update-alternatives
%endif

%description
Fabo runs containers on this machine's kernel and speaks the Docker API on
/run/fabo/docker.sock for users in the fabo group (and at
/var/run/docker.sock while no Docker engine has it). No VM, no daemon other
than fabo itself. Members of the fabo group can run containers as root on
this machine; treat it like the docker group.
%if 0%{?with_cli}
This package brings the docker CLI, Compose and buildx with it, as Fabo.app
does on the Mac: `docker run` works right after installing. A docker CLI from
another package keeps precedence on PATH.
%if 0%{?with_window}
Fabo's window is in the app menu, or: fabo ui
%endif
%else
Engine only: point any docker CLI at it with
  docker context create fabo --docker host=unix:///run/fabo/docker.sock
%endif

%prep

%build

%install
install -D -m 755 %{SOURCE0} %{buildroot}%{_bindir}/fabo
install -D -m 755 %{SOURCE1} %{buildroot}%{_bindir}/fabo-init
install -D -m 644 %{SOURCE2} %{buildroot}%{_unitdir}/fabo.service
install -D -m 644 %{SOURCE3} %{buildroot}%{_licensedir}/%{name}/LICENSE
install -D -m 644 %{SOURCE4} %{buildroot}%{_licensedir}/%{name}/NOTICE
install -D -m 644 %{SOURCE18} %{buildroot}%{_datadir}/polkit-1/actions/dev.fabo.host-access.policy
%if 0%{?with_go_engine}
install -D -m 755 %{SOURCE21} %{buildroot}/usr/libexec/fabo/fabo-daemon
install -D -m 755 %{SOURCE22} %{buildroot}/usr/libexec/fabo/fabo-init
%endif
%if 0%{?with_cli}
install -D -m 755 %{SOURCE5} %{buildroot}/usr/libexec/fabo/bin/docker
install -D -m 755 %{SOURCE8} %{buildroot}/usr/libexec/fabo/docker
install -D -m 755 %{SOURCE6} %{buildroot}/usr/libexec/fabo/cli-plugins/docker-compose
install -D -m 755 %{SOURCE7} %{buildroot}/usr/libexec/fabo/cli-plugins/docker-buildx
%endif
%if 0%{?with_window}
install -D -m 755 %{SOURCE9} %{buildroot}/usr/lib/fabo/fabo-window
%if 0%{?with_desktop}
install -D -m 755 %{SOURCE20} %{buildroot}/usr/lib/fabo/fabo-desktop
%endif
install -D -m 644 %{SOURCE10} %{buildroot}%{_datadir}/applications/fabo.desktop
install -D -m 644 %{SOURCE19} %{buildroot}%{_datadir}/metainfo/com.openfabrica.Fabo.metainfo.xml
for s in 16 32 64 128 256 512; do
  install -D -m 644 %{_sourcedir}/fabo-app-icon-$s.png %{buildroot}%{_datadir}/icons/hicolor/${s}x${s}/apps/fabo.png
done
%endif

%pre
getent group fabo >/dev/null || groupadd --system fabo
exit 0

%post
if [ -d /run/systemd/system ]; then
  systemctl daemon-reload >/dev/null 2>&1 || :
  # Not started or enabled: it starts when someone sets Fabo up (the window's
  # Allow access, or `fabo host-access`). An upgrade ($1 >= 2) leaves a
  # running engine alone: it keeps the old binary until it restarts, which
  # stops its containers.
  if [ "$1" -ge 2 ] && systemctl is-active -q fabo.service; then
    echo "Fabo is upgraded. The engine keeps running the old version until it restarts:"
    echo "  sudo systemctl restart fabo   (running containers stop; those with a restart policy start again)"
    echo "or open Fabo and choose Restart engine. The next boot starts the new one either way."
  fi
fi
# A `fabo` before /usr/bin on PATH runs instead of this package's.
for d in /usr/local/sbin /usr/local/bin; do
  if [ -e "$d/fabo" ] && [ "$(readlink -f "$d/fabo")" != /usr/bin/fabo ]; then
    echo "Warning: $d/fabo comes before /usr/bin/fabo on PATH, so it runs instead of this Fabo."
    echo "  If it's an older Fabo, remove it:  sudo rm $d/fabo"
  fi
done
%if 0%{?with_cli}
# Only a /usr/bin/docker that is absent or already an alternatives link; a
# real file, or a symlink another package or the administrator put there, is
# left alone: Fabo's CLI must not take over someone else's docker.
free=1
if [ -e /usr/bin/docker ] || [ -L /usr/bin/docker ]; then
  case "$(readlink /usr/bin/docker 2>/dev/null)" in /etc/alternatives/*) ;; *) free=0 ;; esac
fi
if [ "$free" = 1 ]; then
  %{_sbindir}/update-alternatives --install /usr/bin/docker docker /usr/libexec/fabo/docker 10 || :
  echo "Fabo is installed with its own docker CLI (docker, docker compose, docker buildx)."
else
  echo "Fabo is installed; /usr/bin/docker belongs to something else and stays. Fabo's CLI: /usr/libexec/fabo/docker"
fi
%else
echo "Fabo is installed (engine only). Point a docker CLI at it:"
echo "  docker context create fabo --docker host=unix:///run/fabo/docker.sock && docker context use fabo"
%endif
echo "To use it without sudo:  sudo fabo host-access \$USER   (or open Fabo, which sets it up)"
echo "Members of the fabo group can run containers as root on this machine; treat it like the docker group."
echo "While no Docker engine has /var/run/docker.sock, it leads to Fabo too (for Testcontainers, docker-py and other tools that ignore docker contexts)."
exit 0

%preun
if [ "$1" -eq 0 ]; then
  if [ -d /run/systemd/system ]; then
    systemctl disable --now fabo.service >/dev/null 2>&1 || :
  fi
  # Everything else it runs: a window or fabo command still open.
  for sig in TERM KILL; do
    pkill -$sig -f '^/usr/lib/fabo/fabo-window( |$)' >/dev/null 2>&1 || :
    pkill -$sig -f '^(/usr)?/bin/fabo( |$)' >/dev/null 2>&1 || :
    [ "$sig" = KILL ] || sleep 1
  done
%if 0%{?with_cli}
  %{_sbindir}/update-alternatives --remove docker /usr/libexec/fabo/docker || :
  for p in docker-compose docker-buildx; do
    %{_sbindir}/update-alternatives --remove $p /usr/libexec/fabo/cli-plugins/$p || :
  done
%endif
fi
exit 0

%if 0%{?with_cli}
%posttrans
# Compose and buildx into /usr/lib/docker/cli-plugins where the slot is free
# and Docker's own copy isn't in another of the CLI's plugin folders; else
# Fabo's link goes and Docker's copy is used. In %posttrans because an
# upgrade from a Fabo that put real files there erases them only after %post;
# again whenever another package changes Docker's plugin folders.
mkdir -p /usr/lib/docker/cli-plugins
for p in docker-compose docker-buildx; do
  l=/usr/lib/docker/cli-plugins/$p
  theirs=
  for d in /usr/local/lib/docker/cli-plugins /usr/local/libexec/docker/cli-plugins /usr/libexec/docker/cli-plugins; do
    [ -e $d/$p ] && theirs=1
  done
  if [ -z "$theirs" ] && { [ ! -e $l ] && [ ! -L $l ] || readlink $l 2>/dev/null | grep -q '^/etc/alternatives/'; }; then
    %{_sbindir}/update-alternatives --install $l $p /usr/libexec/fabo/cli-plugins/$p 10 || :
  elif %{_sbindir}/update-alternatives --display $p 2>/dev/null | grep -q "^/usr/libexec/fabo/cli-plugins/$p "; then
    %{_sbindir}/update-alternatives --remove $p /usr/libexec/fabo/cli-plugins/$p || :
  fi
done
exit 0

%filetriggerin -- /usr/lib/docker/cli-plugins /usr/libexec/docker/cli-plugins /usr/local/lib/docker/cli-plugins /usr/local/libexec/docker/cli-plugins
mkdir -p /usr/lib/docker/cli-plugins
for p in docker-compose docker-buildx; do
  l=/usr/lib/docker/cli-plugins/$p
  theirs=
  for d in /usr/local/lib/docker/cli-plugins /usr/local/libexec/docker/cli-plugins /usr/libexec/docker/cli-plugins; do
    [ -e $d/$p ] && theirs=1
  done
  if [ -z "$theirs" ] && { [ ! -e $l ] && [ ! -L $l ] || readlink $l 2>/dev/null | grep -q '^/etc/alternatives/'; }; then
    %{_sbindir}/update-alternatives --install $l $p /usr/libexec/fabo/cli-plugins/$p 10 || :
  elif %{_sbindir}/update-alternatives --display $p 2>/dev/null | grep -q "^/usr/libexec/fabo/cli-plugins/$p "; then
    %{_sbindir}/update-alternatives --remove $p /usr/libexec/fabo/cli-plugins/$p || :
  fi
done
exit 0

%filetriggerpostun -- /usr/lib/docker/cli-plugins /usr/libexec/docker/cli-plugins /usr/local/lib/docker/cli-plugins /usr/local/libexec/docker/cli-plugins
mkdir -p /usr/lib/docker/cli-plugins
for p in docker-compose docker-buildx; do
  l=/usr/lib/docker/cli-plugins/$p
  theirs=
  for d in /usr/local/lib/docker/cli-plugins /usr/local/libexec/docker/cli-plugins /usr/libexec/docker/cli-plugins; do
    [ -e $d/$p ] && theirs=1
  done
  if [ -z "$theirs" ] && { [ ! -e $l ] && [ ! -L $l ] || readlink $l 2>/dev/null | grep -q '^/etc/alternatives/'; }; then
    %{_sbindir}/update-alternatives --install $l $p /usr/libexec/fabo/cli-plugins/$p 10 || :
  elif %{_sbindir}/update-alternatives --display $p 2>/dev/null | grep -q "^/usr/libexec/fabo/cli-plugins/$p "; then
    %{_sbindir}/update-alternatives --remove $p /usr/libexec/fabo/cli-plugins/$p || :
  fi
done
exit 0
%endif

%postun
if [ -d /run/systemd/system ]; then
  systemctl daemon-reload >/dev/null 2>&1 || :
fi
if [ "$1" -eq 0 ]; then
  echo "Fabo is removed; its images, containers and volumes stay in /var/lib/fabo"
fi
exit 0

%files
%license %{_licensedir}/%{name}/LICENSE
%license %{_licensedir}/%{name}/NOTICE
%{_bindir}/fabo
%{_bindir}/fabo-init
%{_unitdir}/fabo.service
%{_datadir}/polkit-1/actions/dev.fabo.host-access.policy
%if 0%{?with_go_engine} || 0%{?with_cli}
%dir /usr/libexec/fabo
%endif
%if 0%{?with_go_engine}
/usr/libexec/fabo/fabo-daemon
/usr/libexec/fabo/fabo-init
%endif
%if 0%{?with_cli}
%dir /usr/libexec/fabo/bin
/usr/libexec/fabo/bin/docker
/usr/libexec/fabo/docker
%dir /usr/libexec/fabo/cli-plugins
/usr/libexec/fabo/cli-plugins/docker-compose
/usr/libexec/fabo/cli-plugins/docker-buildx
%endif
%if 0%{?with_window}
%dir /usr/lib/fabo
/usr/lib/fabo/fabo-window
%if 0%{?with_desktop}
/usr/lib/fabo/fabo-desktop
%endif
%{_datadir}/applications/fabo.desktop
%{_datadir}/icons/hicolor/*/apps/fabo.*
%{_datadir}/metainfo/com.openfabrica.Fabo.metainfo.xml
%endif

%changelog
* Sun Oct 25 2026 Kiri <kiri@canestack.com> - %{fabo_version}-1
- Packaged from the static Linux build
