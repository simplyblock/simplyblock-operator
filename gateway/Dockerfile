FROM --platform=$BUILDPLATFORM golang:1.26 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace/gateway

# gateway imports the operator's API types, and both reach their sibling
# modules through local replace directives (../operator, ../atlas-lib), so the
# build context is the repository root and the siblings sit next to gateway
# here. Their go.mod/go.sum come first: go mod download needs them to build the
# module graph, but not their source.
COPY atlas-lib/go.mod atlas-lib/go.sum /workspace/atlas-lib/
COPY operator/go.mod operator/go.sum /workspace/operator/
COPY gateway/go.mod gateway/go.sum ./
RUN go mod download
COPY atlas-lib/ /workspace/atlas-lib/
COPY operator/api/ /workspace/operator/api/
COPY gateway/cmd/ cmd/
COPY gateway/internal/ internal/
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -buildvcs=false -o gateway ./cmd/gateway

# A static binary needs nothing from a runtime image but CA certificates
# (for the Kubernetes API, and the Vault/S3/LDAP IAM backends embedgw.Config
# can select once that design lands) and the directory the posix backend
# serves from.
FROM gcr.io/distroless/static-debian12

COPY --from=builder /workspace/gateway/gateway /app/gateway

ENTRYPOINT ["/app/gateway"]
