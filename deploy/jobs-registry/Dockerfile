# Containerize jobs-registry. The module tree is private
# (GOPRIVATE=github.com/fables-for-robots/*), so the binary is built OUTSIDE
# the image where credentials exist, and copied in:
#
#   CGO_ENABLED=0 GOOS=linux nix develop -c \
#     go build -trimpath -o deploy/jobs-registry/jobs-registry ./cmd/jobs-registry
#   docker build -t jobs-registry deploy/jobs-registry
#
# distroless/static ships CA certs (needed for pkarr/DNS discovery when no
# --addr is given) and nothing else.
FROM gcr.io/distroless/static-debian12:nonroot
# Multi-arch: the release builds jobs-registry-{amd64,arm64} into the
# context and one buildx --platform invocation assembles the manifest list
# (COPY-only image — no qemu needed).
ARG TARGETARCH
COPY jobs-registry-${TARGETARCH} /usr/bin/jobs-registry
# /data must be a writable volume: the amber store, blob cache and image
# records live there (see k8s.yaml's PVC).
ENV JOBS_DATA_DIR=/data
EXPOSE 5000
ENTRYPOINT ["/usr/bin/jobs-registry"]
