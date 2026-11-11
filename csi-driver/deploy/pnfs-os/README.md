# Vela OS

<p align="center">
    <a href="https://vela.simplyblock.io">
<img src="https://raw.githubusercontent.com/simplyblock/vela/main/img/vela-logo-white.png" alt="Vela Logo" width="250" height="100">
        </a>
</p>

[Vela](https://vela.simplyblock.io) is a self-hostable, serverless Postgres development platform. Built on a high-performance distributed storage layer, it provides instant database branching, independent scaling of compute and storage, and enterprise-grade access control — all deployable in your own cloud. Enterprise-grade features and toolkit developers love in one product.

**Vela OS** is the operating system which runs the Vela branch services inside the Neon Autoscaling VM. 

The Vela main repository is [here](https://github.com/simplyblock/vela).

---

## Vela OS Build System

Vela OS is built using [Buildroot](https://buildroot.org/) and a set of additional packages, which mostly integrate
PostgreSQL extensions and tools.

This repository contains the required Buildroot configuration files and the scripts which help build the Vela OS
image in a Docker or podman environment. This build system itself if fully containerized and self-contained for easy
execution.

The following section describes the main steps and commands to build the Vela OS image. Additional commands are
available through the builder script.

A full set of commands can be retrieved by running the builder script:

```bash
$ ./builder
```

---

## How to build Vela OS

The Vela OS build system contains of a comprehensive script which unifies and simplifies the build process.

### Prerequisites

Clone the Vela OS repository and change into the directory. These steps, typically, only have to performed once or when
the buildroot submodule is updated.

```bash
$ git clone https://github.com/simplyblock/vela-os.git
$ cd vela-os
```

The Buildroot submodule needs to be initialized, downloaded, and patched. The build system has a command to do this in
one step:

```bash
$ ./builder initialize
```

### Building the Builder Container Image

Since the build system is fully containerized, the container image of the builder needs to be built. The image is
versioned (through the checksum of the Dockerfile) and can be cached for faster builds. If the image needs to be
updated, the build system will notify and ask to rebuild the image.

```bash
$ ./builder docker setup
```

### Building the Vela OS Image

To build the Vela OS image, run the following command:

```bash
$ ./builder build
```

The build system will build all required host tools, as well as all required target packages. The build is extensive
and takes a while, especially for the first run. To speed up further builds, the build system caches built object files
through CCache and downloaded sources. The caches are located in $HOME/ccache and $HOME/dlcache.

### Adjust the Build Configuration

The build system uses configuration files to adjust what needs to be built. The set of configuration files contains the
Buildroot, the Linux kernel, and the Busybox configuration.

To adjust the Buildroot configuration run the following command:

```bash
$ ./builder config
```

To adjust the Linux kernel configuration run the following command:

```bash
$ ./builder config Linux
```

To adjust the Busybox configuration run the following command:

```bash
$ ./builder config busybox
```

After adjusting one or more of the configurations, the following command needs to be run to store the changes back into
the prototype directory:

```bash
$ ./builder saveconfig
```

### Vela Cloud (Recommended)

The easiest way to get started is through **Vela Cloud**.  
Free tier available — no credit card required.

[**Get Started**](https://vela.simplyblock.io)

## Community

Find help, explore resources, or get involved with the Vela community.

### Support & Contributions

- [Open an Issue](https://github.com/simplyblock/vela/issues) – Report bugs or suggest improvements
- [Start a Discussion](https://github.com/simplyblock/vela/discussions) – Share feedback and feature ideas, ask questions, share ideas, and connect with other users
- [Contribute Code](https://github.com/simplyblock/vela/pulls) – Submit pull requests following our contribution guidelines

We welcome contributions of all kinds — from documentation improvements and bug fixes to new features and integrations.

