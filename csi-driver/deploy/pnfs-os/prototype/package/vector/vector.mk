VECTOR_VERSION = 0.52.0
VECTOR_SOURCE = vector-$(VECTOR_VERSION)-$(VECTOR_TARGET_ARCH)-unknown-linux-gnu.tar.gz
VECTOR_SITE = https://github.com/vectordotdev/vector/releases/download/v$(VECTOR_VERSION)
VECTOR_LICENSE = MPL-2.0

ifeq ($(BR2_aarch64),y)
VECTOR_TARGET_ARCH = aarch64
else ifeq ($(BR2_x86_64),y)
VECTOR_TARGET_ARCH = x86_64
endif

define VECTOR_BUILD_CMDS
	upx $(@D)/vector-$(VECTOR_TARGET_ARCH)-unknown-linux-gnu/bin/vector
endef

define VECTOR_INSTALL_TARGET_CMDS
	mkdir -p $(TARGET_DIR)/sbin
	$(INSTALL) $(@D)/vector-$(VECTOR_TARGET_ARCH)-unknown-linux-gnu/bin/vector $(TARGET_DIR)/sbin/vector
endef

define VECTOR_INSTALL_INIT_SYSTEMD
	$(INSTALL) -D -m 644 $(VECTOR_PKGDIR)/vector.service \
		$(TARGET_DIR)/usr/lib/systemd/system/vector.service
endef

define VECTOR_USERS
	vector -1 vector -1 * /var/lib/vector - systemd-journal vector
endef

$(eval $(generic-package))
