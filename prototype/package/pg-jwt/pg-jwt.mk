PG_JWT_VERSION = f3d82fd30151e754e19ce5d6a06c71c20689ce3d
PG_JWT_SITE = $(call github,michelp,pgjwt,$(PG_JWT_VERSION))
PG_JWT_LICENSE = PostgreSQL
PG_JWT_LDFLAGS = $(TARGET_LDFLAGS) $(TARGET_NLS_LIBS)
PG_JWT_DEPENDENCIES = postgresql

define PG_JWT_BUILD_CMDS
endef

# Install into target rootfs. PGXS honors DESTDIR.
define PG_JWT_INSTALL_TARGET_CMDS
	$(TARGET_MAKE_ENV) $(MAKE) -C $(@D) USE_PGXS=1 \
		PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config \
		DESTDIR=$(TARGET_DIR) install
endef

$(eval $(generic-package))