#!/bin/sh
BOARD_DIR="$( dirname "${0}" )"

# Write version information
cat > ${TARGET_DIR}/etc/velaos << EOF
name=Vela OS
version=${VELAOS_VERSION}
build=${VELAOS_BUILD}
EOF

echo "Setting up neonvm directory with symlinks..."
makeSymlink() {
  source="$1"
  dest="$2"

  if [ -h "${dest}" ]; then
    rm "${dest}"
  fi
  ln -s "${source}" "${dest}"
}

linkIfBusybox() {
  dest="$1"
  target_base="$2"
  link="$(readlink "${dest}")"
  if ! [ "${link}" ]; then
    return
  fi

  cmd="$(basename "${dest}")"
  rebased_cmd="${target_base}/${cmd}"
  makeSymlink "${rebased_cmd}" "${TARGET_DIR}/neonvm/bin/${cmd}"
}

fixupBusyboxSymlinks() {
  dir="$1"
  target_base="$2"
  for filename in ${dir}/*; do
    linkIfBusybox "${filename}" "${target_base}"
  done
}

mkdir -p "${TARGET_DIR}/neonvm/bin"

fixupBusyboxSymlinks "${TARGET_DIR}/bin" "/bin"
fixupBusyboxSymlinks "${TARGET_DIR}/sbin" "/sbin"
fixupBusyboxSymlinks "${TARGET_DIR}/usr/bin" "/usr/bin"
fixupBusyboxSymlinks "${TARGET_DIR}/usr/sbin" "/usr/sbin"

chmod +x ${TARGET_DIR}/neonvm/bin/su-exec

makeSymlink "/usr/bin/ssh-keygen" "${TARGET_DIR}/neonvm/bin/ssh-keygen"
makeSymlink "/sbin/udevd" "${TARGET_DIR}/neonvm/bin/udevd"
makeSymlink "/usr/sbin/chronyd" "${TARGET_DIR}/neonvm/bin/chronyd"
makeSymlink "/usr/sbin/sshd" "${TARGET_DIR}/neonvm/bin/sshd"
makeSymlink "/usr/sbin/acpid" "${TARGET_DIR}/neonvm/bin/acpid"
makeSymlink "/usr/bin/flock" "${TARGET_DIR}/neonvm/bin/flock"
makeSymlink "/sbin/blkid" "${TARGET_DIR}/neonvm/bin/blkid"
makeSymlink "/usr/bin/findmnt" "${TARGET_DIR}/neonvm/bin/findmnt"
makeSymlink "/sbin/vector" "${TARGET_DIR}/neonvm/bin/vector"
makeSymlink "/sbin/resize2fs" "${TARGET_DIR}/neonvm/bin/resize2fs"
makeSymlink "/usr/sbin/xfs_growfs" "${TARGET_DIR}/neonvm/bin/xfs_growfs"

echo "Ensuring PostgreSQL extensions are trusted..."
for filename in ${TARGET_DIR}/usr/share/postgresql/extension/*.control;
do
  grep -qF 'trusted' $filename || echo "trusted = true" >> $filename
done

rm -rf ${TARGET_DIR}/etc/init.d/S50postgresql

rm -rf "${TARGET_DIR}/var/log"
mkdir -p "${TARGET_DIR}/var/log/chrony"
touch "${TARGET_DIR}/var/log/chrony/chrony.log"
mkdir -p "${TARGET_DIR}/etc/postgresql.conf.d"
