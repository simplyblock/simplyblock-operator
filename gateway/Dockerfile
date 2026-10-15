FROM --platform=$BUILDPLATFORM golang:1.26 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace

# No sibling module: unlike csi-driver and operator, gateway has no
# replace directive on atlas-lib, so the build context is this directory alone.
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -buildvcs=false -o gateway ./cmd/gateway

# A static binary needs nothing from a runtime image but CA certificates
# (for the Vault/S3/LDAP IAM backends embedgw.Config can select once that
# design lands) and the directory the posix backend serves from.
FROM gcr.io/distroless/static-debian12

COPY --from=builder /workspace/gateway /app/gateway

ENTRYPOINT ["/app/gateway"]
