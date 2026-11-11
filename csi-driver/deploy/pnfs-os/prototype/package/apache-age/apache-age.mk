APACHE_AGE_VERSION = 1.7.0-rc0
APACHE_AGE_SOURCE = v$(APACHE_AGE_VERSION).tar.gz
APACHE_AGE_SITE = https://github.com/apache/age/archive/refs/tags/PG18
APACHE_AGE_LICENSE = Apache-2.0
APACHE_AGE_LICENSE_FILES = LICENSE
APACHE_AGE_DEPENDENCIES = postgresql

define APACHE_AGE_BUILD_CMDS
	$(TARGET_MAKE_ENV) $(MAKE) -C $(@D) USE_PGXS=1 \
		PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config
endef

define APACHE_AGE_INSTALL_TARGET_CMDS
	$(INSTALL) -D -m 0755 $(@D)/age.so $(TARGET_DIR)/usr/lib/postgresql/age.so
	$(INSTALL) -D -m 0644 $(@D)/age.control $(TARGET_DIR)/usr/share/postgresql/extension/age.control
	$(INSTALL) -D -m 0644 $(@D)/age--1.7.0.sql $(TARGET_DIR)/usr/share/postgresql/extension/age--1.7.0.sql
	$(INSTALL) -D -m 0644 $(@D)/age--1.7.0--y.y.y.sql $(TARGET_DIR)/usr/share/postgresql/extension/age--1.7.0--y.y.y.sql
endef

$(eval $(generic-package))
