CARGO_PGRX_VERSION = 0.16.1
CARGO_PGRX_SITE = $(call github,pgcentralfoundation,pgrx,v$(CARGO_PGRX_VERSION))
CARGO_PGRX_LICENSE = MIT OR Apache-2.0
CARGO_PGRX_LICENSE_FILES = LICENSE-MIT LICENSE-APACHE

# The cargo subcommand lives in this subdir of the pgrx repo
CARGO_PGRX_SUBDIR = cargo-pgrx

HOST_CARGO_PGRX_DEPENDENCIES = host-rustc

# Build the cargo subcommand binary
HOST_CARGO_PGRX_CARGO_BUILD_OPTS =

$(eval $(host-cargo-package))
