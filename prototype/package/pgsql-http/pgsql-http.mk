PGSQL_HTTP_VERSION = 1.7.0
PGSQL_HTTP_SOURCE = v$(PGSQL_HTTP_VERSION).tar.gz
PGSQL_HTTP_SITE = https://github.com/pramsey/pgsql-http/archive/refs/tags
PGSQL_HTTP_LICENSE = PostgreSQL
PGSQL_HTTP_LDFLAGS = $(TARGET_LDFLAGS) $(TARGET_NLS_LIBS)
PGSQL_HTTP_DEPENDENCIES = postgresql libcurl

# PGXS build: use the pg_config from staging so it picks the right include/lib dirs
define PGSQL_HTTP_BUILD_CMDS
	$(TARGET_MAKE_ENV) $(MAKE) -C $(@D) USE_PGXS=1 \
		PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config
endef

# Install into target rootfs. PGXS honors DESTDIR.
define PGSQL_HTTP_INSTALL_TARGET_CMDS
	$(TARGET_MAKE_ENV) $(MAKE) -C $(@D) USE_PGXS=1 \
		PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config \
		DESTDIR=$(TARGET_DIR) install
endef

# Optional: install into staging too (useful if other target packages need it at build time)
define PGSQL_HTTP_INSTALL_STAGING_CMDS
	$(TARGET_MAKE_ENV) $(MAKE) -C $(@D) USE_PGXS=1 \
		PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config \
		DESTDIR=$(STAGING_DIR) install
endef

$(eval $(generic-package))