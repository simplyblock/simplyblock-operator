PGVECTOR_VERSION = 0.8.1
PGVECTOR_SOURCE = v$(PGVECTOR_VERSION).tar.gz
PGVECTOR_SITE =  https://github.com/pgvector/pgvector/archive/refs/tags
PGVECTOR_LICENSE = PostgreSQL
PGVECTOR_LDFLAGS = $(TARGET_LDFLAGS) $(TARGET_NLS_LIBS)
PGVECTOR_DEPENDENCIES = postgresql

define PGVECTOR_BUILD_CMDS
	cd $(@D) && $(MAKE1) PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config $(TARGET_CONFIGURE_OPTS) PKGCONFIG="$(PKG_CONFIG_HOST_BINARY)" OPTFLAGS="" -C $(@D)
endef

define PGVECTOR_INSTALL_TARGET_CMDS
	$(INSTALL) $(@D)/vector.so $(TARGET_DIR)/usr/lib/postgresql
	$(INSTALL) $(@D)/vector.control $(TARGET_DIR)/usr/share/postgresql/extension
	$(INSTALL) $(@D)/sql/* $(TARGET_DIR)/usr/share/postgresql/extension
endef

$(eval $(generic-package))