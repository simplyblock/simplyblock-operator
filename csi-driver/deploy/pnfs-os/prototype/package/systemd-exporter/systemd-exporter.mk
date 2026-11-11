SYSTEMD_EXPORTER_VERSION = 0.7.0
SYSTEMD_EXPORTER_SOURCE = systemd_exporter-$(SYSTEMD_EXPORTER_VERSION).linux-amd64.tar.gz
SYSTEMD_EXPORTER_SITE = https://github.com/prometheus-community/systemd_exporter/releases/download/v$(SYSTEMD_EXPORTER_VERSION)
SYSTEMD_EXPORTER_LICENSE = Apache2.0

define SYSTEMD_EXPORTER_BUILD_CMDS
	:
endef

define SYSTEMD_EXPORTER_INSTALL_TARGET_CMDS
	$(INSTALL) $(@D)/systemd_exporter $(TARGET_DIR)/usr/sbin
endef

define SYSTEMD_EXPORTER_INSTALL_INIT_SYSTEMD
	$(INSTALL) -D -m 644 $(SYSTEMD_EXPORTER_PKGDIR)/systemd-exporter.service \
		$(TARGET_DIR)/usr/lib/systemd/system/systemd-exporter.service
endef

define SYSTEMD_EXPORTER_USERS
	systemd-exporter -1 systemd-exporter -1 * - - - systemd-exporter
endef

$(eval $(generic-package))
