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

# The API upgrade tool ships here too, so it can run either from a workstation
# against a kubeconfig or as a Job in the cluster, and so that the conversion
# code it verifies is the code this image carries.
#
# CGO is off for the same reason as the probe: it talks to the Kubernetes API
# over the caller's own credentials and does no crypto of its own.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -a -o simplyblock-upgrade ./cmd/simplyblock-upgrade

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
# On PATH, because the probe Job names it by name rather than by path.
COPY --from=builder /workspace/simplyblock-nodeprobe /usr/local/bin/simplyblock-nodeprobe
# On PATH, because a user runs it by name inside a debug pod as well as through
# the Job the upgrade creates.
COPY --from=builder /workspace/simplyblock-upgrade /usr/local/bin/simplyblock-upgrade
COPY LICENSE /licenses/LICENSE

# The manager runs unprivileged as this user. The probe pod overrides it: it
# runs as root because a container's device cgroup denies opening block devices
# otherwise, and because the host's mount table is PID 1's and readable by the
# user PID 1 runs as. See internal/nodeprobe/job.go for what that buys and what
# bounds it.
USER 65532:65532

ENTRYPOINT ["/manager"]
