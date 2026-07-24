#!/usr/bin/env bash
# test.sh — run the Go unit tests. They link libtdjson (CGO), so they run inside
# the build image with the same toolchain as the build, not on the host.
#
# Usage:  ./scripts/test.sh
set -eu

docker build --target go-builder -t tgapi-gobuild .
docker run --rm tgapi-gobuild go test ./...
