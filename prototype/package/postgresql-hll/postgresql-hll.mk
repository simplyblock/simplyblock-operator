POSTGRESQL_HLL_VERSION = 2.19
POSTGRESQL_HLL_SOURCE = v$(POSTGRESQL_HLL_VERSION).tar.gz
POSTGRESQL_HLL_SITE = https://github.com/citusdata/postgresql-hll/archive/refs/tags
POSTGRESQL_HLL_LICENSE = PostgreSQL
POSTGRESQL_HLL_DEPENDENCIES = postgresql libpqxx
POSTGRESQL_HLL_SUPPORTS_IN_SOURCE_BUILD = NO
POSTGRESQL_HLL_INSTALL_STAGING = YES

# PGXS build: use the pg_config from staging so it picks the right include/lib dirs
define POSTGRESQL_HLL_BUILD_CMDS
	$(TARGET_MAKE_ENV) $(MAKE) -C $(@D) USE_PGXS=1 \
		CPP="$(TARGET_CXX)" \
		CXX="$(TARGET_CXX)" \
		CC="$(TARGET_CC)" \
		GCC="$(TARGET_CC)" \
		LD="$(TARGET_LD)" \
		PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config
endef

# Install into target rootfs. PGXS honors DESTDIR.
define POSTGRESQL_HLL_INSTALL_TARGET_CMDS
	$(TARGET_MAKE_ENV) $(MAKE) -C $(@D) USE_PGXS=1 \
		PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config \
		DESTDIR=$(TARGET_DIR) install
endef

# Optional: install into staging too (useful if other target packages need it at build time)
define POSTGRESQL_HLL_INSTALL_STAGING_CMDS
	$(TARGET_MAKE_ENV) $(MAKE) -C $(@D) USE_PGXS=1 \
		PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config \
		DESTDIR=$(STAGING_DIR) install
endef

$(eval $(generic-package))