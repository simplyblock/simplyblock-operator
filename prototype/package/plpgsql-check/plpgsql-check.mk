PLPGSQL_CHECK_VERSION = 2.8.5
PLPGSQL_CHECK_SOURCE = v$(PLPGSQL_CHECK_VERSION).tar.gz
PLPGSQL_CHECK_SITE = https://github.com/okbob/plpgsql_check/archive/refs/tags
PLPGSQL_CHECK_LICENSE = PostgreSQL
PLPGSQL_CHECK_LDFLAGS = $(TARGET_LDFLAGS) $(TARGET_NLS_LIBS)
PLPGSQL_CHECK_DEPENDENCIES = postgresql

define PLPGSQL_CHECK_BUILD_CMDS
	cd $(@D) && $(MAKE1) PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config $(TARGET_CONFIGURE_OPTS) PKGCONFIG="$(PKG_CONFIG_HOST_BINARY)" OPTFLAGS="" -C $(@D)
endef

define PLPGSQL_CHECK_INSTALL_TARGET_CMDS
	$(INSTALL) $(@D)/plpgsql_check.so $(TARGET_DIR)/usr/lib/postgresql
	$(INSTALL) $(@D)/plpgsql_check.control $(TARGET_DIR)/usr/share/postgresql/extension
	$(INSTALL) $(@D)/*.sql $(TARGET_DIR)/usr/share/postgresql/extension
endef

$(eval $(generic-package))