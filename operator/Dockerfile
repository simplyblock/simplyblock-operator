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

# atlas is a sibling module consumed via a local replace (../atlas-lib). The build
# context is the repo root, so it lives next to the operator module. Copy only its
# manifests first: go mod download needs atlas-lib's go.mod/go.sum to build the module
# graph, but not its source (local-path replacements are never downloaded). Copying the
# full tree here would invalidate the download layer on any atlas-lib source change.
COPY atlas-lib/go.mod atlas-lib/go.sum /atlas-lib/

# Copy the Go Modules manifests
COPY operator/go.mod operator/go.sum ./
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the full sources (operator + the atlas-lib tree resolved by the replace directive).
COPY atlas-lib/ /atlas-lib/
COPY operator/ .

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
