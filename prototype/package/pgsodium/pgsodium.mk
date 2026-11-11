PGSODIUM_VERSION = 3.1.9
PGSODIUM_SOURCE = v$(PGSODIUM_VERSION).tar.gz
PGSODIUM_SITE = https://github.com/michelp/pgsodium/archive/refs/tags
PGSODIUM_LICENSE = PostgreSQL
PGSODIUM_LDFLAGS = $(TARGET_LDFLAGS) $(TARGET_NLS_LIBS)
PGSODIUM_DEPENDENCIES = postgresql libsodium
PGSODIUM_INSTALL_STAGING = YES

# PGXS build: use the pg_config from staging so it picks the right include/lib dirs
define PGSODIUM_BUILD_CMDS
	$(TARGET_MAKE_ENV) $(MAKE) -C $(@D) USE_PGXS=1 \
		PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config
endef

# Install into target rootfs. PGXS honors DESTDIR.
define PGSODIUM_INSTALL_TARGET_CMDS
	$(TARGET_MAKE_ENV) $(MAKE) -C $(@D) USE_PGXS=1 \
		PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config \
		DESTDIR=$(TARGET_DIR) install
endef

# Optional: install into staging too (useful if other target packages need it at build time)
define PGSODIUM_INSTALL_STAGING_CMDS
	$(TARGET_MAKE_ENV) $(MAKE) -C $(@D) USE_PGXS=1 \
		PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config \
		DESTDIR=$(STAGING_DIR) install
endef

$(eval $(generic-package))