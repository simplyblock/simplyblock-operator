VM_MONITOR_VERSION = release-proxy-8853
VM_MONITOR_SOURCE = $(VM_MONITOR_VERSION).tar.gz
VM_MONITOR_SITE = https://github.com/neondatabase/neon/archive/refs/tags
VM_MONITOR_LICENSE = Apache2.0
VM_MONITOR_SUBDIR = libs/vm_monitor

define VM_MONITOR_INSTALL_INIT_SYSTEMD
	$(INSTALL) -D -m 644 $(VM_MONITOR_PKGDIR)/vm-monitor.service \
		$(TARGET_DIR)/usr/lib/systemd/system/vm-monitor.service
endef

define VM_MONITOR_USERS
	vm-monitor -1 vm-monitor -1 - - - - vm-monitor
endef

$(eval $(cargo-package))
