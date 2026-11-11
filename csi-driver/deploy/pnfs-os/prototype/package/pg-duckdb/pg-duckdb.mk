PG_DUCKDB_VERSION = v1.1.1
PG_DUCKDB_SITE = git://github.com/duckdb/pg_duckdb.git
PG_DUCKDB_LICENSE = PostgreSQL
PG_DUCKDB_LDFLAGS = $(TARGET_LDFLAGS) $(TARGET_NLS_LIBS)
PG_DUCKDB_DEPENDENCIES = postgresql
PG_DUCKDB_INSTALL_STAGING = YES

PG_DUCKDB_TOOLCHAIN_OPS = AR="$(TARGET_AR)" \
                          AS="$(TARGET_AS)" \
                          LD="$(TARGET_LD)" \
                          NM="$(TARGET_NM)" \
                          CC="$(TARGET_CC)" \
                          GCC="$(TARGET_GCC)" \
                          CPP="$(TARGET_CPP)" \
                          CXX="$(TARGET_CXX)" \
                          FC="$(TARGET_FC)" \
                          F77="$(TARGET_F77)" \
                          RANLIB="$(TARGET_RANLIB)" \
                          READELF="$(TARGET_READELF)" \
                          STRIP="/bin/true" \
                          OBJCOPY="$(TARGET_OBJCOPY)" \
                          OBJDUMP="$(TARGET_OBJDUMP)"

ifeq ($(BR2_aarch64),y)
DUCKDB_PLATFORM = "linux_arm64_glibc"
else ifeq ($(BR2_x86_64),y)
DUCKDB_PLATFORM = "linux_amd64_glibc"
endif
# PGXS build: use the pg_config from staging so it picks the right include/lib dirs
define PG_DUCKDB_BUILD_CMDS
	cd $(@D) && \
	$(TARGET_CONFIGURE_OPTS) $(PG_DUCKDB_TOOLCHAIN_OPS) \
		FORCE_COLORED_OUTPUT=0 \
		DUCKDB_EXPLICIT_PLATFORM="$(DUCKDB_PLATFORM)" DUCKDB_PLATFORM="$(DUCKDB_PLATFORM)" \
		DUCKDB_BUILD=ReleaseStatic GIT_DIR=.git $(MAKE) -C $(@D) USE_PGXS=1 \
		$(PG_DUCKDB_MAKE_OPTS) PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config \
		$(PG_DUCKDB_TOOLCHAIN_OPS)
endef

# Install into target rootfs. PGXS honors DESTDIR.
define PG_DUCKDB_INSTALL_TARGET_CMDS
	cd $(@D) && \
	$(TARGET_MAKE_ENV) GIT_DIR=.git $(MAKE) -C $(@D) USE_PGXS=1 \
		PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config \
		DESTDIR=$(TARGET_DIR) install
endef

# Optional: install into staging too (useful if other target packages need it at build time)
define PG_DUCKDB_INSTALL_STAGING_CMDS
	cd $(@D) && \
	$(TARGET_MAKE_ENV) GIT_DIR=.git $(MAKE) -C $(@D) USE_PGXS=1 \
		PG_CONFIG=$(STAGING_DIR)/usr/bin/pg_config \
		DESTDIR=$(STAGING_DIR) install
endef

# For a successful build, pg_duckdb (and duckdb) need to recursively download
# dependencies. Hence, we'll re-hydrate the git metadata of the repository.
define PG_DUCKDB_INIT_GIT_REPO
	cd $(@D) && \
	if ! [ -d .git ]; then \
	git init && \
	git remote add origin $(PG_DUCKDB_SITE) && \
	git fetch --tags origin && \
	git checkout $(PG_DUCKDB_VERSION) -f && \
	sed -i 's/-L\$$(PG_LIB)/-Llibpq/g' Makefile; \
	fi
endef

PG_DUCKDB_POST_EXTRACT_HOOKS += PG_DUCKDB_INIT_GIT_REPO

$(eval $(generic-package))