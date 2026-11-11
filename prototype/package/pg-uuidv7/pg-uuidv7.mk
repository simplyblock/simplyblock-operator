PG_UUIDV7_VERSION = 1.7.0
PG_UUIDV7_SOURCE = v$(PG_UUIDV7_VERSION).tar.gz
PG_UUIDV7_SITE = https://github.com/fboulnois/pg_uuidv7/archive/refs/tags
PG_UUIDV7_LICENSE = PostgreSQL
PG_UUIDV7_LDFLAGS = $(TARGET_LDFLAGS) $(TARGET_NLS_LIBS)
PG_UUIDV7_DEPENDENCIES = postgresql

define PG_UUIDV7_BUILD_CMDS
	cd $(@D) && $(MAKE1) PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config $(TARGET_CONFIGURE_OPTS) PKGCONFIG="$(PKG_CONFIG_HOST_BINARY)" OPTFLAGS="" -C $(@D)
endef

define PG_UUIDV7_INSTALL_TARGET_CMDS
	$(INSTALL) $(@D)/pg_uuidv7.so $(TARGET_DIR)/usr/lib/postgresql
	$(INSTALL) $(@D)/pg_uuidv7.control $(TARGET_DIR)/usr/share/postgresql/extension
	$(INSTALL) $(@D)/sql/*.sql $(TARGET_DIR)/usr/share/postgresql/extension
endef

$(eval $(generic-package))