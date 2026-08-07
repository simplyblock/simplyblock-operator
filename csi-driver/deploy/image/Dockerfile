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

# lvm2 (pvcreate/vgcreate/lvcreate/vgchange/lvextend/dmsetup/etc.) is required by
# pkg/util/vdo.go to manage client-side VDO devices. vdo provides vdoformat, which
# lvcreate --type vdo shells out to internally to format the new VDO pool -- confirmed live
# ("/usr/bin/vdoformat: execvp failed: No such file or directory") after lvm2 alone wasn't
# enough. Neither is part of base_image; added here rather than there to avoid
# rebuilding/overwriting that shared, cross-branch image tag for an in-progress feature.
# Candidate for promotion into Dockerfile_base once this lands.
RUN dnf install -y lvm2 vdo --nogpgcheck && dnf clean all

COPY --from=builder --chmod=0755 /workspace/spdkcsi spdkcsi

ENTRYPOINT ["/app/spdkcsi"]
