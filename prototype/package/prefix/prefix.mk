PREFIX_VERSION = 1.2.10
PREFIX_SOURCE = v$(PREFIX_VERSION).tar.gz
PREFIX_SITE = https://github.com/dimitri/prefix/archive/refs/tags
PREFIX_LICENSE = PostgreSQL
PREFIX_LDFLAGS = $(TARGET_LDFLAGS) $(TARGET_NLS_LIBS)
PREFIX_DEPENDENCIES = postgresql
PREFIX_SUPPORTS_IN_SOURCE_BUILD = NO

define PREFIX_BUILD_CMDS
	cd $(@D) && $(MAKE1) PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config $(TARGET_CONFIGURE_OPTS) PKGCONFIG="$(PKG_CONFIG_HOST_BINARY)" OPTFLAGS="" -C $(@D)
endef

define PREFIX_INSTALL_TARGET_CMDS
	$(INSTALL) $(@D)/prefix.so $(TARGET_DIR)/usr/lib/postgresql
	$(INSTALL) $(@D)/prefix.control $(TARGET_DIR)/usr/share/postgresql/extension
	$(INSTALL) $(@D)/*.sql $(TARGET_DIR)/usr/share/postgresql/extension
endef

$(eval $(generic-package))