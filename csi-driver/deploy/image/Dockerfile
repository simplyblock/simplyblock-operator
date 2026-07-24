# SPDX-License-Identifier: Apache-2.0
# Copyright (c) Arm Limited and Contributors
# Copyright (c) Intel Corporation
#
FROM --platform=$BUILDPLATFORM golang:1.26 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace

# Build context is the repo root. atlas is a sibling module consumed via a local
# replace (../atlas-lib), so it must sit next to the csi module. Copy only its
# manifests before go mod download: the download needs atlas-lib's go.mod/go.sum to
# build the module graph, not its source (local-path replacements are never
# downloaded). Copying the full tree here would invalidate the download layer on any
# atlas-lib source change.
COPY atlas-lib/go.mod atlas-lib/go.sum /atlas-lib/

COPY csi-driver/go.mod csi-driver/go.sum ./
RUN go mod download
COPY atlas-lib/ /atlas-lib/
COPY csi-driver/cmd/ cmd/
COPY csi-driver/pkg/ pkg/
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -buildvcs=false -o spdkcsi ./cmd/

FROM simplyblock/spdkcsi:base_image

USER root
WORKDIR /app

COPY --from=builder --chmod=0755 /workspace/spdkcsi spdkcsi

ENTRYPOINT ["/app/spdkcsi"]
