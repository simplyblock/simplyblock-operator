PG_REPACK_VERSION = 1.5.3
PG_REPACK_SOURCE = ver_$(PG_REPACK_VERSION).tar.gz
PG_REPACK_SITE = https://github.com/reorg/pg_repack/archive/refs/tags
PG_REPACK_LICENSE = PostgreSQL
PG_REPACK_LDFLAGS = $(TARGET_LDFLAGS) $(TARGET_NLS_LIBS)
PG_REPACK_DEPENDENCIES = postgresql

define PG_REPACK_BUILD_CMDS
	cd $(@D) && $(MAKE1) USE_PGXS=1 PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config $(TARGET_CONFIGURE_OPTS) PKGCONFIG="$(PKG_CONFIG_HOST_BINARY)" OPTFLAGS="" -C $(@D)
endef

define PG_REPACK_INSTALL_TARGET_CMDS
	$(INSTALL) $(@D)/lib/pg_repack.so $(TARGET_DIR)/usr/lib/postgresql
	$(INSTALL) $(@D)/lib/pg_repack.control $(TARGET_DIR)/usr/share/postgresql/extension
	$(INSTALL) $(@D)/lib/*.sql $(TARGET_DIR)/usr/share/postgresql/extension
endef

$(eval $(generic-package))