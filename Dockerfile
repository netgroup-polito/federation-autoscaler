# syntax=docker/dockerfile:1.4
# Build one federation-autoscaler component image.
#
# The component is selected at build time via the COMPONENT build-arg and must
# be one of: broker, agent, grpc-server. The Makefile targets docker-build,
# docker-push, docker-buildx all pass --build-arg COMPONENT=<...>.
#
#   docker build --build-arg COMPONENT=broker -t federation-autoscaler/broker:latest .
#
# The agent image additionally bundles `liqoctl` on $PATH so the consumer
# Peer/Unpeer + provider GenerateKubeconfig handlers can shell out to it. The
# broker and grpc-server images stay liqoctl-free — see the runtime-* stages
# below for the split.
#
# Global ARGs go before the first FROM so they can be used inside the final
# FROM runtime-${COMPONENT} directive.
ARG COMPONENT
ARG LIQOCTL_VERSION=v1.1.2

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

# -----------------------------------------------------------------------------
# liqoctl: pinned upstream Liqo release.
#
# Only consumed by the agent runtime stage; broker / grpc-server builds skip
# this stage entirely because BuildKit prunes unreferenced targets.
# Bump LIQOCTL_VERSION in lock-step with the Liqo CRDs the consumer / provider
# instruction handlers create — they must match the cluster's installed Liqo.
# -----------------------------------------------------------------------------
FROM alpine:3 AS liqoctl
ARG TARGETOS
ARG TARGETARCH
ARG LIQOCTL_VERSION
RUN apk add --no-cache curl ca-certificates tar && \
    os=${TARGETOS:-linux} && arch=${TARGETARCH:-amd64} && \
    curl -fsSL -o /tmp/liqoctl.tar.gz \
        "https://github.com/liqotech/liqo/releases/download/${LIQOCTL_VERSION}/liqoctl-${os}-${arch}.tar.gz" && \
    tar -C /tmp -xzf /tmp/liqoctl.tar.gz liqoctl && \
    chmod +x /tmp/liqoctl

# -----------------------------------------------------------------------------
# Runtime stages, one per component.
#
# broker and grpc-server share the same minimal distroless layer (just the
# Go binary). The agent stage adds liqoctl on /usr/local/bin. The final
# `FROM runtime-${COMPONENT}` line picks the matching flavour.
# -----------------------------------------------------------------------------
FROM gcr.io/distroless/static:nonroot AS runtime-broker
COPY --from=builder /workspace/app /app

FROM gcr.io/distroless/static:nonroot AS runtime-grpc-server
COPY --from=builder /workspace/app /app

FROM gcr.io/distroless/static:nonroot AS runtime-agent
COPY --from=builder /workspace/app /app
COPY --from=liqoctl /tmp/liqoctl /usr/local/bin/liqoctl

# Pick the right runtime layer based on which component is being built.
FROM runtime-${COMPONENT}
WORKDIR /
USER 65532:65532
ENTRYPOINT ["/app"]
