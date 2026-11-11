PG_PARTMAN_VERSION = 5.4.0
PG_PARTMAN_SOURCE = v$(PG_PARTMAN_VERSION).tar.gz
PG_PARTMAN_SITE = https://github.com/pgpartman/pg_partman/archive/refs/tags
PG_PARTMAN_LICENSE = PostgreSQL
PG_PARTMAN_LDFLAGS = $(TARGET_LDFLAGS) $(TARGET_NLS_LIBS)
PG_PARTMAN_DEPENDENCIES = postgresql

# PGXS build: use the pg_config from staging so it picks the right include/lib dirs
define PG_PARTMAN_BUILD_CMDS
	$(TARGET_MAKE_ENV) $(MAKE) -C $(@D) USE_PGXS=1 \
		PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config
endef

# Install into target rootfs. PGXS honors DESTDIR.
define PG_PARTMAN_INSTALL_TARGET_CMDS

	$(TARGET_MAKE_ENV) $(MAKE) -C $(@D) USE_PGXS=1 \
		PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config \
		DESTDIR=$(TARGET_DIR) install
endef

# Optional: install into staging too (useful if other target packages need it at build time)
define PG_PARTMAN_INSTALL_STAGING_CMDS
	$(TARGET_MAKE_ENV) $(MAKE) -C $(@D) USE_PGXS=1 \
		PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config \
		DESTDIR=$(STAGING_DIR) install
endef

$(eval $(generic-package))