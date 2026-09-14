# The fleet image. It carries the hub manager today, and it is where the member
# agent lands when that is built, for the reason the operator's image carries its
# node probe: the manager renders the add-on template that deploys the agent and
# names its own image for it, so the agent cannot be a version out of step with
# the manager that shipped it.
#
# The build context is the repository root, because this module resolves
# atlas-lib and the operator through local replace directives and a Docker build
# cannot reach outside its context.
FROM --platform=$BUILDPLATFORM golang:1.26 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace

# The two sibling modules this one replaces. Their manifests are copied first and
# on their own: the module graph needs their go.mod and go.sum, and a local
# replacement is never downloaded, so copying their sources here would invalidate
# the download layer on every unrelated edit to them.
COPY atlas-lib/go.mod atlas-lib/go.sum /atlas-lib/
COPY operator/go.mod operator/go.sum /operator/

COPY fleet/go.mod fleet/go.sum ./
RUN go mod download

COPY atlas-lib/ /atlas-lib/
COPY operator/ /operator/
COPY fleet/ .

# CGO is off. This process talks to two Kubernetes API servers and a control
# plane over TLS the standard library provides, and a static binary is what the
# minimal runtime base below can run without a libc.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -a -o fleet-manager ./cmd/fleet-manager

FROM registry.access.redhat.com/ubi10/ubi-minimal:10.2

ARG VERSION=0.1.0
ARG RELEASE=1

RUN microdnf update -y && microdnf clean all

LABEL name="simplyblock-fleet" \
      vendor="Simplyblock" \
      maintainer="developers@simplyblock.io" \
      version="${VERSION}" \
      release="${RELEASE}" \
      summary="The simplyblock fleet manager drives several Kubernetes clusters from one hub." \
      description="The simplyblock fleet manager reconciles the fleet.simplyblock.io custom resources on an Open Cluster Management hub into the payloads that build out storage in each enrolled member cluster."

WORKDIR /
COPY --from=builder /workspace/fleet-manager .
COPY LICENSE /licenses/LICENSE

USER 65532:65532

ENTRYPOINT ["/fleet-manager"]
