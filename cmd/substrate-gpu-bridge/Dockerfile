# Bundles kubectl-ate-linux into the bridge image so the sidecar is
# self-contained: no separate container, no relying on whatever the worker
# pod's own filesystem happens to carry. kubectl-ate does not build natively
# on Windows (internal/imagecache uses syscall.Stat_t); cross-compile it
# from a real agent-substrate checkout first:
#   CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/kubectl-ate-linux ./cmd/kubectl-ate
# CGO_ENABLED=0 is REQUIRED, not optional, even when building on a real Linux
# box for linux/amd64: cgo defaults to on there too, producing a
# dynamically-linked binary against glibc. This base image
# (gcr.io/distroless/static) has no dynamic linker at all, so the container
# fails with "fork/exec ...: no such file or directory" at runtime --
# despite the file genuinely existing -- the exact same failure mode this
# repo already hit once building podgate-webhook itself (see
# config/substrate-integration/README.md), confirmed again the hard way
# (2026-11-13) for this binary specifically.
# (run from the agent-substrate repo root, then copy bin/kubectl-ate-linux
# into this directory's bin/ before `docker build`, same as
# bin/substrate-gpu-bridge below.)
FROM gcr.io/distroless/static:nonroot
COPY bin/substrate-gpu-bridge /substrate-gpu-bridge
COPY bin/kubectl-ate-linux /usr/local/bin/kubectl-ate
ENTRYPOINT ["/substrate-gpu-bridge"]
