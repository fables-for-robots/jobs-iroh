#!/usr/bin/env bash
# Build and push the release images for the current (clean, tagged) checkout:
#
#   dmilhdef/jobs-iroh-server  ← cmd/jobs-server    (deploy/jobs-server)
#   dmilhdef/jobs-iroh-runner  ← cmd/jobs-runner    (deploy/jobs-runner)
#   dmilhdef/jobs-registry     ← cmd/jobs-registry  (deploy/jobs-registry)
#
# (jobs-iroh-* rather than jobs-*: dmilhdef/jobs-server and jobs-runner are the
# pre-iroh JOBS images and stay untouched.)
#
# Each is a linux/amd64 + linux/arm64 manifest list tagged v<version> and
# latest, labelled with version/revision/source. One invocation per release:
#
#   scripts/release-images.sh            # version from version/version.go
#   scripts/release-images.sh 0.29.0     # or explicit
#
# Requirements: clean tree on the release tag (a dirty tree flips Go's
# vcs.modified stamp into the binaries), docker with the jobs-multi buildx
# builder (see CLAUDE.md "Release process"), and a Docker Hub login. The
# Dockerfiles are COPY-only, so no QEMU is needed for the arm64 half.
set -euo pipefail
cd "$(dirname "$0")/.."

V=${1:-$(sed -n 's/^const Version = "\(.*\)"$/\1/p' version/version.go)}
[ -n "$V" ] || { echo "no version"; exit 1; }
[ -z "$(git status --porcelain)" ] || { echo "tree is dirty — release images must come from a clean checkout"; exit 1; }
REV=$(git rev-parse HEAD)
DOCKER=${DOCKER:-docker}
DOCKER_CONFIG_DIR=${DOCKER_CONFIG_DIR:-$HOME/.docker}

cleanup() { rm -f deploy/jobs-server/jobs-server-{amd64,arm64} deploy/jobs-runner/jobs-runner-{amd64,arm64} deploy/jobs-registry/jobs-registry-{amd64,arm64}; }
trap cleanup EXIT

echo "== building binaries for v$V ($REV)"
for b in jobs-server jobs-runner jobs-registry; do
  for arch in amd64 arm64; do
    nix develop -c bash -c "export GOPRIVATE='github.com/jobs-build/*'; CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -o deploy/$b/$b-$arch ./cmd/$b"
  done
done

image_name() { case "$1" in jobs-server) echo jobs-iroh-server;; jobs-runner) echo jobs-iroh-runner;; *) echo "$1";; esac; }
for b in jobs-server jobs-runner jobs-registry; do
  img=$(image_name "$b")
  echo "== pushing dmilhdef/$img:v$V (+latest)"
  "$DOCKER" --config "$DOCKER_CONFIG_DIR" buildx build --builder jobs-multi \
    --platform linux/amd64,linux/arm64 --provenance=false --sbom=false \
    --label org.opencontainers.image.version="$V" \
    --label org.opencontainers.image.revision="$REV" \
    --label org.opencontainers.image.source=https://github.com/jobs-build/jobs-iroh \
    --annotation "index:org.opencontainers.image.version=$V" \
    --annotation "index:org.opencontainers.image.revision=$REV" \
    --annotation "index:org.opencontainers.image.source=https://github.com/jobs-build/jobs-iroh" \
    -t "dmilhdef/$img:v$V" -t "dmilhdef/$img:latest" \
    --push "deploy/$b"
  "$DOCKER" --config "$DOCKER_CONFIG_DIR" buildx imagetools inspect "dmilhdef/$img:v$V" | grep -E "Platform:|Name:" | head -6
done
echo "== done: v$V"
