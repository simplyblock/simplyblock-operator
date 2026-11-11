PG_NET_VERSION = 0.20.2
PG_NET_SOURCE = v$(PG_NET_VERSION).tar.gz
PG_NET_SITE = https://github.com/supabase/pg_net/archive/refs/tags
PG_NET_LICENSE = PostgreSQL
PG_NET_LDFLAGS = $(TARGET_LDFLAGS) $(TARGET_NLS_LIBS)
PG_NET_DEPENDENCIES = postgresql libcurl
PG_NET_SUPPORTS_IN_SOURCE_BUILD = NO

# PGXS build: use the pg_config from staging so it picks the right include/lib dirs
define PG_NET_BUILD_CMDS
	$(TARGET_MAKE_ENV) $(MAKE) -C $(@D) USE_PGXS=1 \
		PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config
endef

# Install into target rootfs. PGXS honors DESTDIR.
define PG_NET_INSTALL_TARGET_CMDS
	$(TARGET_MAKE_ENV) $(MAKE) -C $(@D) USE_PGXS=1 \
		PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config \
		DESTDIR=$(TARGET_DIR) install
endef

# Optional: install into staging too (useful if other target packages need it at build time)
define PG_NET_INSTALL_STAGING_CMDS
	$(TARGET_MAKE_ENV) $(MAKE) -C $(@D) USE_PGXS=1 \
		PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config \
		DESTDIR=$(STAGING_DIR) install
endef

$(eval $(generic-package))