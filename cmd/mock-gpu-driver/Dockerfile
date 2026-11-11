FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY cmd/mock-gpu-driver/go.mod cmd/mock-gpu-driver/go.sum ./
RUN go mod download
COPY cmd/mock-gpu-driver/main.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -o /mock-gpu-driver .

# root, not :nonroot -- this plugin must write into root-owned kubelet
# plugin/registry directories and the node's CDI spec directory via hostPath
# mounts, the same way real device-plugin/CSI/DRA driver images typically do.
FROM gcr.io/distroless/static:latest
COPY --from=build /mock-gpu-driver /mock-gpu-driver
ENTRYPOINT ["/mock-gpu-driver"]
