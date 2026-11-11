PGAUDITLOGTOFILE_VERSION = 1.7.6
PGAUDITLOGTOFILE_SOURCE = v$(PGAUDITLOGTOFILE_VERSION).tar.gz
PGAUDITLOGTOFILE_SITE = https://github.com/fmbiete/pgauditlogtofile/archive/refs/tags
PGAUDITLOGTOFILE_LICENSE = PostgreSQL
PGAUDITLOGTOFILE_LDFLAGS = $(TARGET_LDFLAGS) $(TARGET_NLS_LIBS)
PGAUDITLOGTOFILE_DEPENDENCIES = postgresql
PGAUDITLOGTOFILE_INSTALL_STAGING = YES

# PGXS build: use the pg_config from staging so it picks the right include/lib dirs
define PGAUDITLOGTOFILE_BUILD_CMDS
	$(TARGET_MAKE_ENV) $(MAKE) -C $(@D) USE_PGXS=1 \
		PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config
endef

# Install into target rootfs. PGXS honors DESTDIR.
define PGAUDITLOGTOFILE_INSTALL_TARGET_CMDS
	$(TARGET_MAKE_ENV) $(MAKE) -C $(@D) USE_PGXS=1 \
		PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config \
		DESTDIR=$(TARGET_DIR) install
endef

# Optional: install into staging too (useful if other target packages need it at build time)
define PGAUDITLOGTOFILE_INSTALL_STAGING_CMDS
	$(TARGET_MAKE_ENV) $(MAKE) -C $(@D) USE_PGXS=1 \
		PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config \
		DESTDIR=$(STAGING_DIR) install
endef

$(eval $(generic-package))