//go:build linux

// What the kernel says this machine is, from uname.
//
// It is one system call and it has no file behind it, which is why it is the
// one reading in this package that needs a build tag: there is no tree a
// developer's machine could be pointed at to produce the same answer.

package inventory

import "golang.org/x/sys/unix"

// LocalMachine is uname's machine field: `x86_64`, `aarch64`.
//
// It is the kernel's own answer, so a process reading it inside a container
// gets the host's architecture rather than its image's, which makes it the one
// host reading a pod does not have to be given a mount for.
func LocalMachine() (string, error) {
	var uts unix.Utsname
	if err := unix.Uname(&uts); err != nil {
		return "", err
	}
	return unix.ByteSliceToString(uts.Machine[:]), nil
}
