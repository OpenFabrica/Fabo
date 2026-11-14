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

# The Linux build's CPU: this machine's, unless LINUX_ARCH says otherwise.
LINUX_ARCH ?= $(shell go env GOARCH)

.PHONY: release-check help check guest kernel disk daemon app app-dev dev dmg linux clean

help:
	@echo "make check     vet and test engine and desktop; compile the Linux-only tests"
	@echo "make guest     Go guest agent and $(GUEST_OUT)/initramfs-go.cpio"
	@echo "make kernel    guest kernel in $(GUEST_OUT) (needs a Docker engine)"
	@echo "make disk      empty data disk template guest/out/data.img (needs a Docker engine)"
	@echo "make daemon    target/fabo-daemon, signed for Virtualization.framework"
	@echo "make app       target/Fabo.app: engine, guest, desktop, docker CLI, Compose, buildx"
	@echo "make app-dev   target/dev/Fabo Dev.app: a second installation with its own state"
	@echo "               (FLAVOR=name builds Fabo <Name>.app instead)"
	@echo "make dev       build and open the development app (also accepts FLAVOR=name)"
	@echo "make dmg       target/Fabo-<version>-$(ARCH).dmg from the app, notarized when"
	@echo "               FABO_NOTARY_PROFILE names a notarytool profile"
	@echo "make linux     target/linux/release: fabo, fabo-daemon, fabo-init"
	@echo "make release-check TAG=vX.Y.Z  stop unless HEAD is exactly that tag, on main, clean"
	@echo "ARCH=x86_64 builds guest, kernel, daemon, app and dmg for Intel Macs."

check:
	go -C engine vet ./...
	go -C engine test -race ./...
	go -C desktop vet ./...
	go -C desktop test ./...
	GOOS=linux GOARCH=arm64 go -C engine vet ./...
	GOOS=linux GOARCH=arm64 go -C engine test -exec true ./...

guest:
	mkdir -p $(GUEST_OUT)
	GOOS=linux GOARCH=$(GOARCH) CGO_ENABLED=0 go -C engine build -trimpath -o $(CURDIR)/$(GUEST_OUT)/fabo-init ./cmd/fabo-init
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
		go -C engine build -trimpath -ldflags "-X main.engineRelease=$(VERSION)" -o $(CURDIR)/target/fabo-daemon ./cmd/fabo-daemon
	codesign --force --sign - --entitlements packaging/macos/fabo.entitlements target/fabo-daemon

app: guest
	scripts/macos-app.sh $(ARCH) $(VERSION)

app-dev: guest
	scripts/macos-app.sh $(ARCH) $(VERSION) $(or $(FLAVOR),dev)

dev: app-dev
	@flavor="$(or $(FLAVOR),dev)"; \
	name="$$(printf '%s' "$$flavor" | cut -c1 | tr a-z A-Z)$$(printf '%s' "$$flavor" | cut -c2-)"; \
	open "$(CURDIR)/$(if $(filter x86_64,$(ARCH)),target/intel,target)/dev/Fabo $$name.app"

dmg:
	scripts/macos-dmg.sh $(ARCH) $(VERSION)

# Static binaries, so one build runs on every distribution.
linux:
	mkdir -p target/linux/release
	GOOS=linux GOARCH=$(LINUX_ARCH) CGO_ENABLED=0 go -C engine build -trimpath -ldflags "-X main.release=$(VERSION)" -o $(CURDIR)/target/linux/release/fabo ./cmd/fabo
	GOOS=linux GOARCH=$(LINUX_ARCH) CGO_ENABLED=0 go -C engine build -trimpath -ldflags "-X main.engineRelease=$(VERSION)" -o $(CURDIR)/target/linux/release/fabo-daemon ./cmd/fabo-daemon
	GOOS=linux GOARCH=$(LINUX_ARCH) CGO_ENABLED=0 go -C engine build -trimpath -o $(CURDIR)/target/linux/release/fabo-init ./cmd/fabo-init

# The release gate from docs/release.md; run it before building a release.
release-check:
	@test -n "$(TAG)" || { echo "usage: make release-check TAG=vX.Y.Z" >&2; exit 2; }
	scripts/release-check.sh $(TAG)

clean:
	rm -rf target
