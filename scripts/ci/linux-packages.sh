#!/bin/sh
# Kiri's Linux packages on a Mac, in an arm64 Linux container (the Mac's
# docker engine is Kiri): scripts/ci/linux-build.sh with what it needs
# installed first. Release builds make them on Ubuntu instead and test them
# there and on Fedora.
#
#   scripts/ci/linux-packages.sh
#
# Leaves them and SHA256SUMS-linux in target/packages. Named volumes keep
# zig, the crates, the downloads and the build between runs.
set -eu
root=$(cd "$(dirname "$0")/../.." && pwd)
go_version=$(awk '$1 == "go" { print $2; exit }' "$root/engine/go.mod")
tools_image="kiri-linux-package-tools:go-$go_version"
docker build --platform linux/arm64 --build-arg "GO_VERSION=$go_version" -t "$tools_image" - <<'DOCKERFILE'
ARG GO_VERSION
FROM golang:${GO_VERSION}-bookworm AS go
FROM rust:1-bookworm
COPY --from=go /usr/local/go /usr/local/go
ENV PATH="/usr/local/go/bin:${PATH}"
RUN apt-get update -qq && apt-get install -y -qq --no-install-recommends \
    python3 python3-venv rpm pkg-config libwebkit2gtk-4.1-dev && rm -rf /var/lib/apt/lists/*
DOCKERFILE
docker run --rm --platform linux/arm64 \
  -v "$root:/src" -w /src \
  -v kiri-linux-cache:/root/.cache \
  -v kiri-linux-cargo:/usr/local/cargo/registry \
  -e HOME=/root \
  "$tools_image" sh scripts/ci/linux-build.sh
