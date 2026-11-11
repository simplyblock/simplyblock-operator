HASHIDS_VERSION = 1.2.1
HASHIDS_SOURCE = v$(HASHIDS_VERSION).tar.gz
HASHIDS_SITE = https://github.com/iCyberon/pg_hashids/archive/refs/tags
HASHIDS_LICENSE = PostgreSQL
HASHIDS_LDFLAGS = $(TARGET_LDFLAGS) $(TARGET_NLS_LIBS)
HASHIDS_DEPENDENCIES = postgresql
HASHIDS_SUPPORTS_IN_SOURCE_BUILD = NO

define HASHIDS_BUILD_CMDS
	cd $(@D) && $(MAKE1) PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config $(TARGET_CONFIGURE_OPTS) PKGCONFIG="$(PKG_CONFIG_HOST_BINARY)" OPTFLAGS="" -C $(@D)
endef

define HASHIDS_INSTALL_TARGET_CMDS
	$(INSTALL) $(@D)/pg_hashids.so $(TARGET_DIR)/usr/lib/postgresql
	$(INSTALL) $(@D)/pg_hashids.control $(TARGET_DIR)/usr/share/postgresql/extension
	$(INSTALL) $(@D)/*.sql $(TARGET_DIR)/usr/share/postgresql/extension
endef

$(eval $(generic-package))