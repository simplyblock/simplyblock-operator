PGX_ULID_VERSION = dfb4f29205f337a648f1eca5a8c14ac1011510c0
PGX_ULID_SITE = $(call github,pksunkara,pgx_ulid,$(PGX_ULID_VERSION))
PGX_ULID_LICENSE = MIT
PGX_ULID_LICENSE_FILES = LICENSE

PGX_ULID_DEPENDENCIES = postgresql host-cargo-pgrx
PGX_ULID_CARGO_TARGET_ENV = $(call UPPERCASE,$(subst -,_,$(RUSTC_TARGET_NAME)))

PGX_ULID_CARGO_ENV = \
	PGRX_PG_CONFIG_PATH="$(PGX_ULID_PG_CONFIG_WRAPPER)" \
	PGRX_BINDGEN_NO_DETECT_INCLUDES=1 \
	BINDGEN_EXTRA_CLANG_ARGS_$(subst -,_,$(RUSTC_TARGET_NAME))="--target=$(RUSTC_TARGET_NAME) --sysroot=$(STAGING_DIR)"

PGX_ULID_CARGO_BUILD_OPTS += --features pg18
PGX_ULID_CARGO_INSTALL_OPTS =
PGX_ULID_CARGO_INSTALL_TARGET_OPTS =

PGX_ULID_PGRX_OUTDIR = $(@D)/pgrx-out
PGX_ULID_PG_CONFIG_WRAPPER = $(@D)/pg_config.br

define PGX_ULID_CREATE_PG_CONFIG_WRAPPER
	{ \
	  echo '#!/bin/sh'; \
	  echo 'SYSROOT="$(STAGING_DIR)"'; \
	  echo 'HOST_PG_CONFIG="$${HOST_PG_CONFIG:-/usr/bin/pg_config}"'; \
	  echo 'TARGET_TRIPLE="$${TARGET:-}"'; \
	  echo ''; \
	  echo 'is_target_x86_64=0'; \
	  echo 'case "$$TARGET_TRIPLE" in'; \
	  echo '  "$(RUSTC_TARGET_NAME)"|x86_64-*) is_target_x86_64=1 ;;'; \
	  echo '  *) exec "$$HOST_PG_CONFIG" "$$@";;'; \
	  echo 'esac'; \
	  echo ''; \
	  echo 'case "$$1" in'; \
	  echo '  --version) echo "PostgreSQL $(POSTGRESQL_VERSION)";;'; \
	  echo '  --configure) echo "--prefix=/usr";;'; \
	  echo '  --sharedir) echo "$$SYSROOT/usr/share/postgresql";;'; \
	  echo '  --libdir|--pkglibdir)'; \
	  echo '    if [ "$$is_target_x86_64" -eq 1 ]; then echo "$$SYSROOT/usr/lib"; else echo "/usr/lib"; fi'; \
	  echo '    ;;'; \
	  echo '  --includedir|--pkgincludedir) echo "$$SYSROOT/usr/include/postgresql";;'; \
	  echo '  --includedir-server) echo "$$SYSROOT/usr/include/postgresql/server";;'; \
	  echo '  --cppflags)'; \
	  echo '    if [ "$$is_target_x86_64" -eq 1 ]; then'; \
	  echo '      # Target build: use sysroot libc headers'; \
	  echo '      echo "--sysroot=$$SYSROOT -I$$SYSROOT/usr/include -I$$SYSROOT/usr/include/postgresql -I$$SYSROOT/usr/include/postgresql/server"'; \
	  echo '    else'; \
	  echo '      # Host pgrx_embed build: DO NOT use sysroot libc headers (prevents gnu/stubs-32.h failures)'; \
	  echo '      echo "-I$$SYSROOT/usr/include/postgresql -I$$SYSROOT/usr/include/postgresql/server"'; \
	  echo '    fi'; \
	  echo '    ;;'; \
	  echo '  *) echo "";;'; \
	  echo 'esac'; \
	} >"$(PGX_ULID_PG_CONFIG_WRAPPER)"
	chmod +x "$(PGX_ULID_PG_CONFIG_WRAPPER)"
endef

PGX_ULID_PRE_BUILD_HOOKS += PGX_ULID_CREATE_PG_CONFIG_WRAPPER

define PGX_ULID_GENERATE_PGRX_PACKAGE
	rm -rf $(PGX_ULID_PGRX_OUTDIR)
	mkdir -p $(PGX_ULID_PGRX_OUTDIR) $(@D)/.pgrx

	env -u CC -u CXX -u AR -u RANLIB -u CFLAGS -u CXXFLAGS -u LDFLAGS \
		PGRX_HOME="$(@D)/.pgrx" \
		HOST_PG_CONFIG="/usr/lib/postgresql/18/bin/pg_config" \
		PGRX_PG_CONFIG_PATH="$(PGX_ULID_PG_CONFIG_WRAPPER)" \
		CARGO_HOME="$(DL_DIR)/br-cargo-home" \
		CARGO_TARGET_DIR="$(@D)/target" \
		CARGO_NET_OFFLINE=true \
		CARGO="$(HOST_DIR)/bin/cargo" \
		PATH="$(HOST_DIR)/bin:$(HOST_DIR)/sbin:$$PATH" \
		CC_$(subst -,_,$(RUSTC_TARGET_NAME))="$(TARGET_CC)" \
		AR_$(subst -,_,$(RUSTC_TARGET_NAME))="$(TARGET_AR)" \
		RANLIB_$(subst -,_,$(RUSTC_TARGET_NAME))="$(TARGET_RANLIB)" \
		CFLAGS_$(subst -,_,$(RUSTC_TARGET_NAME))="$(TARGET_CFLAGS)" \
		CARGO_TARGET_$(PGX_ULID_CARGO_TARGET_ENV)_LINKER="$(TARGET_CC)" \
		CARGO_TARGET_$(PGX_ULID_CARGO_TARGET_ENV)_AR="$(TARGET_AR)" \
		$(HOST_DIR)/bin/cargo-pgrx pgrx package \
			--no-default-features \
			--features "pg18" \
			--pg-config "$(PGX_ULID_PG_CONFIG_WRAPPER)" \
			--out-dir "$(PGX_ULID_PGRX_OUTDIR)" \
			--manifest-path "$(@D)/Cargo.toml" \
			--target "$(RUSTC_TARGET_NAME)"
endef

PGX_ULID_POST_BUILD_HOOKS += PGX_ULID_GENERATE_PGRX_PACKAGE

define PGX_ULID_INSTALL_TARGET_CMDS
	# Prefer the cargo-pgrx package output, which contains the generated SQL. :contentReference[oaicite:4]{index=4}
	PG_PKG="$(PGX_ULID_PGRX_OUTDIR)"; \
	if [ ! -d "$$PG_PKG" ]; then \
		echo "ERROR: pgrx out dir missing: $$PG_PKG"; \
		exit 1; \
	fi; \
	\
	SO="$$(find "$$PG_PKG" -type f -name 'pgx_ulid*.so' | head -n1)"; \
	CTRL="$$(find "$$PG_PKG" -type f -name 'pgx_ulid.control' | head -n1)"; \
	SQLS="$$(find "$$PG_PKG" -type f -name 'pgx_ulid--*.sql' | tr '\n' ' ')"; \
	\
	if [ -z "$$SO" ] || [ -z "$$CTRL" ] || [ -z "$$SQLS" ]; then \
		echo "ERROR: missing artifacts in $$PG_PKG"; \
		echo "  so=$$SO"; \
		echo "  control=$$CTRL"; \
		echo "  sql=$$SQLS"; \
		exit 1; \
	fi; \
	\
	echo "Installing $$SO to $(TARGET_DIR)/usr/lib/postgresql/$$(basename "$$SO")..."; \
	install -D -m 0755 "$$SO" "$(TARGET_DIR)/usr/lib/postgresql/$$(basename "$$SO")"; \
	mkdir -p "$(TARGET_DIR)/usr/share/postgresql/extension"; \
	echo "Installing $$CTRL to $(TARGET_DIR)/usr/share/postgresql/extension/pgx_ulid.control..."; \
	install -m 0644 "$$CTRL" "$(TARGET_DIR)/usr/share/postgresql/extension/pgx_ulid.control"; \
	for f in $$SQLS; do \
	echo "Installing $$f to $(TARGET_DIR)/usr/share/postgresql/extension/..."; \
		install -m 0644 "$$f" "$(TARGET_DIR)/usr/share/postgresql/extension/"; \
	done
endef

# Disable staging install from cargo-package
define PGX_ULID_INSTALL_STAGING_CMDS
	:
endef

$(eval $(cargo-package))
