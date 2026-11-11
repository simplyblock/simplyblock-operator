SB_STORAGE_VERSION = 1.33.2
SB_STORAGE_SOURCE = v$(SB_STORAGE_VERSION).tar.gz
SB_STORAGE_SITE = https://github.com/supabase/storage/archive/refs/tags
SB_STORAGE_LICENSE = Apache2.0
SB_STORAGE_LICENSE_FILES = LICENSE
SB_STORAGE_DEPENDENCIES = nodejs host-nodejs

ifeq ($(BR2_arm),y)
SB_STORAGE_TARGET_ARCH = arm
else ifeq ($(BR2_aarch64),y)
SB_STORAGE_TARGET_ARCH = arm64
else ifeq ($(BR2_x86_64),y)
SB_STORAGE_TARGET_ARCH = amd64
endif

define SB_STORAGE_BUILD_CMDS
    cd $(@D) && \
    $(NPM) clean-install && \
    $(NPM) run build && \
    $(NPM) prune --omit=dev --omit=optional && \
    $(NPM) exec -- modclean -r --patterns="default:*"
endef

define SB_STORAGE_INSTALL_TARGET_CMDS
    @mkdir -p $(TARGET_DIR)/opt/storage
    @mkdir -p $(TARGET_DIR)/opt/storage/node_modules
    @mkdir -p $(TARGET_DIR)/opt/storage/dist
    @cp -r $(@D)/migrations $(TARGET_DIR)/opt/storage/
    @cp -r $(@D)/node_modules/* $(TARGET_DIR)/opt/storage/node_modules/
    @cp -r $(@D)/dist/* $(TARGET_DIR)/opt/storage/dist/
    $(INSTALL) -D -m 0755 $(@D)/package.json $(TARGET_DIR)/opt/storage/package.json
    @rm -rf $(TARGET_DIR)/opt/storage/node_modules/bare-fs/prebuilds/android*
    @rm -rf $(TARGET_DIR)/opt/storage/node_modules/bare-os/prebuilds/android*
    @rm -rf $(TARGET_DIR)/opt/storage/node_modules/bare-url/prebuilds/android*
    @rm -rf $(TARGET_DIR)/opt/storage/node_modules/bare-fs/prebuilds/ios*
    @rm -rf $(TARGET_DIR)/opt/storage/node_modules/bare-os/prebuilds/ios*
    @rm -rf $(TARGET_DIR)/opt/storage/node_modules/bare-url/prebuilds/ios*
    @rm -rf $(TARGET_DIR)/opt/storage/node_modules/bare-fs/prebuilds/darwin*
    @rm -rf $(TARGET_DIR)/opt/storage/node_modules/bare-os/prebuilds/darwin*
    @rm -rf $(TARGET_DIR)/opt/storage/node_modules/bare-url/prebuilds/darwin*
    @rm -rf $(TARGET_DIR)/opt/storage/node_modules/bare-fs/prebuilds/win32*
    @rm -rf $(TARGET_DIR)/opt/storage/node_modules/bare-os/prebuilds/win32*
    @rm -rf $(TARGET_DIR)/opt/storage/node_modules/bare-url/prebuilds/win32*
endef

define SB_STORAGE_CLEAN_AMD64
    @rm -rf $(TARGET_DIR)/opt/storage/node_modules/bare-fs/prebuilds/linux-arm64
    @rm -rf $(TARGET_DIR)/opt/storage/node_modules/bare-os/prebuilds/linux-arm64
    @rm -rf $(TARGET_DIR)/opt/storage/node_modules/bare-url/prebuilds/linux-arm64
endef

define SB_STORAGE_CLEAN_AARCH64
    @rm -rf $(TARGET_DIR)/opt/storage/node_modules/bare-fs/prebuilds/linux-x64
    @rm -rf $(TARGET_DIR)/opt/storage/node_modules/bare-os/prebuilds/linux-x64
    @rm -rf $(TARGET_DIR)/opt/storage/node_modules/bare-url/prebuilds/linux-x64
endef

ifeq ("$(SB_STORAGE_TARGET_ARCH)", "amd64")
    SB_STORAGE_POST_INSTALL_TARGET_HOOKS+=SB_STORAGE_CLEAN_AMD64
else
    SB_STORAGE_POST_INSTALL_TARGET_HOOKS+=SB_STORAGE_CLEAN_AARCH64
endif

define SB_STORAGE_INSTALL_INIT_SYSTEMD
	$(INSTALL) -D -m 644 $(SB_STORAGE_PKGDIR)/storage.service \
		$(TARGET_DIR)/usr/lib/systemd/system/storage.service
endef

define SB_STORAGE_USERS
	storage -1 storage -1 * - - - storage
endef

$(eval $(generic-package))
