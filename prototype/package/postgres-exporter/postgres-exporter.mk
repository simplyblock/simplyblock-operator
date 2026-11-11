POSTGRES_EXPORTER_VERSION = 0.18.1
POSTGRES_EXPORTER_SOURCE = postgres_exporter-$(POSTGRES_EXPORTER_VERSION).linux-amd64.tar.gz
POSTGRES_EXPORTER_SITE = https://github.com/prometheus-community/postgres_exporter/releases/download/v$(POSTGRES_EXPORTER_VERSION)
POSTGRES_EXPORTER_LICENSE = Apache2.0

define POSTGRES_EXPORTER_BUILD_CMDS
	:
endef

define POSTGRES_EXPORTER_INSTALL_TARGET_CMDS
	$(INSTALL) $(@D)/postgres_exporter $(TARGET_DIR)/usr/sbin
endef

define POSTGRES_EXPORTER_INSTALL_INIT_SYSTEMD
	$(INSTALL) -D -m 644 $(POSTGRES_EXPORTER_PKGDIR)/postgres-exporter.service \
		$(TARGET_DIR)/usr/lib/systemd/system/postgres-exporter.service
endef

define POSTGRES_EXPORTER_USERS
	postgres-exporter -1 postgres-exporter -1 * - - - postgres-exporter
endef

$(eval $(generic-package))
