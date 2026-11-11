POSTGRESQL_UNIT_VERSION = 7.10
POSTGRESQL_UNIT_SOURCE = $(POSTGRESQL_UNIT_VERSION).tar.gz
POSTGRESQL_UNIT_SITE = https://github.com/df7cb/postgresql-unit/archive/refs/tags
POSTGRESQL_UNIT_LICENSE = PostgreSQL
POSTGRESQL_UNIT_LDFLAGS = $(TARGET_LDFLAGS) $(TARGET_NLS_LIBS)
POSTGRESQL_UNIT_DEPENDENCIES = postgresql
POSTGRESQL_UNIT_SUPPORTS_IN_SOURCE_BUILD = NO

define POSTGRESQL_UNIT_BUILD_CMDS
	cd $(@D) && $(MAKE1) PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config $(TARGET_CONFIGURE_OPTS) PKGCONFIG="$(PKG_CONFIG_HOST_BINARY)" OPTFLAGS="" -C $(@D)
endef

define POSTGRESQL_UNIT_INSTALL_TARGET_CMDS
	$(INSTALL) $(@D)/unit.so $(TARGET_DIR)/usr/lib/postgresql
	$(INSTALL) $(@D)/unit.control $(TARGET_DIR)/usr/share/postgresql/extension
	$(INSTALL) $(@D)/*.sql $(TARGET_DIR)/usr/share/postgresql/extension
endef

$(eval $(generic-package))