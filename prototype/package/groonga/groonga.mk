GROONGA_VERSION = 15.2.1
GROONGA_SOURCE = v$(GROONGA_VERSION).tar.gz
GROONGA_SITE = https://github.com/groonga/groonga/archive/refs/tags
GROONGA_LICENSE = LGPL-2.1
GROONGA_INSTALL_STAGING = true
GROONGA_DEPENDENCIES = host-pkgconf xxhash
GROONGA_CONF_OPTS = --preset=release-default

define GROONGA_SETUP_PRE_CONFIGURE
	cd $(@D) && ./setup.sh
endef

GROONGA_PRE_CONFIGURE_HOOKS += GROONGA_SETUP_PRE_CONFIGURE

$(eval $(cmake-package))