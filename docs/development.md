# Development

Building FabriBay from source, running it, and how it fits together. The
binaries, Docker context and state directories still carry Kiri's names.

## Build

Needs Go 1.27.3 or newer and, on a Mac, Xcode's command line tools. The
kernel and the disk template also need a Docker engine. `make help` lists
every task.

```sh
make check     # vet and test engine and desktop; compile the Linux-only tests
make guest     # guest/out/kiri-go-init and guest/out/initramfs-go.cpio
make kernel    # guest/out/Image
make disk      # guest/out/data.img (sparse 64 GiB ext4 template)
make app       # target/Fabo.app
make app-dev   # target/dev/Fabo Dev.app, a second installation with its own state
make dmg       # target/Fabo-<version>-arm64.dmg from the app
make linux     # target/linux/release: fabo, kiri-go-daemon, kiri-go-init
```

For Intel Macs add `ARCH=x86_64` to `guest`, `kernel`, `daemon`, `app` and
`dmg`: an x86-64 guest in `guest/out/x86_64` (a `bzImage`, cross-compiled on
Apple Silicon) and `target/intel/Fabo.app` with Intel builds of the docker
CLI, Compose and buildx. Each Mac gets its own app rather than one universal
app, which would carry both builds of the docker tools. On Intel there is no
Rosetta for Linux: amd64 images run natively, and images with no amd64 build
are refused.

The app is signed with `KIRI_SIGN_IDENTITY`, else the keychain's Developer ID
Application identity, else ad hoc. `KIRI_SIGN_IDENTITY=-` forces an ad hoc
signature, which runs only on this Mac. `make dmg` notarizes the disk image
and staples the ticket when `KIRI_NOTARY_PROFILE` names a `notarytool`
keychain profile.

The docker CLI, Compose and buildx versions and their SHA-256s are pinned
once, in `packaging/cli-pins.env`.

## Use

`make app-dev` is the build to try changes with. Its bundle identifier,
`dev.kiri.app.dev`, gives it `~/.kiri-dev` and `~/Kiri Dev` instead of the
installed app's `~/.kiri` and `~/Kiri`, and it leaves the default domain
ports alone. `FLAVOR=name` builds `Fabo <Name>.app` with `~/.kiri-<name>`.

```sh
KIRI_SIGN_IDENTITY=- make app-dev
open "target/dev/Fabo Dev.app"                 # sets up, starts the engine, opens the window
docker --context kiri-dev run --rm alpine echo hello
SOAK=600 DOCKER_CONTEXT=kiri-dev tests/e2e.sh  # every check, then 600 runs watching for leaks
"target/dev/Fabo Dev.app/Contents/MacOS/fabo" setup --uninstall   # takes the setup back
```

Opening the app from Finder sets the Mac up the first time, as `fabo setup`
does: a docker context for the installation, `fabo` and `docker` in its bin
directory, a LaunchAgent that starts the engine at login, and a buildx
builder that loads what it builds into the engine. The installed app
(`dev.kiri.app`) also puts `~/.kiri/bin` on PATH, links `/usr/local/bin/fabo`,
adds its Compose and buildx to docker's plugin path, makes its context
docker's default when there is no other engine, and points Testcontainers
at the engine. A flavored app leaves all of those alone. `fabo setup
--uninstall` takes back exactly what setup did and stops the engine; the
data stays. `DOCKER_CONFIG` redirects everything setup writes for docker,
which keeps a trial run out of `~/.docker`.

The launcher's commands: `fabo` or `fabo ui` opens the window, and starts
the engine first when nothing answers on the socket. `fabo daemon` runs the
engine in the foreground. `fabo setup` and `fabo build-with` are above.
`fabo migrate-rust` imports an offline Kiri (Rust) store, as
[desktop-overview.md](desktop-overview.md) describes. `fabo run`, `fabo ps`, `fabo exec`, `fabo compose` and the other Docker
commands use this installation's socket regardless of the selected Docker
context. The engine must already be running. Arguments, terminal I/O and
exit status pass directly to the Docker CLI on PATH (or the bundled CLI).
An explicit `BUILDX_BUILDER` is preserved; otherwise a registered builder
for this installation is selected. On macOS, use `fabo container update`
for container resources: `fabo update` is reserved for the app updater.
Kiri's other commands (`k8s`, `machine`, `import`, `usb`, `bench`, `raw`) have no Go
counterpart yet; see the [baseline](compatibility-baseline.md).

The engine never writes to a Kiri (Rust) store. When
`~/Library/Application Support/Kiri` holds one that has not been imported,
the launcher refuses to start. `KIRI_ENGINE=go` starts with an empty Go store
anyway.

To run the daemon without a bundle, with private directories of your own,
follow "Go VM on macOS" in [desktop-overview.md](desktop-overview.md).

## Architecture

```
docker CLI ─┐
compose ────┼─ ~/.kiri/run/docker.sock ─► kiri-go-daemon ── vsock ──► kiri-go-init (PID 1)
buildx ─────┤     Docker Engine API         owns the VM              clone3 → container
MyGo window ┘
```

- **VM**: Apple's Virtualization.framework, bound through cgo
  (`engine/cmd/kiri-go-daemon/virtualization_darwin.m`). A custom 10 MB
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
- **Protocol**: Kiri's host ↔ guest protocol, version 4
  (`engine/internal/guestwire`).

## Layout

| Path | What |
|------|------|
| `engine/cmd/kiri` | the launcher: starts the engine with the installation's paths, opens the window |
| `engine/cmd/kiri-go-daemon` | the daemon: Docker API, VM or host-process agent, Finder mount, USB |
| `engine/cmd/kiri-go-init` | the guest agent's entry point, PID 1 in the VM |
| `engine/cmd/kiri-mkinitramfs` | packs the agent into the initramfs the VM boots |
| `engine/internal/localengine` | the Docker API and everything the daemon persists |
| `engine/internal/guestagent` | the container runtime, Linux only |
| `engine/internal/guestwire` | host ↔ guest wire protocol |
| `desktop/` | the MyGo window and tray (`kiri-desktop`) |
| `guest/kernel` | kernel config and patches |
| `scripts/macos-app.sh`, `scripts/macos-dmg.sh` | the app bundle and its disk image |
| `packaging/` | `cli-pins.env`, the macOS entitlement, Linux `.deb` and `.rpm` |
| `tests/e2e.sh` | about 500 checks through the real `docker` CLI, plus a leak soak (`K8S=1` adds Kubernetes) |
| `tests/linux-host.sh` | the Linux build, driven in a privileged container |
| `contrib/`, `installer/windows` | upstream's systemd, WSL and Windows scripts, not yet adapted to the Go binaries |
| `bench/results.json` | upstream Kiri's measured results |
| `brand/` | icons and logos |
