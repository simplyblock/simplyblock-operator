// Detaching a mount that cannot be unmounted: one of an NFS server that is
// gone, which an ordinary umount waits on for as long as the server stays
// away. Linux only, as the flags are.

package mount

import "golang.org/x/sys/unix"

// Detach forces off what is outstanding on the mount at path and removes it
// from the namespace at once. For NFS, MNT_FORCE kills the RPCs waiting on the
// server, so nothing keeps the mount, and its client, alive behind the lazy
// detach.
func (m *Mounter) Detach(path string) error {
	return unix.Unmount(path, unix.MNT_FORCE|unix.MNT_DETACH)
}
