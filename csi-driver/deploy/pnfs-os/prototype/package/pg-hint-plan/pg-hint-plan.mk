PG_HINT_PLAN_VERSION = 18_1_8_0
PG_HINT_PLAN_SOURCE = REL$(PG_HINT_PLAN_VERSION).tar.gz
PG_HINT_PLAN_SITE = https://github.com/ossc-db/pg_hint_plan/archive/refs/tags
PG_HINT_PLAN_LICENSE = PostgreSQL
PG_HINT_PLAN_LDFLAGS = $(TARGET_LDFLAGS) $(TARGET_NLS_LIBS)
PG_HINT_PLAN_DEPENDENCIES = postgresql

define PG_HINT_PLAN_BUILD_CMDS
	cd $(@D) && $(MAKE1) PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config $(TARGET_CONFIGURE_OPTS) PKGCONFIG="$(PKG_CONFIG_HOST_BINARY)" OPTFLAGS="" -C $(@D)
endef

define PG_HINT_PLAN_INSTALL_TARGET_CMDS
	$(INSTALL) $(@D)/pg_hint_plan.so $(TARGET_DIR)/usr/lib/postgresql
	$(INSTALL) $(@D)/pg_hint_plan.control $(TARGET_DIR)/usr/share/postgresql/extension
	$(INSTALL) $(@D)/*.sql $(TARGET_DIR)/usr/share/postgresql/extension
endef

$(eval $(generic-package))