PG_META_VERSION = 0.95.1
PG_META_SOURCE = v$(PG_META_VERSION).tar.gz
PG_META_SITE = https://github.com/supabase/postgres-meta/archive/refs/tags
PG_META_LICENSE = Apache2.0
PG_META_LICENSE_FILES = LICENSE
PG_META_DEPENDENCIES = nodejs host-nodejs

ifeq ($(BR2_arm),y)
PG_META_TARGET_ARCH = arm
else ifeq ($(BR2_aarch64),y)
PG_META_TARGET_ARCH = arm64
else ifeq ($(BR2_x86_64),y)
PG_META_TARGET_ARCH = amd64
endif

define PG_META_BUILD_CMDS
	cd $(@D) && \
	$(NPM) clean-install && \
	$(NPM) run build && \
	$(NPM) prune --omit=dev --omit=optional && \
	$(NPM) exec -- modclean -r --patterns="default:*"
endef

define PG_META_INSTALL_TARGET_CMDS
	@mkdir -p $(TARGET_DIR)/opt/meta
	@mkdir -p $(TARGET_DIR)/opt/meta/node_modules
	@mkdir -p $(TARGET_DIR)/opt/meta/dist
	@cp -r $(@D)/node_modules/* $(TARGET_DIR)/opt/meta/node_modules/
	@cp -r $(@D)/dist/* $(TARGET_DIR)/opt/meta/dist/
	$(INSTALL) -D -m 0755 $(@D)/package.json $(TARGET_DIR)/opt/meta/package.json
    @rm -rf $(TARGET_DIR)/opt/meta/node_modules/@sentry-internal/node-cpu-profiler/lib/sentry_cpu_profiler-darwin*
    @rm -rf $(TARGET_DIR)/opt/meta/node_modules/@sentry-internal/node-cpu-profiler/lib/sentry_cpu_profiler-win32*
endef

define PG_META_CLEAN_AMD64
    @rm -rf $(TARGET_DIR)/opt/meta/node_modules/@sentry-internal/node-cpu-profiler/lib/sentry_cpu_profiler-linux-arm64*
    @rm -rf $(TARGET_DIR)/opt/meta/node_modules/@sentry-internal/node-cpu-profiler/lib/sentry_cpu_profiler-linux-x64-musl*
endef

define PG_META_CLEAN_AARCH64
    @rm -rf $(TARGET_DIR)/opt/meta/node_modules/@sentry-internal/node-cpu-profiler/lib/sentry_cpu_profiler-linux-x64*
    @rm -rf $(TARGET_DIR)/opt/meta/node_modules/@sentry-internal/node-cpu-profiler/lib/sentry_cpu_profiler-linux-arm64-musl*
endef

ifeq ("$(PG_META_TARGET_ARCH)", "amd64")
	PG_META_POST_INSTALL_TARGET_HOOKS+=PG_META_CLEAN_AMD64
else
	PG_META_POST_INSTALL_TARGET_HOOKS+=PG_META_CLEAN_AARCH64
endif

define PG_META_USERS
	postgres-meta -1 postgres-meta -1 * - - - Postgres meta
endef

define PG_META_INSTALL_INIT_SYSTEMD
	$(INSTALL) -D -m 644 $(PG_META_PKGDIR)/postgres-meta.service \
		$(TARGET_DIR)/usr/lib/systemd/system/postgres-meta.service
endef

$(eval $(generic-package))
