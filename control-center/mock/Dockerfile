# syntax=docker/dockerfile:1
#
# sb-mock — the Control Center's test backend. One static binary that
# impersonates the Kubernetes API (simplyblock + Ramen CRDs + core objects),
# the operator API and Prometheus. See mock/README.md.
#
#   docker build -t quay.io/simplyblock-io/control-center-mock:dev control-center/mock

FROM docker.io/library/golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /sb-mock .

FROM scratch
COPY --from=build /sb-mock /sb-mock
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/sb-mock"]
