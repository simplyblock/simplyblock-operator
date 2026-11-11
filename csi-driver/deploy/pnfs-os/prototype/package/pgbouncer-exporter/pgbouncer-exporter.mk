PGBOUNCER_EXPORTER_VERSION = 0.11.0
PGBOUNCER_EXPORTER_SOURCE = pgbouncer_exporter-$(PGBOUNCER_EXPORTER_VERSION).linux-amd64.tar.gz
PGBOUNCER_EXPORTER_SITE = https://github.com/prometheus-community/pgbouncer_exporter/releases/download/v$(PGBOUNCER_EXPORTER_VERSION)
PGBOUNCER_EXPORTER_LICENSE = Apache2.0

define PGBOUNCER_EXPORTER_BUILD_CMDS
	:
endef

define PGBOUNCER_EXPORTER_INSTALL_TARGET_CMDS
	$(INSTALL) $(@D)/pgbouncer_exporter $(TARGET_DIR)/usr/sbin
endef

define PGBOUNCER_EXPORTER_INSTALL_INIT_SYSTEMD
	$(INSTALL) -D -m 644 $(PGBOUNCER_EXPORTER_PKGDIR)/pgbouncer-exporter.service \
		$(TARGET_DIR)/usr/lib/systemd/system/pgbouncer-exporter.service
endef

define PGBOUNCER_EXPORTER_USERS
	pgbouncer-exporter -1 pgbouncer-exporter -1 * - - - pgbouncer-exporter
endef

$(eval $(generic-package))
