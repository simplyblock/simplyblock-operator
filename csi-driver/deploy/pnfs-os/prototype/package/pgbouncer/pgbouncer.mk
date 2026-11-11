PGBOUNCER_VERSION = 1_25_1
PGBOUNCER_SOURCE = pgbouncer_$(PGBOUNCER_VERSION).tar.gz
PGBOUNCER_SITE = https://github.com/pgbouncer/pgbouncer/archive/refs/tags
PGBOUNCER_LICENSE = ISC
PGBOUNCER_DEPENDENCIES = host-pkgconf libopenssl libevent c-ares linux-pam
PGBOUNCER_MAKE_ENV = \
	$(TARGET_MAKE_ENV) \
	CFLAGS="$(TARGET_CFLAGS)" \
	PKG_CONFIG="$(HOST_DIR)/bin/pkg-config"

define PGBOUNCER_CONFIGURE_CMDS
	cd $(@D) && \
	./autogen.sh && \
	$(PGBOUNCER_MAKE_ENV) \
	./configure --prefix=/usr --with-pam
endef

define PGBOUNCER_BUILD_CMDS
	cd $(@D) && \
	$(PGBOUNCER_MAKE_ENV) \
	$(MAKE1) $(TARGET_CONFIGURE_OPTS) PKGCONFIG="$(PKG_CONFIG_HOST_BINARY)" OPTFLAGS="" -C $(@D)
endef

define PGBOUNCER_INSTALL_TARGET_CMDS
	$(INSTALL) $(@D)/pgbouncer $(TARGET_DIR)/usr/sbin/pgbouncer
endef

define PGBOUNCER_INSTALL_INIT_SYSTEMD
	$(INSTALL) -D -m 644 $(PGBOUNCER_PKGDIR)/pgbouncer.service \
		$(TARGET_DIR)/usr/lib/systemd/system/pgbouncer.service
endef

define PGBOUNCER_USERS
	pgbouncer -1 pgbouncer -1 * - - - pgbouncer
endef

$(eval $(generic-package))
