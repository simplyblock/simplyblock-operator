IP4R_VERSION = 2.4.2
IP4R_SOURCE = $(IP4R_VERSION).tar.gz
IP4R_SITE = https://github.com/RhodiumToad/ip4r/archive/refs/tags
IP4R_LICENSE = PostgreSQL
IP4R_LDFLAGS = $(TARGET_LDFLAGS) $(TARGET_NLS_LIBS)
IP4R_DEPENDENCIES = postgresql
IP4R_SUPPORTS_IN_SOURCE_BUILD = NO

define IP4R_BUILD_CMDS
	cd $(@D) && $(MAKE1) PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config $(TARGET_CONFIGURE_OPTS) PKGCONFIG="$(PKG_CONFIG_HOST_BINARY)" OPTFLAGS="" -C $(@D)
endef

define IP4R_INSTALL_TARGET_CMDS
	$(INSTALL) $(@D)/ip4r.so $(TARGET_DIR)/usr/lib/postgresql
	$(INSTALL) $(@D)/ip4r.control $(TARGET_DIR)/usr/share/postgresql/extension
	$(INSTALL) $(@D)/sql/*.sql $(TARGET_DIR)/usr/share/postgresql/extension
endef

$(eval $(generic-package))