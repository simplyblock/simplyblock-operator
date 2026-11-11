RUM_VERSION = 1.3.15
RUM_SOURCE = $(RUM_VERSION).tar.gz
RUM_SITE = https://github.com/postgrespro/rum/archive/refs/tags
RUM_LICENSE = PostgreSQL
RUM_LDFLAGS = $(TARGET_LDFLAGS) $(TARGET_NLS_LIBS)
RUM_DEPENDENCIES = postgresql
RUM_SUPPORTS_IN_SOURCE_BUILD = NO

define RUM_BUILD_CMDS
	cd $(@D) && $(MAKE1) PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config USE_PGXS=1 PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config $(TARGET_CONFIGURE_OPTS) PKGCONFIG="$(PKG_CONFIG_HOST_BINARY)" OPTFLAGS="" -C $(@D)
endef

define RUM_INSTALL_TARGET_CMDS
	$(INSTALL) $(@D)/rum.so $(TARGET_DIR)/usr/lib/postgresql
	$(INSTALL) $(@D)/rum.control $(TARGET_DIR)/usr/share/postgresql/extension
	$(INSTALL) $(@D)/*.sql $(TARGET_DIR)/usr/share/postgresql/extension
endef

$(eval $(generic-package))