# Build tasks. `make help` lists them.
#
# ARCH picks the Mac a build is for: arm64 (default) or x86_64 for Intel.
# Each Mac gets its own app rather than one universal app, which would carry
# both builds of the docker CLI, Compose and buildx.

ARCH ?= arm64
ifeq ($(ARCH),x86_64)
GOARCH := amd64
GUEST_OUT := guest/out/x86_64
KERNEL_CACHE := fabo-kbuild-x86_64
CLANG_ARCH := x86_64
else ifeq ($(ARCH),arm64)
GOARCH := arm64
GUEST_OUT := guest/out
KERNEL_CACHE := fabo-kbuild
CLANG_ARCH := arm64
else
$(error ARCH is arm64 or x86_64, not $(ARCH))
endif

# The version comes from the newest tag (docs/release.md). A checkout with no
# tag yet builds as 0.0.0-dev.
VERSION ?= $(or $(shell git describe --tags --dirty 2>/dev/null | sed 's/^v//'),0.0.0-dev)

# Shipped binaries leave out the symbol table and DWARF (-s -w), about a
# third of their size; panics still name functions and lines. `make daemon`,
# built to debug the engine, keeps them.
STRIP := -s -w

# The Linux build's CPU: this machine's, unless LINUX_ARCH says otherwise.
LINUX_ARCH ?= $(shell go env GOARCH)

# The fabo-edge relay `fabo share` uses unless the user picks one, as
# host[:port]. Empty in source: no relay is assumed. The relay itself is
# kept in its own repository.
FABO_SHARE_SERVER ?=
export FABO_SHARE_SERVER
SHARE_LDFLAGS := $(if $(FABO_SHARE_SERVER),-X github.com/OpenFabrica/Fabo/engine/sharetunnel.DefaultServer=$(FABO_SHARE_SERVER))

.PHONY: release-mac release-check help check guest kernel disk daemon app app-dev dev dmg linux windows clean

help:
	@echo "make check     vet and test engine and desktop; compile the Linux- and Windows-only tests"
	@echo "make guest     Go guest agent and $(GUEST_OUT)/initramfs-go.cpio"
	@echo "make kernel    guest kernel in $(GUEST_OUT) (needs a Docker engine)"
	@echo "make disk      empty data disk template guest/out/data.img (needs a Docker engine)"
	@echo "make daemon    target/fabo-daemon, signed for Virtualization.framework"
	@echo "make app       target/Fabo.app: engine, guest, desktop, docker CLI, Compose, buildx"
	@echo "make app-dev   target/dev/Fabo Dev.app: a second installation with its own state;"
	@echo "               its window shows the installed app's engine, containers and images"
	@echo "               (FABO_OWN_ENGINE=1: its own engine; FLAVOR=name builds Fabo <Name>.app)"
	@echo "make dev       run the desktop with automatic reload on source changes"
	@echo "make release-mac  both DMGs (notarized at once), Linux archives, kernel source, SHA256SUMS"
	@echo "make dmg       target/Fabo-<version>-$(ARCH).dmg from the app, notarized when"
	@echo "               FABO_NOTARY_PROFILE names a notarytool profile"
	@echo "make linux     target/linux/release: fabo, fabo-daemon, fabo-init"
	@echo "make windows   target/windows/fabo-windows-\$$WIN_ARCH: the folder the MSI installs (x86_64 or arm64),"
	@echo "               checked against installer/windows/fabo.wxs; the MSI itself is built on Windows"
	@echo "               (scripts/windows-msi.ps1, docs/windows.md). FABO_UPDATE_URL sets its update feed"
	@echo "FABO_SHARE_SERVER=host builds daemon, app, app-dev and linux with that default share relay."
	@echo "make release-check TAG=vX.Y.Z  stop unless HEAD is exactly that tag, on main, clean"
	@echo "ARCH=x86_64 builds guest, kernel, daemon, app and dmg for Intel Macs."

check:
	go -C engine vet ./...
	go -C engine test -race ./...
	go -C desktop vet ./...
	go -C desktop test ./...
	GOOS=linux GOARCH=arm64 go -C engine vet ./...
	GOOS=linux GOARCH=arm64 go -C engine test -exec true ./...
	GOOS=windows GOARCH=amd64 go -C engine vet ./...
	GOOS=windows GOARCH=amd64 go -C engine test -exec true ./...
	GOOS=windows GOARCH=amd64 go -C desktop vet ./...
	GOOS=windows GOARCH=amd64 go -C desktop test -exec true ./...
	GOOS=windows GOARCH=arm64 go -C engine vet ./...

guest:
	mkdir -p $(GUEST_OUT)
	GOOS=linux GOARCH=$(GOARCH) CGO_ENABLED=0 go -C engine build -trimpath -ldflags "$(STRIP)" -o $(CURDIR)/$(GUEST_OUT)/fabo-init ./cmd/fabo-init
	go -C engine run ./cmd/fabo-mkinitramfs $(CURDIR)/$(GUEST_OUT)/fabo-init $(CURDIR)/$(GUEST_OUT)/initramfs-go.cpio

# Both kernels build in their source tree, so each keeps its own cached tree
# in a named volume. On Apple Silicon the x86-64 kernel is cross-compiled.
kernel:
	mkdir -p $(GUEST_OUT)
	docker run --rm -e DEBIAN_FRONTEND=noninteractive -e KARCH=$(ARCH) \
		-v $(KERNEL_CACHE):/build -v $(CURDIR)/guest/kernel:/src:ro -v $(CURDIR)/$(GUEST_OUT):/out \
		debian:trixie sh /src/build.sh

# A sparse 64 GiB ext4 image with lazily initialised inode tables: it uses a
# few MB until data is written. The same image serves both architectures.
disk:
	mkdir -p guest/out
	docker run --rm -v $(CURDIR)/guest/out:/out debian:trixie sh -c '\
		set -e; apt-get update -qq >/dev/null && apt-get install -y -qq e2fsprogs >/dev/null; \
		rm -f /out/data.img; truncate -s 64G /out/data.img; \
		mkfs.ext4 -q -F -m 0 -L fabo-data -E lazy_itable_init=1,lazy_journal_init=1 /out/data.img'

# Virtualization.framework refuses to start a VM without the entitlement.
daemon:
	mkdir -p target
	GOOS=darwin GOARCH=$(GOARCH) CGO_ENABLED=1 CC="clang -arch $(CLANG_ARCH)" MACOSX_DEPLOYMENT_TARGET=14.0 \
		go -C engine build -trimpath -ldflags "-X main.engineRelease=$(VERSION) $(SHARE_LDFLAGS)" -o $(CURDIR)/target/fabo-daemon ./cmd/fabo-daemon
	codesign --force --sign - --entitlements packaging/macos/fabo.entitlements target/fabo-daemon

app: guest
	scripts/macos-app.sh $(ARCH) $(VERSION)

app-dev: guest
	scripts/macos-app.sh $(ARCH) $(VERSION) $(or $(FLAVOR),dev)

# The development window borrows the installed app as a shared-engine
# bundle does (openDesktop in engine/cmd/fabo/desktop.go): its socket, so
# the window's files are those in ~/.fabo too, and its launcher. Naming a
# socket yourself opts out of all three, so the window never shows one
# engine while its fabo commands act on another.
dev:
	@if [ -z "$$FABO_SOCKET" ] && [ -x /Applications/Fabo.app/Contents/MacOS/fabo ]; then \
		export FABO_SOCKET="$$HOME/.fabo/run/docker.sock"; \
		export FABO_LAUNCHER="$${FABO_LAUNCHER:-/Applications/Fabo.app/Contents/MacOS/fabo}"; \
		export FABO_SHARED_ENGINE=1; \
	fi; \
	exec go -C desktop tool mygo dev

dmg:
	scripts/macos-dmg.sh $(ARCH) $(VERSION)

# Every macOS-built release file for both architectures, notarized together
# (scripts/macos-release.sh); needs FABO_SHARE_SERVER and FABO_NOTARY_PROFILE.
release-mac:
	scripts/macos-release.sh $(VERSION)

# Static binaries, so one build runs on every distribution.
linux:
	mkdir -p target/linux/release
	GOOS=linux GOARCH=$(LINUX_ARCH) CGO_ENABLED=0 go -C engine build -trimpath -ldflags "$(STRIP) -X main.release=$(VERSION)" -o $(CURDIR)/target/linux/release/fabo ./cmd/fabo
	GOOS=linux GOARCH=$(LINUX_ARCH) CGO_ENABLED=0 go -C engine build -trimpath -ldflags "$(STRIP) -X main.engineRelease=$(VERSION) $(SHARE_LDFLAGS)" -o $(CURDIR)/target/linux/release/fabo-daemon ./cmd/fabo-daemon
	GOOS=linux GOARCH=$(LINUX_ARCH) CGO_ENABLED=0 go -C engine build -trimpath -ldflags "$(STRIP)" -o $(CURDIR)/target/linux/release/fabo-init ./cmd/fabo-init

# The Windows bundle, cross-built on any OS; WiX makes the MSI from it on
# Windows (docs/windows.md).
WIN_ARCH ?= x86_64
windows:
	scripts/windows-bundle.sh $(WIN_ARCH) $(VERSION)
	python3 -I scripts/check-wxs.py target/windows/fabo-windows-$(WIN_ARCH)

# The release gate from docs/release.md; run it before building a release.
release-check:
	@test -n "$(TAG)" || { echo "usage: make release-check TAG=vX.Y.Z" >&2; exit 2; }
	scripts/release-check.sh $(TAG)

clean:
	rm -rf target
