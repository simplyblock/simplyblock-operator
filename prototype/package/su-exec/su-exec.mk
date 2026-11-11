SU_EXEC_VERSION = 0.3
SU_EXEC_SOURCE = v$(SU_EXEC_VERSION).tar.gz
SU_EXEC_SITE = https://github.com/ncopa/su-exec/archive/refs/tags
SU_EXEC_LICENSE = MIT

define SU_EXEC_BUILD_CMDS
	cd $(@D) && $(MAKE1) $(TARGET_CONFIGURE_OPTS) PKGCONFIG="$(PKG_CONFIG_HOST_BINARY)" OPTFLAGS="" -C $(@D)
endef

define SU_EXEC_INSTALL_TARGET_CMDS
	mkdir -p $(TARGET_DIR)/neonvm/bin
	$(INSTALL) $(@D)/su-exec $(TARGET_DIR)/neonvm/bin
endef

$(eval $(generic-package))