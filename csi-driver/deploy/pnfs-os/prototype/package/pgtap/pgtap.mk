PGTAP_VERSION = 1.3.4
PGTAP_SOURCE = v$(PGTAP_VERSION).tar.gz
PGTAP_SITE = https://github.com/theory/pgtap/archive/refs/tags
PGTAP_LICENSE = PostgreSQL
PGTAP_CONFIG_SCRIPTS = pg_config
PGTAP_LDFLAGS = $(TARGET_LDFLAGS) $(TARGET_NLS_LIBS)
PGTAP_DEPENDENCIES = postgresql
PGTAP_SUPPORTS_IN_SOURCE_BUILD = NO

define PGTAP_BUILD_CMDS
	cd $(@D) && $(MAKE1) PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config $(TARGET_CONFIGURE_OPTS) PKGCONFIG="$(PKG_CONFIG_HOST_BINARY)" OPTFLAGS="" -C $(@D)
endef

define PGTAP_INSTALL_TARGET_CMDS
	cd $(@D) && $(MAKE1) install PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config prefix=$(TARGET_DIR)/usr
endef

$(eval $(generic-package))