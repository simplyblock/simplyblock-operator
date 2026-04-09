# Build the manager binary
FROM --platform=$BUILDPLATFORM golang:1.24 AS builder
ARG TARGETOS
ARG TARGETARCH
ARG BUILDPLATFORM

WORKDIR /workspace

# Install cross-compilers required for CGO (CGO is needed for GOEXPERIMENT=boringcrypto).
RUN apt-get update && apt-get install -y \
    gcc \
    gcc-aarch64-linux-gnu \
    libc6-dev-arm64-cross \
    && rm -rf /var/lib/apt/lists/*

# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the Go source (relies on .dockerignore to filter)
COPY . .

# Build with BoringCrypto (FIPS 140-2 validated crypto).
# CGO_ENABLED=1 is required by GOEXPERIMENT=boringcrypto.
# CC selects the appropriate cross-compiler when targeting arm64 from an amd64 host.
RUN CC=$([ "${TARGETARCH}" = "arm64" ] && echo "aarch64-linux-gnu-gcc" || echo "gcc") \
    CGO_ENABLED=1 GOEXPERIMENT=boringcrypto \
    GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -a -o manager cmd/main.go

# Use distroless/base (includes glibc, required for CGO-linked binaries) as minimal base image.
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM gcr.io/distroless/base:nonroot
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]
