# Build the manager binary
FROM --platform=$BUILDPLATFORM golang:1.26 AS builder
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

# Use Red Hat UBI10 minimal as runtime base (required for Red Hat certification).
# ubi10-minimal includes glibc, which is required for CGO-linked binaries (BoringCrypto).
FROM registry.access.redhat.com/ubi10/ubi-minimal:10.2

ARG VERSION=0.1.0
ARG RELEASE=1

# Apply all available security patches from the UBI10 repositories.
RUN microdnf update -y && microdnf clean all

# Required labels for Red Hat certification (preflight check operator).
LABEL name="simplyblock-operator" \
      vendor="Simplyblock" \
      maintainer="developers@simplyblock.io" \
      version="${VERSION}" \
      release="${RELEASE}" \
      summary="Simplyblock Operator manages the lifecycle of Simplyblock storage clusters on Kubernetes." \
      description="The Simplyblock Operator reconciles StorageCluster, StorageNode, Pool, Device, Lvol, Task, SnapshotReplication, and StorageBackup custom resources against the Simplyblock control-plane API."

WORKDIR /
COPY --from=builder /workspace/manager .
COPY LICENSE /licenses/LICENSE
USER 65532:65532

ENTRYPOINT ["/manager"]
