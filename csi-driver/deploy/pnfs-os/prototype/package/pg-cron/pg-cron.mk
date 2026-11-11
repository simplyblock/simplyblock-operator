PG_CRON_VERSION = 1.6.7
PG_CRON_SOURCE = v$(PG_CRON_VERSION).tar.gz
PG_CRON_SITE = https://github.com/citusdata/pg_cron/archive/refs/tags
PG_CRON_LICENSE = PostgreSQL
PG_CRON_LDFLAGS = $(TARGET_LDFLAGS) $(TARGET_NLS_LIBS)
PG_CRON_DEPENDENCIES = postgresql
PG_CRON_INSTALL_STAGING = YES

# PGXS build: use the pg_config from staging so it picks the right include/lib dirs
define PG_CRON_BUILD_CMDS
	$(TARGET_MAKE_ENV) $(MAKE) -C $(@D) USE_PGXS=1 \
		PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config
endef

# Install into target rootfs. PGXS honors DESTDIR.
define PG_CRON_INSTALL_TARGET_CMDS
	$(TARGET_MAKE_ENV) $(MAKE) -C $(@D) USE_PGXS=1 \
		PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config \
		DESTDIR=$(TARGET_DIR) install
endef

# Optional: install into staging too (useful if other target packages need it at build time)
define PG_CRON_INSTALL_STAGING_CMDS
	$(TARGET_MAKE_ENV) $(MAKE) -C $(@D) USE_PGXS=1 \
		PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config \
		DESTDIR=$(STAGING_DIR) install
endef

$(eval $(generic-package))