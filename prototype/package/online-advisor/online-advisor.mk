ONLINE_ADVISOR_VERSION = 1.0
ONLINE_ADVISOR_SOURCE = $(ONLINE_ADVISOR_VERSION).tar.gz
ONLINE_ADVISOR_SITE = https://github.com/knizhnik/online_advisor/archive/refs/tags
ONLINE_ADVISOR_LICENSE = PostgreSQL
ONLINE_ADVISOR_DEPENDENCIES = postgresql

# PGXS build: use the pg_config from staging so it picks the right include/lib dirs
define ONLINE_ADVISOR_BUILD_CMDS
	$(TARGET_MAKE_ENV) $(MAKE) -C $(@D) USE_PGXS=1 \
		PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config
endef

# Install into target rootfs. PGXS honors DESTDIR.
define ONLINE_ADVISOR_INSTALL_TARGET_CMDS
	$(TARGET_MAKE_ENV) $(MAKE) -C $(@D) USE_PGXS=1 \
		PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config \
		DESTDIR=$(TARGET_DIR) install
endef

# Optional: install into staging too (useful if other target packages need it at build time)
define ONLINE_ADVISOR_INSTALL_STAGING_CMDS
	$(TARGET_MAKE_ENV) $(MAKE) -C $(@D) USE_PGXS=1 \
		PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config \
		DESTDIR=$(STAGING_DIR) install
endef

$(eval $(generic-package))