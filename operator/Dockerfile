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

# The worker probe ships in this image rather than in one of its own, so that
# the Job a discovery run creates cannot be a version out of step with the
# operator that created it: the operator names its own image for the Job.
#
# CGO is off for it, unlike the manager. The probe reads sysfs and procfs and
# issues ioctls through golang.org/x/sys and does no crypto at all, so it has
# nothing to gain from BoringCrypto and nothing to link against.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -a -o simplyblock-nodeprobe ./cmd/simplyblock-nodeprobe

# The conversion webhook ships in this image under its own entry point rather
# than an image of its own, so the conversion code and the API types it converts
# between are versioned together with the operator that reads them
# (design-api-upgrade.md §29.3). It runs as a separate Deployment all the same:
# a CRD whose conversion strategy is Webhook is unreadable while the webhook is
# down, and the resources an administrator reads to diagnose a failed operator
# are exactly those (§6.1).
#
# It serves TLS, so it takes the same BoringCrypto build the manager does.
RUN CC=$([ "${TARGETARCH}" = "arm64" ] && echo "aarch64-linux-gnu-gcc" || echo "gcc") \
    CGO_ENABLED=1 GOEXPERIMENT=boringcrypto \
    GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -a -o conversion-webhook ./cmd/conversion-webhook

# Use Red Hat UBI10 minimal as runtime base (required for Red Hat certification).
# ubi10-minimal includes glibc, which is required for CGO-linked binaries (BoringCrypto).
FROM registry.access.redhat.com/ubi10/ubi-minimal:10.2

ARG VERSION=0.1.0
ARG RELEASE=1

# Apply all available security patches from the UBI10 repositories.
#
# checkov:skip=CKV_DOCKER_5: The check wants an update paired with an install,
# so that a stale package index cannot be cached ahead of one. Patching the
# base image is the whole purpose of this layer and it installs nothing, which
# is what Red Hat certification asks of a UBI-derived image.
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
# On PATH, because the probe Job names it by name rather than by path.
COPY --from=builder /workspace/simplyblock-nodeprobe /usr/local/bin/simplyblock-nodeprobe
COPY --from=builder /workspace/conversion-webhook /usr/local/bin/conversion-webhook
COPY LICENSE /licenses/LICENSE

# The manager runs unprivileged as this user. The probe pod overrides it: it
# runs as root because a container's device cgroup denies opening block devices
# otherwise, and because the host's mount table is PID 1's and readable by the
# user PID 1 runs as. See internal/nodeprobe/job.go for what that buys and what
# bounds it.
USER 65532:65532

ENTRYPOINT ["/manager"]
