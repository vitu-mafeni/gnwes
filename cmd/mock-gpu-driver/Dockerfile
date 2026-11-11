FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY cmd/mock-gpu-driver/go.mod cmd/mock-gpu-driver/go.sum ./
RUN go mod download
COPY cmd/mock-gpu-driver/*.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -o /mock-gpu-driver .

# Default target (`docker build .` / `--target mock`): the mock backend
# never shells out to anything, so the smallest possible static image works.
# root, not :nonroot -- this plugin must write into root-owned kubelet
# plugin/registry directories and the node's CDI spec directory via hostPath
# mounts, the same way real device-plugin/CSI/DRA driver images typically do.
FROM gcr.io/distroless/static:latest AS mock
COPY --from=build /mock-gpu-driver /mock-gpu-driver
ENTRYPOINT ["/mock-gpu-driver"]

# `--target real-gpu`: the nvidia backend execs nvidia-smi/nvidia-ctk, real
# dynamically-linked ELF binaries bind-mounted in from the host node (see
# config/dra/driver-real-gpu.yaml) -- running them needs a real libc/dynamic
# linker, which the distroless/static image above deliberately doesn't have.
FROM debian:bookworm-slim AS real-gpu
COPY --from=build /mock-gpu-driver /mock-gpu-driver
ENTRYPOINT ["/mock-gpu-driver"]
