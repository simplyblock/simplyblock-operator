# The build environment of the pNFS guest: the host tools buildroot needs and
# the pinned buildroot release, unpacked to /opt/buildroot. build.sh runs in it
# as the invoking user, so the outputs it writes to /out are not owned by root.
# Buildroot is part of this image rather than of the source tree, so the
# repository carries no copy of it and the download is cached with the image.

FROM ubuntu:24.04

ARG BUILDROOT_VERSION=2026.05.3
ARG BUILDROOT_SHA256=f43f1627082a45848f73083b3b8d59f32bb4c77a449a8ead8a2a1f91849dea00

ENV DEBIAN_FRONTEND=noninteractive
ENV LANG=en_US.UTF-8
ENV LC_ALL=en_US.UTF-8

RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        bc \
        bison \
        build-essential \
        ca-certificates \
        ccache \
        cmake \
        cpio \
        file \
        flex \
        git \
        libbz2-dev \
        libssl-dev \
        libtool \
        locales \
        lzop \
        m4 \
        pkg-config \
        python3 \
        rsync \
        texinfo \
        unzip \
        wget \
        xz-utils && \
    locale-gen en_US.UTF-8 && \
    rm -rf /var/lib/apt/lists/*

RUN set -eu; \
    wget -qO /tmp/buildroot.tar.xz "https://buildroot.org/downloads/buildroot-${BUILDROOT_VERSION}.tar.xz"; \
    echo "${BUILDROOT_SHA256}  /tmp/buildroot.tar.xz" | sha256sum -c -; \
    mkdir /opt/buildroot; \
    tar -xJf /tmp/buildroot.tar.xz -C /opt/buildroot --strip-components=1; \
    rm /tmp/buildroot.tar.xz; \
    mkdir -m 1777 /work

ENV HOME=/work
WORKDIR /work
