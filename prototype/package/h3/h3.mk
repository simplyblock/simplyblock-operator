H3_VERSION = 4.4.1
H3_SOURCE = v$(H3_VERSION).tar.gz
H3_SITE = https://github.com/uber/h3/archive/refs/tags
H3_LICENSE = PostgreSQL
H3_LDFLAGS = $(TARGET_LDFLAGS) $(TARGET_NLS_LIBS)
H3_DEPENDENCIES = postgresql boost postgis
H3_SUPPORTS_IN_SOURCE_BUILD = NO

$(eval $(cmake-package))