# Build one federation-autoscaler component image.
#
# The component is selected at build time via the COMPONENT build-arg and must
# be one of: broker, agent, grpc-server. The Makefile targets docker-build,
# docker-push, docker-buildx all pass --build-arg COMPONENT=<...>.
#
#   docker build --build-arg COMPONENT=broker -t federation-autoscaler/broker:latest .
FROM golang:1.24 AS builder
ARG TARGETOS
ARG TARGETARCH
ARG COMPONENT

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# Cache deps before copying source so source changes don't re-invalidate the
# module download layer.
RUN go mod download

# Copy the Go source (relies on .dockerignore to filter)
COPY . .

# Build the selected component. GOARCH has no default so the image matches the
# host platform by default; --platform / buildx can override.
RUN test -n "${COMPONENT}" || { echo "ERROR: --build-arg COMPONENT=<broker|agent|grpc-server> is required" >&2; exit 1; }
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -a -o app ./cmd/${COMPONENT}

# Use distroless as minimal base image to package the binary.
# Refer to https://github.com/GoogleContainerTools/distroless for more details.
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/app /app
USER 65532:65532

ENTRYPOINT ["/app"]
