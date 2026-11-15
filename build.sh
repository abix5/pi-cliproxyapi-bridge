#!/usr/bin/env bash
# Build pi-bridge.so for the CLIProxyAPI container (linux/amd64 by default).
#
# The plugin must be compiled against the same SDK version as the running
# CLIProxyAPI image, and cgo requires a matching toolchain, so the build runs
# inside a Go container rather than on the host.
#
# Set PLATFORM=linux/arm64 to build for an arm64 CLIProxyAPI image. Building
# for a platform other than the Docker host's needs QEMU/binfmt emulation.
#
# The artifact is named pi-bridge-v<VERSION>.so because the host only
# hot-reloads a plugin when its file PATH changes; overwriting a .so in place
# would require a container restart.
set -euo pipefail

CPA_SDK_VERSION="${CPA_SDK_VERSION:-v8.0.63}"
# The CLIProxyAPI image runs on debian:bookworm, so link against the same
# glibc to avoid symbol versions the runtime does not provide.
GO_IMAGE="${GO_IMAGE:-golang:1.26-bookworm}"
OUT_DIR="${OUT_DIR:-dist}"
PLATFORM="${PLATFORM:-linux/amd64}"

case "$PLATFORM" in
  linux/amd64 | linux/arm64) ;;
  *)
    echo "unsupported PLATFORM: $PLATFORM (use linux/amd64 or linux/arm64)" >&2
    exit 1
    ;;
esac

cd "$(dirname "$0")"
mkdir -p "$OUT_DIR"

# Single source of truth: the version constant compiled into the plugin.
VERSION="$(sed -n 's/.*pluginVersion = "\(.*\)".*/\1/p' main.go)"
if [ -z "$VERSION" ]; then
  echo "could not determine pluginVersion from main.go" >&2
  exit 1
fi
ARTIFACT="pi-bridge-v${VERSION}.so"

echo "Building ${ARTIFACT} (SDK ${CPA_SDK_VERSION}, ${PLATFORM})"

docker run --rm \
  --platform "$PLATFORM" \
  -v "$PWD":/src \
  -w /src \
  -e GOFLAGS=-mod=mod \
  -e CGO_ENABLED=1 \
  "$GO_IMAGE" \
  go build -buildmode=c-shared -trimpath -o "$OUT_DIR/$ARTIFACT" .

echo "Built: $OUT_DIR/$ARTIFACT"
ls -la "$OUT_DIR/$ARTIFACT"
