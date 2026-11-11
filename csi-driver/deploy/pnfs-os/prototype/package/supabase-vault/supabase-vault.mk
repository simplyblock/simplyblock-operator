SUPABASE_VAULT_VERSION = 0.3.1
SUPABASE_VAULT_SOURCE = v$(SUPABASE_VAULT_VERSION).tar.gz
SUPABASE_VAULT_SITE = https://github.com/supabase/vault/archive/refs/tags
SUPABASE_VAULT_LICENSE = PostgreSQL
SUPABASE_VAULT_LDFLAGS = $(TARGET_LDFLAGS) $(TARGET_NLS_LIBS)
SUPABASE_VAULT_DEPENDENCIES = postgresql libsodium
SUPABASE_VAULT_INSTALL_STAGING = YES

# PGXS build: use the pg_config from staging so it picks the right include/lib dirs
define SUPABASE_VAULT_BUILD_CMDS
	$(TARGET_MAKE_ENV) $(MAKE) -C $(@D) USE_PGXS=1 \
		PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config
endef

# Install into target rootfs. PGXS honors DESTDIR.
define SUPABASE_VAULT_INSTALL_TARGET_CMDS
	$(TARGET_MAKE_ENV) $(MAKE) -C $(@D) USE_PGXS=1 \
		PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config \
		DESTDIR=$(TARGET_DIR) install
endef

# Optional: install into staging too (useful if other target packages need it at build time)
define SUPABASE_VAULT_INSTALL_STAGING_CMDS
	$(TARGET_MAKE_ENV) $(MAKE) -C $(@D) USE_PGXS=1 \
		PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config \
		DESTDIR=$(STAGING_DIR) install
endef

$(eval $(generic-package))