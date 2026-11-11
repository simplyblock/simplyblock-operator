PG_IVM_VERSION = 1.13
PG_IVM_SOURCE = v$(PG_IVM_VERSION).tar.gz
PG_IVM_SITE = https://github.com/sraoss/pg_ivm/archive/refs/tags
PG_IVM_LICENSE = PostgreSQL
PG_IVM_LDFLAGS = $(TARGET_LDFLAGS) $(TARGET_NLS_LIBS)
PG_IVM_DEPENDENCIES = postgresql

define PG_IVM_BUILD_CMDS
	cd $(@D) && $(MAKE1) USE_PGXS=1 PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config $(TARGET_CONFIGURE_OPTS) PKGCONFIG="$(PKG_CONFIG_HOST_BINARY)" OPTFLAGS="" -C $(@D)
endef

define PG_IVM_INSTALL_TARGET_CMDS
	$(INSTALL) $(@D)/pg_ivm.so $(TARGET_DIR)/usr/lib/postgresql
	$(INSTALL) $(@D)/pg_ivm.control $(TARGET_DIR)/usr/share/postgresql/extension
	$(INSTALL) $(@D)/*.sql $(TARGET_DIR)/usr/share/postgresql/extension
endef

$(eval $(generic-package))