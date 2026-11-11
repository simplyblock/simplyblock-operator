NEONVMD_VERSION = 0.3.0
NEONVMD_SOURCE = v$(NEONVMD_VERSION).tar.gz
NEONVMD_SITE = https://github.com/simplyblock/autoscaling/archive/refs/tags
NEONVMD_LICENSE = Apache2.0
NEONVMD_DEPENDENCIES = host-go

ifeq ($(BR2_arm),y)
GO_GOARCH = arm
ifeq ($(BR2_ARM_CPU_ARMV5),y)
GO_GOARM = 5
else ifeq ($(BR2_ARM_CPU_ARMV6),y)
GO_GOARM = 6
else ifeq ($(BR2_ARM_CPU_ARMV7A),y)
GO_GOARM = 7
else ifeq ($(BR2_ARM_CPU_ARMV8A),y)
# Go doesn't support 32-bit GOARM=8 (https://github.com/golang/go/issues/29373)
# but can still benefit from armv7 optimisations
GO_GOARM = 7
endif
else ifeq ($(BR2_aarch64),y)
GO_GOARCH = arm64
else ifeq ($(BR2_i386),y)
GO_GOARCH = 386
# i386: use softfloat if no SSE2: https://golang.org/doc/go1.16#386
ifneq ($(BR2_X86_CPU_HAS_SSE2),y)
GO_GO386 = softfloat
endif
else ifeq ($(BR2_x86_64),y)
GO_GOARCH = amd64
else ifeq ($(BR2_powerpc64),y)
GO_GOARCH = ppc64
else ifeq ($(BR2_powerpc64le),y)
GO_GOARCH = ppc64le
else ifeq ($(BR2_mips64),y)
GO_GOARCH = mips64
else ifeq ($(BR2_mips64el),y)
GO_GOARCH = mips64le
else ifeq ($(BR2_riscv),y)
GO_GOARCH = riscv64
else ifeq ($(BR2_s390x),y)
GO_GOARCH = s390x
endif

define NEONVMD_BUILD_CMDS
	cd $(@D) && \
	GOOS="linux" \
    GOARCH=$(GO_GOARCH) \
	go build -o neonvmd neonvm-daemon/cmd/*.go
endef

define NEONVMD_INSTALL_TARGET_CMDS
	mkdir -p $(TARGET_DIR)/neonvm/bin
	$(INSTALL) -D -m 0755 $(@D)/neonvmd $(TARGET_DIR)/neonvm/bin/neonvmd
endef

define NEONVMD_INSTALL_INIT_SYSTEMD
	$(INSTALL) -D -m 644 $(NEONVMD_PKGDIR)/neonvmd.service \
		$(TARGET_DIR)/usr/lib/systemd/system/neonvmd.service
endef

$(eval $(generic-package))
