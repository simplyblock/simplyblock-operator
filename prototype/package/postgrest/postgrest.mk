POSTGREST_VERSION = 14.3
POSTGREST_SOURCE = postgrest-v$(POSTGREST_VERSION)-linux-static-x86-64.tar.xz
POSTGREST_SITE = https://github.com/PostgREST/postgrest/releases/download/v$(POSTGREST_VERSION)
POSTGREST_LICENSE = PostgreSQL
POSTGREST_LDFLAGS = $(TARGET_LDFLAGS) $(TARGET_NLS_LIBS)
POSTGREST_DEPENDENCIES = postgresql
POSTGREST_STRIP_COMPONENTS = 0

define POSTGREST_BUILD_CMDS
	:
endef

define POSTGREST_INSTALL_TARGET_CMDS
	$(INSTALL) $(@D)/postgrest $(TARGET_DIR)/usr/sbin/postgrest
endef

define POSTGREST_INSTALL_INIT_SYSTEMD
	$(INSTALL) -D -m 644 $(POSTGREST_PKGDIR)/postgrest.service \
		$(TARGET_DIR)/usr/lib/systemd/system/postgrest.service
endef

define POSTGREST_USERS
	postgrest -1 postgrest -1 * - - - postgrest
endef

$(eval $(generic-package))
