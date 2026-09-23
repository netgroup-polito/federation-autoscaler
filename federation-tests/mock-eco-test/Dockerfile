FROM golang:1.24 AS builder
WORKDIR /workspace
COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -a -o mock-eco-test ./federation-tests/mock-eco-test/cmd/

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /workspace/mock-eco-test /app
WORKDIR /
USER 65532:65532
ENTRYPOINT ["/app"]
