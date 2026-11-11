DBMATE_VERSION = 2.29.3
DBMATE_SOURCE = dbmate-linux-amd64
DBMATE_SITE = https://github.com/amacneil/dbmate/releases/download/v$(DBMATE_VERSION)
DBMATE_LICENSE = MIT

define DBMATE_EXTRACT_CMDS
	cp $(DBMATE_DL_DIR)/$(DBMATE_SOURCE) $(@D)/dbmate
endef

define DBMATE_BUILD_CMDS
	:
endef

define DBMATE_INSTALL_TARGET_CMDS
	$(INSTALL) -D -m 0755 $(@D)/dbmate $(TARGET_DIR)/usr/bin/dbmate
endef

$(eval $(generic-package))
