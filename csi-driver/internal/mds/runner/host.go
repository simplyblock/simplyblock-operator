// What the runner needs from its pod before a guest can start, how it starts
// QEMU, and how it asks the guest agent whether the guest is healthy: /dev/kvm,
// the QEMU process, and the probe the pod's readiness follows.

package runner

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

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

// GRPCHealthProbe returns a probe that passes while the guest agent at addr
// reports SERVING through gRPC's health service. The connection is plaintext:
// the agent is reachable only across the pod's private bridge.
//
// One client serves every probe. grpc.NewClient connects lazily and
// reconnects on its own, so a guest that restarts its agent is found again
// without a new client.
func GRPCHealthProbe(addr string) func(context.Context) error {
	var (
		once   sync.Once
		client healthpb.HealthClient
		err    error
	)
	return func(ctx context.Context) error {
		once.Do(func() {
			var conn *grpc.ClientConn
			conn, err = grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err == nil {
				client = healthpb.NewHealthClient(conn)
			}
		})
		if err != nil {
			return fmt.Errorf("guest agent client for %s: %w", addr, err)
		}
		resp, callErr := client.Check(ctx, &healthpb.HealthCheckRequest{})
		if callErr != nil {
			return fmt.Errorf("guest agent at %s: %w", addr, callErr)
		}
		if status := resp.GetStatus(); status != healthpb.HealthCheckResponse_SERVING {
			return fmt.Errorf("guest agent at %s reports %s", addr, status)
		}
		return nil
	}
}
