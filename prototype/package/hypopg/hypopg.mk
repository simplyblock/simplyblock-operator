HYPOPG_VERSION = 1.4.2
HYPOPG_SOURCE = $(HYPOPG_VERSION).tar.gz
HYPOPG_SITE = https://github.com/HypoPG/hypopg/archive/refs/tags
HYPOPG_LICENSE = PostgreSQL
HYPOPG_LDFLAGS = $(TARGET_LDFLAGS) $(TARGET_NLS_LIBS)
HYPOPG_DEPENDENCIES = postgresql
HYPOPG_SUPPORTS_IN_SOURCE_BUILD = NO

define HYPOPG_BUILD_CMDS
	cd $(@D) && $(MAKE1) PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config $(TARGET_CONFIGURE_OPTS) PKGCONFIG="$(PKG_CONFIG_HOST_BINARY)" OPTFLAGS="" -C $(@D)
endef

define HYPOPG_INSTALL_TARGET_CMDS
	$(INSTALL) $(@D)/hypopg.so $(TARGET_DIR)/usr/lib/postgresql
	$(INSTALL) $(@D)/hypopg.control $(TARGET_DIR)/usr/share/postgresql/extension
	$(INSTALL) $(@D)/*.sql $(TARGET_DIR)/usr/share/postgresql/extension
endef

$(eval $(generic-package))