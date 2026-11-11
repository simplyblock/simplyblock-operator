PGROONGA_VERSION = 4.0.5
PGROONGA_SOURCE = $(PGROONGA_VERSION).tar.gz
PGROONGA_SITE = https://github.com/pgroonga/pgroonga/archive/refs/tags
PGROONGA_LICENSE = PostgreSQL
PGROONGA_INSTALL_STAGING = true
PGROONGA_DEPENDENCIES = postgresql host-pkgconf xxhash groonga zstd

$(eval $(meson-package))