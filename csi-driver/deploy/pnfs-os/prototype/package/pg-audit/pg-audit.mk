PG_AUDIT_VERSION = 18.0
PG_AUDIT_SOURCE = $(PG_AUDIT_VERSION).tar.gz
PG_AUDIT_SITE = https://github.com/pgaudit/pgaudit/archive/refs/tags
PG_AUDIT_LICENSE = PostgreSQL
PG_AUDIT_LDFLAGS = $(TARGET_LDFLAGS) $(TARGET_NLS_LIBS)
PG_AUDIT_DEPENDENCIES = postgresql
PG_AUDIT_INSTALL_STAGING = YES

# PGXS build: use the pg_config from staging so it picks the right include/lib dirs
define PG_AUDIT_BUILD_CMDS
	$(TARGET_MAKE_ENV) $(MAKE) -C $(@D) USE_PGXS=1 \
		PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config
endef

# Install into target rootfs. PGXS honors DESTDIR.
define PG_AUDIT_INSTALL_TARGET_CMDS
	$(TARGET_MAKE_ENV) $(MAKE) -C $(@D) USE_PGXS=1 \
		PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config \
		DESTDIR=$(TARGET_DIR) install
endef

# Optional: install into staging too (useful if other target packages need it at build time)
define PG_AUDIT_INSTALL_STAGING_CMDS
	$(TARGET_MAKE_ENV) $(MAKE) -C $(@D) USE_PGXS=1 \
		PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config \
		DESTDIR=$(STAGING_DIR) install
endef

$(eval $(generic-package))