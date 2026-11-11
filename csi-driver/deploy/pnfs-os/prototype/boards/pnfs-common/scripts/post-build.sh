#!/bin/sh
BOARD_DIR="$( dirname "${0}" )"

# Write version information
cat > ${TARGET_DIR}/etc/velaos << EOF
name=Vela OS (pNFS MDS)
version=${VELAOS_VERSION}
build=${VELAOS_BUILD}
EOF

makeSymlink() {
  source="$1"
  dest="$2"

  if [ -h "${dest}" ] || [ -e "${dest}" ]; then
    rm -rf "${dest}"
  fi
  ln -s "${source}" "${dest}"
}

UNITDIR="${TARGET_DIR}/usr/lib/systemd/system"
mkdir -p "${TARGET_DIR}/etc/systemd/system" "${TARGET_DIR}/etc/systemd/network" \
         "${UNITDIR}/multi-user.target.wants"

# NFSv4.1+ only: rpcbind is pulled in by nfs-utils but must not run. statd and
# blkmapd serve NFSv3 locking and the client side respectively.
echo "Configuring NFS server..."
for unit in rpcbind.service rpcbind.socket nfs-blkmap.service rpc-statd.service \
            rpc-statd-notify.service; do
  makeSymlink "/dev/null" "${TARGET_DIR}/etc/systemd/system/${unit}"
done
# nfsd is not started at boot: a guest boots as a standby, and mds-agent starts
# nfs-server.service and nfsdcld.service when the operator promotes it, after it
# has fenced every device and mounted the state volume.
rm -f "${UNITDIR}/multi-user.target.wants/nfs-server.service"
makeSymlink "../mds-agent.service" "${UNITDIR}/multi-user.target.wants/mds-agent.service"

# The root filesystem is read-only, the assembler writes its exports here.
makeSymlink "/run/exports.d" "${TARGET_DIR}/etc/exports.d"

# The client recovery database lives on a simplyblock volume that mds-agent
# connects and mounts at /var/lib/nfs when the guest is promoted. Nothing in the
# image mounts it, and an earlier image's fstab entry for a state disk is removed
# so a guest without one still reaches local-fs.target.
echo "Configuring state directory..."
sed -i '\#[[:space:]]/var/lib/nfs[[:space:]]#d' "${TARGET_DIR}/etc/fstab"

# The address comes from the kernel command line (ip=...:eth0:off) and is
# configured before udev runs, keep kernel interface names so it stays on eth0.
makeSymlink "/dev/null" "${TARGET_DIR}/etc/systemd/network/99-default.link"

# Debug SSH (dropbear) runs only with simplyblock.debug_ssh=1 on the kernel
# command line, through debug-ssh.service. Buildroot's own dropbear units would
# start it on every boot, so they are masked. Root's authorized_keys is written
# to /run at boot from QEMU fw_cfg (debug-ssh-key.service).
echo "Configuring debug SSH..."
for unit in dropbear.service dropbear.socket; do
  makeSymlink "/dev/null" "${TARGET_DIR}/etc/systemd/system/${unit}"
done
makeSymlink "../debug-ssh.service" "${UNITDIR}/multi-user.target.wants/debug-ssh.service"
mkdir -p "${TARGET_DIR}/root"
makeSymlink "/run/root-ssh" "${TARGET_DIR}/root/.ssh"
