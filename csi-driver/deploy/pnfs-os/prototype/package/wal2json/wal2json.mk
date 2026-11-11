WAL2JSON_VERSION = 2_6
WAL2JSON_SOURCE = wal2json_$(WAL2JSON_VERSION).tar.gz
WAL2JSON_SITE = https://github.com/eulerto/wal2json/archive/refs/tags
WAL2JSON_LICENSE = PostgreSQL
WAL2JSON_LDFLAGS = $(TARGET_LDFLAGS) $(TARGET_NLS_LIBS)
WAL2JSON_DEPENDENCIES = postgresql
WAL2JSON_INSTALL_STAGING = YES

# PGXS build: use the pg_config from staging so it picks the right include/lib dirs
define WAL2JSON_BUILD_CMDS
	$(TARGET_MAKE_ENV) $(MAKE) -C $(@D) USE_PGXS=1 \
		PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config
endef

# Install into target rootfs. PGXS honors DESTDIR.
define WAL2JSON_INSTALL_TARGET_CMDS
	$(TARGET_MAKE_ENV) $(MAKE) -C $(@D) USE_PGXS=1 \
		PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config \
		DESTDIR=$(TARGET_DIR) install
endef

# Optional: install into staging too (useful if other target packages need it at build time)
define WAL2JSON_INSTALL_STAGING_CMDS
	$(TARGET_MAKE_ENV) $(MAKE) -C $(@D) USE_PGXS=1 \
		PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config \
		DESTDIR=$(STAGING_DIR) install
endef

$(eval $(generic-package))