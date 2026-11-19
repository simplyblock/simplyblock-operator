# syntax=docker/dockerfile:1
#
# sb-mock — the Control Center's test backend. One static binary that
# impersonates the Kubernetes API (simplyblock + Ramen CRDs + core objects),
# the control plane v2 API, the operator API and Prometheus. See mock/README.md.
#
#   docker build -t quay.io/simplyblock-io/control-center-mock:dev control-center/mock
#
# The image carries no CRD YAML (the build context is this directory); mount
# operator/config/crd/bases at /crds, or pass --crd-dir, for the
# customresourcedefinitions list to be populated.

FROM docker.io/library/golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /sb-mock .

FROM scratch
COPY --from=build /sb-mock /sb-mock
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/sb-mock"]
