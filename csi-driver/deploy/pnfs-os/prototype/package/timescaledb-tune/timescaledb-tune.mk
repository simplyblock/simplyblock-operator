TIMESCALEDB_TUNE_VERSION = 0.18.1
TIMESCALEDB_TUNE_SITE = https://github.com/timescale/timescaledb-tune/archive/refs/tags
TIMESCALEDB_TUNE_SOURCE = v$(TIMESCALEDB_TUNE_VERSION).tar.gz
TIMESCALEDB_TUNE_LICENSE = Apache-2.0
TIMESCALEDB_TUNE_LICENSE_FILES = LICENSE

define TIMESCALEDB_TUNE_BUILD_CMDS
    cd $(@D) && \
        GOOS=linux GOARCH=amd64 \
        GOPATH=$(HOST_DIR)/share/go \
        CGO_ENABLED=0 \
        GOTOOLCHAIN=local \
        go build -mod=mod \
        -ldflags="-s -w" \
        -o timescaledb-tune \
        ./cmd/timescaledb-tune
endef

define TIMESCALEDB_TUNE_INSTALL_TARGET_CMDS
    $(INSTALL) -D -m 0755 $(@D)/timescaledb-tune $(TARGET_DIR)/usr/bin/timescaledb-tune
endef

$(eval $(generic-package))
