PG_STAT_MONITOR_VERSION = 2.3.1
PG_STAT_MONITOR_SOURCE = $(PG_STAT_MONITOR_VERSION).tar.gz
PG_STAT_MONITOR_SITE = https://github.com/percona/pg_stat_monitor/archive/refs/tags
PG_STAT_MONITOR_LICENSE = PostgreSQL
PG_STAT_MONITOR_INSTALL_STAGING = true
PG_STAT_MONITOR_DEPENDENCIES = postgresql host-pkgconf

# PGXS build: use the pg_config from staging so it picks the right include/lib dirs
define PG_STAT_MONITOR_BUILD_CMDS
	$(TARGET_MAKE_ENV) $(MAKE) -C $(@D) USE_PGXS=1 \
		PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config
endef

# Install into target rootfs. PGXS honors DESTDIR.
define PG_STAT_MONITOR_INSTALL_TARGET_CMDS
	$(TARGET_MAKE_ENV) $(MAKE) -C $(@D) USE_PGXS=1 \
		PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config \
		DESTDIR=$(TARGET_DIR) install
endef

# Optional: install into staging too (useful if other target packages need it at build time)
define PG_STAT_MONITOR_INSTALL_STAGING_CMDS
	$(TARGET_MAKE_ENV) $(MAKE) -C $(@D) USE_PGXS=1 \
		PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config \
		DESTDIR=$(STAGING_DIR) install
endef

$(eval $(generic-package))