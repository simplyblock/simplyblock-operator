PG_TIKTOKEN_VERSION = 0baf8d46620c9fa21acf4dc5f167e25f693aa932
PG_TIKTOKEN_SITE = $(call github,kelvich,pg_tiktoken,$(PG_TIKTOKEN_VERSION))
PG_TIKTOKEN_LICENSE = Apache2.0
PG_TIKTOKEN_LICENSE_FILES = LICENSE

# Use Buildroot cargo infrastructure (vendors dependencies for offline builds).
# You can override build/install commands while still getting vendoring. :contentReference[oaicite:3]{index=3}
PG_TIKTOKEN_DEPENDENCIES = postgresql
PG_TIKTOKEN_CARGO_LOCKED = buildroot
PG_TIKTOKEN_CARGO_LOCK_VERSION = 1

# If you packaged cargo-pgrx as a host tool, add it here:
#PG_TIKTOKEN_DEPENDENCIES += host-cargo-pgrx

# PostgreSQL pg_config from staging (Buildroot installs a pg_config helper)
PG_TIKTOKEN_PG_CONFIG = $(STAGING_DIR)/usr/bin/pg_config

# Derive major version from Buildroot’s postgresql version if available.
# If POSTGRESQL_VERSION is not defined in your tree, just hardcode PG_TIKTOKEN_PG_MAJOR = 18 (or 17/16).
PG_TIKTOKEN_PG_MAJOR = $(firstword $(subst ., ,$(POSTGRESQL_VERSION)))

# pgrx needs a writable home with config.toml
PG_TIKTOKEN_PGRX_HOME = $(@D)/.pgrx-home

# Ensure we only enable ONE pg feature (common pgrx build failure otherwise). :contentReference[oaicite:4]{index=4}
PG_TIKTOKEN_FEATURES = pg$(PG_TIKTOKEN_PG_MAJOR)

# Let Buildroot’s cargo infra handle cross flags; we just add pgrx env.
PG_TIKTOKEN_CARGO_ENV += \
	PGRX_HOME="$(PG_TIKTOKEN_PGRX_HOME)" \
	CARGO_NET_OFFLINE=true

define PG_TIKTOKEN_BUILD_CMDS
	# 1) Fix: "$PGRX_HOME does not exist"
	mkdir -p "$(PG_TIKTOKEN_PGRX_HOME)"

	# 2) Initialize pgrx to register the *staging* pg_config, but don’t run postgres
	# Cross-build guidance from pgrx: init with --no-run and a --pgXX flag. :contentReference[oaicite:5]{index=5}
	(cd $(@D) && \
		$(PG_TIKTOKEN_CARGO_ENV) \
		$(HOST_DIR)/bin/cargo pgrx init --no-run --pg$(PG_TIKTOKEN_PG_MAJOR) "$(PG_TIKTOKEN_PG_CONFIG)")

	# 3) Build the extension as a cdylib for the target
	(cd $(@D) && \
		$(TARGET_MAKE_ENV) \
		$(PG_TIKTOKEN_CARGO_ENV) \
		$(HOST_DIR)/bin/cargo build --release \
			--target "$(RUSTC_TARGET_NAME)" \
			--no-default-features --features "$(PG_TIKTOKEN_FEATURES)")
endef

define PG_TIKTOKEN_INSTALL_TARGET_CMDS
	# Compute target extension dirs from pg_config
	# (control/sql -> sharedir/extension, .so -> pkglibdir). :contentReference[oaicite:6]{index=6}
	PG_SHARE_EXT_DIR="$$("$(PG_TIKTOKEN_PG_CONFIG)" --sharedir)/extension"; \
	PG_PKGLIB_DIR="$$("$(PG_TIKTOKEN_PG_CONFIG)" --pkglibdir"; \
	$(INSTALL) -d "$(TARGET_DIR)$${PG_SHARE_EXT_DIR}" "$(TARGET_DIR)$${PG_PKGLIB_DIR}"; \
	\
	# Copy SQL + control from the repo
	# (pg_tiktoken includes pg_tiktoken.control and sql/ in-tree) :contentReference[oaicite:7]{index=7}
	$(INSTALL) -m 0644 $(@D)/pg_tiktoken.control "$(TARGET_DIR)$${PG_SHARE_EXT_DIR}/"; \
	if [ -d "$(@D)/sql" ]; then \
		$(INSTALL) -m 0644 $(@D)/sql/*.sql "$(TARGET_DIR)$${PG_SHARE_EXT_DIR}/" 2>/dev/null || true; \
	fi; \
	\
	# Copy the built library.
	# Rust outputs lib<name>.so; Postgres typically loads <extname>.so, so install/rename.
	if [ -f "$(@D)/target/$(RUSTC_TARGET_NAME)/release/libpg_tiktoken.so" ]; then \
		$(INSTALL) -m 0755 $(@D)/target/$(RUSTC_TARGET_NAME)/release/libpg_tiktoken.so \
			"$(TARGET_DIR)$${PG_PKGLIB_DIR}/pg_tiktoken.so"; \
	else \
		echo "ERROR: built library not found"; \
		exit 1; \
	fi
endef

$(eval $(cargo-package))