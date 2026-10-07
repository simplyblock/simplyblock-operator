// What the runner needs from its pod before a guest can start, and how it
// starts QEMU: /dev/kvm, the QEMU process, and the health probe the pod's
// readiness follows until the guest agent answers its own health call.

package runner

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"

	"github.com/simplyblock/atlas/errs/deferrers"
)

// CheckKVM fails unless the KVM device at path can be opened for reading and
// writing, which is what QEMU's -enable-kvm needs. The runner checks before
// starting QEMU so a node without KVM fails with that reason, instead of with
// whatever QEMU prints.
func CheckKVM(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("KVM is not usable (%w): the MDS needs a node with /dev/kvm, "+
			"nested virtualization on a cloud instance", err)
	}
	deferrers.Close(f)
	return nil
}

// StartProcess returns a Start that runs binary with args, its output going
// to stdout and stderr, which in the pod is the container log.
//
// The context only bounds the start. It is deliberately not tied to the
// process: canceling it is how the pod asks for a shutdown, and the guest is
// shut down through its power button, not by killing QEMU.
func StartProcess(binary string, args []string, stdout, stderr io.Writer) func(context.Context) (Process, error) {
	return func(context.Context) (Process, error) {
		cmd := exec.Command(binary, args...)
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		return execProcess{cmd}, nil
	}
}

type execProcess struct{ cmd *exec.Cmd }

func (p execProcess) Wait() error { return p.cmd.Wait() }
func (p execProcess) Kill() error { return p.cmd.Process.Kill() }

// TCPProbe returns a probe that passes while addr accepts TCP connections.
func TCPProbe(addr string) func(context.Context) error {
	return func(ctx context.Context) error {
		var d net.Dialer
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return err
		}
		deferrers.Close(conn)
		return nil
	}
}
