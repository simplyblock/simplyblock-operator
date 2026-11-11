PG_ROARINGBITMAP_VERSION = 1.1.0
PG_ROARINGBITMAP_SOURCE = v$(PG_ROARINGBITMAP_VERSION).tar.gz
PG_ROARINGBITMAP_SITE = https://github.com/ChenHuajun/pg_roaringbitmap/archive/refs/tags
PG_ROARINGBITMAP_LICENSE = PostgreSQL
PG_ROARINGBITMAP_LDFLAGS = $(TARGET_LDFLAGS) $(TARGET_NLS_LIBS)
PG_ROARINGBITMAP_DEPENDENCIES = postgresql
PG_ROARINGBITMAP_SUPPORTS_IN_SOURCE_BUILD = NO

# PGXS build: use the pg_config from staging so it picks the right include/lib dirs
define PG_ROARINGBITMAP_BUILD_CMDS
	$(TARGET_MAKE_ENV) $(MAKE) -C $(@D) USE_PGXS=1 \
		PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config
endef

# Install into target rootfs. PGXS honors DESTDIR.
define PG_ROARINGBITMAP_INSTALL_TARGET_CMDS
	$(TARGET_MAKE_ENV) $(MAKE) -C $(@D) USE_PGXS=1 \
		PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config \
		DESTDIR=$(TARGET_DIR) install
endef

# Optional: install into staging too (useful if other target packages need it at build time)
define PG_ROARINGBITMAP_INSTALL_STAGING_CMDS
	$(TARGET_MAKE_ENV) $(MAKE) -C $(@D) USE_PGXS=1 \
		PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config \
		DESTDIR=$(STAGING_DIR) install
endef

$(eval $(generic-package))