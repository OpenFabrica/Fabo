# Development

Building Fabo from source, running it, and how it fits together. The
binaries, Docker context and state directories carry Fabo's names.

## Build

Needs Go 1.27.3 or newer and, on a Mac, Xcode's command line tools. The
kernel and the disk template also need a Docker engine. `make help` lists
every task.

```sh
make check     # vet and test engine and desktop; compile the Linux-only tests
make guest     # guest/out/fabo-init and guest/out/initramfs-go.cpio
make kernel    # guest/out/Image
make disk      # guest/out/data.img (sparse 64 GiB ext4 template)
make app       # target/Fabo.app
make app-dev   # target/dev/Fabo Dev.app, a second installation with its own state
make dev       # run the desktop and reload it on source changes
make dmg       # target/Fabo-<version>-arm64.dmg from the app
make linux     # target/linux/release: fabo, fabo-daemon, fabo-init
```

For Intel Macs add `ARCH=x86_64` to `guest`, `kernel`, `daemon`, `app` and
`dmg`: an x86-64 guest in `guest/out/x86_64` (a `bzImage`, cross-compiled on
Apple Silicon) and `target/intel/Fabo.app` with Intel builds of the docker
CLI, Compose and buildx. Each Mac gets its own app rather than one universal
app, which would carry both builds of the docker tools. On Intel there is no
Rosetta for Linux: amd64 images run natively, and images with no amd64 build
are refused.

The app is signed with `FABO_SIGN_IDENTITY`, else the keychain's Developer ID
Application identity, else ad hoc. `FABO_SIGN_IDENTITY=-` forces an ad hoc
signature, which runs only on this Mac. `make dmg` notarizes the disk image
and staples the ticket when `FABO_NOTARY_PROFILE` names a `notarytool`
keychain profile.

The docker CLI, Compose and buildx versions and their SHA-256s are pinned
once, in `packaging/cli-pins.env`.

## Use

Run `make dev` for desktop UI changes. MyGo rebuilds and relaunches the
window when you save Go source or embedded translations. A failed build
leaves the previous window running. Press Ctrl+C to stop the session.
On macOS, with `/Applications/Fabo.app` installed, the window borrows the
installed app as a `make app-dev` bundle does: its engine at
`~/.fabo/run/docker.sock`, its launcher, and the files the window keeps
in `~/.fabo` (`desktop.json`, `debug-sessions.json`, `logs-cleared.json`,
`k8s.json`). Set `FABO_SOCKET` for another installation, and
`FABO_LAUNCHER` with it when the window should run that installation's
`fabo`; with a socket of your own, `make dev` sets no launcher.

Use `make app-dev` to build a full application bundle. Its bundle identifier,
`com.openfabrica.fabo.dev`, gives it `~/.fabo-dev` and `~/Fabo Dev` instead of the
installed app's `~/.fabo` and `~/Fabo`, and it leaves the default domain
ports alone. `FLAVOR=name` builds `Fabo <Name>.app` with `~/.fabo-<name>`.

```sh
FABO_SIGN_IDENTITY=- make app-dev
open "target/dev/Fabo Dev.app"                 # sets up, starts the engine, opens the window
docker --context fabo-dev run --rm alpine echo hello
SOAK=600 DOCKER_CONTEXT=fabo-dev tests/e2e.sh  # every check, then 600 runs watching for leaks
"target/dev/Fabo Dev.app/Contents/MacOS/fabo" setup --uninstall   # takes the setup back
```

The desktop's containers page can be checked against a running engine,
as a person would use it: listing, logs, files, terminal, a Compose
project's panel, and, for the project named, Save and apply and a stop
and start of all its containers. Without `FABO_LIVE_PROJECT` it only reads.

```sh
FABO_LIVE_SOCKET=~/.fabo-dev/run/docker.sock FABO_LIVE_PROJECT=demo \
  FABO_SHOTS=/tmp/shots go -C desktop test -run TestLiveEngine -v -timeout 10m .
```

Opening the app from Finder sets the Mac up the first time, as `fabo setup`
does: a docker context for the installation, `fabo` and `docker` in its bin
directory, a LaunchAgent that starts the engine at login, and a buildx
builder that loads what it builds into the engine. The installed app
(`com.openfabrica.fabo`) also puts `~/.fabo/bin` on PATH, links `/usr/local/bin/fabo`,
adds its Compose and buildx to docker's plugin path, makes its context
docker's default when there is no other engine, and points Testcontainers
at the engine. A flavored app leaves all of those alone. `fabo setup
--uninstall` takes back exactly what setup did and stops the engine; the
data stays. `DOCKER_CONFIG` redirects everything setup writes for docker,
which keeps a trial run out of `~/.docker`.

The launcher's commands: `fabo` or `fabo ui` opens the window, and starts
the engine first when nothing answers on the socket. `fabo daemon` runs the
engine in the foreground. `fabo setup` and `fabo build-with` are above.
`fabo migrate` imports an offline Kiri store, as
[desktop-overview.md](desktop-overview.md) describes. `fabo run`, `fabo ps`, `fabo exec`, `fabo compose` and the other Docker
commands use this installation's socket regardless of the selected Docker
context. The engine must already be running. Arguments, terminal I/O and
exit status pass directly to the Docker CLI on PATH (or the bundled CLI).
An explicit `BUILDX_BUILDER` is preserved; otherwise a registered builder
for this installation is selected. On macOS, use `fabo container update`
for container resources: `fabo update` is reserved for the app updater.
`fabo machine` creates persistent Linux development containers with the host
user and shared homes directory. It supports create, list, shell, start, stop,
restart and delete, including `--systemd`; see [machines.md](machines.md).
`fabo k8s`, `import`, `usb`, `bench` and `raw` are implemented in Go.
See [CLI tools](cli-tools.md) for command contracts, state ownership, platform
boundaries and verification.

The engine never writes to a Kiri store, and a Kiri store on the machine does
not stop Fabo from starting. Run `fabo migrate` before Fabo's first start to
bring one over.

To run the daemon without a bundle, with private directories of your own,
follow "Go VM on macOS" in [desktop-overview.md](desktop-overview.md).

## Architecture

```
docker CLI ─┐
compose ────┼─ ~/.fabo/run/docker.sock ─► fabo-daemon ── vsock ──► fabo-init (PID 1)
buildx ─────┤     Docker Engine API         owns the VM              clone3 → container
MyGo window ┘
```

- **VM**: Apple's Virtualization.framework, bound through cgo
  (`engine/cmd/fabo-daemon/virtualization_darwin.m`). A custom 10 MB
  kernel (`guest/kernel/`) built from allnoconfig, with one patch that cuts
  virtio-pci reset waits from a scheduler tick to microseconds. Rosetta is
  shared in over virtiofs and registered with binfmt_misc.
- **Guest agent**: a static Go binary that is PID 1 in the VM. On Linux and
  WSL the same agent runs as a host process, and the daemon talks to it over
  a Unix socket instead of vsock.
- **Containers**: one `clone3` straight into new namespaces and the
  container's cgroup, overlayfs over layers unpacked on an ext4 data disk,
  `pivot_root`, capabilities and a seccomp filter. Networking uses netlink
  and nftables directly. No runc, no shim, no dockerd.
- **Images**: pulled and pushed by the daemon, streamed into the guest once
  and unpacked there.
- **Protocol**: Fabo's host ↔ guest protocol, version 0.6
  (`engine/internal/guestwire`).

## Layout

| Path | What |
|------|------|
| `engine/cmd/fabo` | the launcher: starts the engine with the installation's paths, opens the window. On Windows (`*_windows.go`), `bin\fabo.exe`, the `fabo` command |
| `engine/cmd/fabo-win` | the Windows host: Docker API pipes, WSL bridge, first-run setup, updates (docs/windows.md) |
| `engine/cmd/fabo-mkrootfs` | builds Fabo Linux, the WSL distro, from Alpine's minirootfs |
| `engine/internal/wslbridge` | the multiplexed Windows ↔ WSL bridge protocol, both ends |
| `engine/internal/winhost` | what the Windows programs share: paths, setup.json, pipes, the bundle manifest |
| `engine/cmd/fabo-daemon` | the daemon: Docker API, VM or host-process agent, Finder mount, USB |
| `engine/cmd/fabo-init` | the guest agent's entry point, PID 1 in the VM |
| `engine/cmd/fabo-mkinitramfs` | packs the agent into the initramfs the VM boots |
| `engine/internal/localengine` | the Docker API and everything the daemon persists |
| `engine/internal/guestagent` | the container runtime, Linux only |
| `engine/internal/guestwire` | host ↔ guest wire protocol |
| `desktop/` | the MyGo window and tray (`fabo-desktop`) |
| `guest/kernel` | kernel config and patches |
| `scripts/macos-app.sh`, `scripts/macos-dmg.sh` | the app bundle and its disk image |
| `packaging/` | `cli-pins.env`, the macOS entitlement, Linux `.deb` and `.rpm` |
| `tests/e2e.sh` | about 500 checks through the real `docker` CLI, plus a leak soak (`K8S=1` adds Kubernetes) |
| `tests/linux-host.sh` | the Linux build, driven in a privileged container |
| `contrib/linux`, `packaging/linux` | upstream's systemd and Linux package scripts, not yet adapted to the Go binaries |
| `contrib/windows`, `contrib/wsl`, `installer/windows` | the Windows host's scripts and MSI source, for the Go binaries (docs/windows.md) |
| `bench/results.json` | upstream Kiri's measured results |
| `brand/` | icons and logos |
