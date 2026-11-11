PGROUTING_VERSION = 4.0.0
PGROUTING_SOURCE = v$(PGROUTING_VERSION).tar.gz
PGROUTING_SITE = https://github.com/pgRouting/pgrouting/archive
PGROUTING_LICENSE = PostgreSQL
PGROUTING_LDFLAGS = $(TARGET_LDFLAGS) $(TARGET_NLS_LIBS)
PGROUTING_DEPENDENCIES = postgresql boost
PGROUTING_SUPPORTS_IN_SOURCE_BUILD = NO

define PGROUTING_PATH_FIXUP
	for f in $(TARGET_DIR)/usr/lib/postgresql/18/lib/libpgrouting*; do \
		mv "$$f" "$(TARGET_DIR)/usr/lib/postgresql/$$(basename "$$f")"; \
	done
	for f in $(TARGET_DIR)/usr/share/postgresql/18/extension/pgrouting*; do \
		mv "$$f" "$(TARGET_DIR)/usr/share/postgresql/extension/$$(basename "$$f")"; \
	done
	rm -rf $(TARGET_DIR)/usr/lib/postgresql/18
	rm -rf $(TARGET_DIR)/usr/share/postgresql/18
endef

PGROUTING_POST_INSTALL_TARGET_HOOKS += PGROUTING_PATH_FIXUP

$(eval $(cmake-package))