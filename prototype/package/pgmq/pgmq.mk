PGMQ_VERSION = 1.8.1
PGMQ_SOURCE = v$(PGMQ_VERSION).tar.gz
PGMQ_SITE = https://github.com/pgmq/pgmq/archive/refs/tags
PGMQ_LICENSE = PostgreSQL
PGMQ_LDFLAGS = $(TARGET_LDFLAGS) $(TARGET_NLS_LIBS)
PGMQ_DEPENDENCIES = postgresql

# PGXS build: use the pg_config from staging so it picks the right include/lib dirs
define PGMQ_BUILD_CMDS
	$(TARGET_MAKE_ENV) $(MAKE) -C $(@D)/pgmq-extension USE_PGXS=1 \
		PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config
endef

# Install into target rootfs. PGXS honors DESTDIR.
define PGMQ_INSTALL_TARGET_CMDS

	$(TARGET_MAKE_ENV) $(MAKE) -C $(@D)/pgmq-extension USE_PGXS=1 \
		PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config \
		DESTDIR=$(TARGET_DIR) install
endef

# Optional: install into staging too (useful if other target packages need it at build time)
define PGMQ_INSTALL_STAGING_CMDS
	$(TARGET_MAKE_ENV) $(MAKE) -C $(@D)/pgmq-extension USE_PGXS=1 \
		PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config \
		DESTDIR=$(STAGING_DIR) install
endef

$(eval $(generic-package))