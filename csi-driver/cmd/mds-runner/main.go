//go:build linux

// The MDS runner: the one container of the pNFS metadata server pod, which
// starts and supervises the guest that serves the exports.
//
// It checks /dev/kvm, sizes the guest from the pod's limits, builds the
// private network between the pod and the guest, starts QEMU, and serves the
// pod's readiness from the guest's health. When the pod is stopped it shuts
// the guest down through its power button. Whatever ends the guest otherwise
// ends this process with an error, and kubelet restarts the pod
// (design-pnfs-mds-vm.md §5.2).

package main

import (
	"context"
	"errors"
	"flag"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"k8s.io/klog"

	"github.com/simplyblock/atlas/errs/deferrers"
	"github.com/simplyblock/csi-driver/internal/mds/netsetup"
	"github.com/simplyblock/csi-driver/internal/mds/qemu"
	"github.com/simplyblock/csi-driver/internal/mds/qmp"
	"github.com/simplyblock/csi-driver/internal/mds/runner"
)

type config struct {
	kernel, firmware       string
	rootDisk, stateDisk    string
	kvmDevice, runDir      string
	hostname, probeAddress string
	cpuLimitMilli          int64
	memoryLimitMiB         int64
	vhostNet               bool
	bootDeadline           time.Duration
	shutdownGrace          time.Duration
}

func parseFlags() config {
	var c config
	defaultFirmware := ""
	if runtime.GOARCH == string(qemu.ArchARM64) {
		defaultFirmware = "/usr/share/AAVMF/QEMU_EFI.fd"
	}

	flag.StringVar(&c.kernel, "kernel", "/mds/kernel/vmlinuz", "Guest kernel, booted directly")
	flag.StringVar(&c.firmware, "firmware", defaultFirmware, "UEFI firmware, arm64 only")
	flag.StringVar(&c.rootDisk, "root-disk", "/mds/disk.qcow2", "Guest root filesystem image, attached read-only")
	flag.StringVar(&c.stateDisk, "state-disk", "/dev/mds-state", "State PVC's block device, mounted at /var/lib/nfs")
	flag.StringVar(&c.kvmDevice, "kvm-device", "/dev/kvm", "KVM device")
	flag.StringVar(&c.runDir, "run-dir", "/run/mds", "Directory for the QMP socket")
	flag.StringVar(&c.hostname, "hostname", os.Getenv("POD_NAME"),
		"Guest hostname (downward API pod name), stable across restarts for NFSv4.1 state reclaim")
	flag.Int64Var(&c.cpuLimitMilli, "cpu-limit-millis", envInt("MDS_CPU_LIMIT_MILLI"),
		"Pod CPU limit in millicores (downward API limits.cpu, divisor 1m)")
	flag.Int64Var(&c.memoryLimitMiB, "memory-limit-mib", envInt("MDS_MEMORY_LIMIT_MIB"),
		"Pod memory limit in MiB (downward API limits.memory, divisor 1Mi)")
	flag.BoolVar(&c.vhostNet, "vhost-net", false, "Use vhost-net for the guest NIC; needs /dev/vhost-net")
	flag.StringVar(&c.probeAddress, "probe-address", ":8080", "Address serving /readyz and /healthz")
	flag.DurationVar(&c.bootDeadline, "boot-deadline", 120*time.Second,
		"Time the guest has to turn healthy before the pod restarts")
	flag.DurationVar(&c.shutdownGrace, "shutdown-grace", 20*time.Second,
		"Time a powered-down guest has to exit; keep below the pod's termination grace period")

	klog.InitFlags(nil)
	if err := flag.Set("logtostderr", "true"); err != nil {
		klog.Exitf("failed to set logtostderr flag: %v", err)
	}
	flag.Parse()
	return c
}

func envInt(name string) int64 {
	v, err := strconv.ParseInt(os.Getenv(name), 10, 64)
	if err != nil {
		return 0
	}
	return v
}

func main() {
	c := parseFlags()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	if err := run(ctx, c); err != nil {
		klog.Errorf("MDS runner: %v", err)
		klog.Flush()
		os.Exit(1)
	}
	klog.Info("MDS guest shut down")
}

func run(ctx context.Context, c config) error {
	if err := runner.CheckKVM(c.kvmDevice); err != nil {
		return err
	}
	resources, err := runner.GuestResources(c.cpuLimitMilli, c.memoryLimitMiB)
	if err != nil {
		return err
	}

	uplink, err := netsetup.DefaultUplink()
	if err != nil {
		return err
	}
	plan := netsetup.DefaultPlan(uplink)
	if err := netsetup.Apply(ctx, plan, netsetup.ExecIPTables); err != nil {
		return err
	}

	if err := os.MkdirAll(c.runDir, 0o700); err != nil {
		return err
	}
	qmpSocket := filepath.Join(c.runDir, "qmp.sock")
	// A previous QEMU in the same pod leaves its socket behind, and QEMU refuses
	// to bind over it.
	if err := os.Remove(qmpSocket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	guest := qemu.Guest{
		Arch:          qemu.Arch(runtime.GOARCH),
		KernelPath:    c.kernel,
		FirmwarePath:  c.firmware,
		RootDiskPath:  c.rootDisk,
		StateDiskPath: c.stateDisk,
		CPUs:          resources.CPUs,
		MemoryMiB:     resources.MemoryMiB,
		TapName:       plan.Tap,
		VhostNet:      c.vhostNet,
		MAC:           plan.GuestMAC,
		Address:       plan.GuestAddress(),
		Gateway:       plan.Gateway.Addr(),
		Hostname:      c.hostname,
		QMPSocket:     qmpSocket,
	}
	binary, err := guest.Binary()
	if err != nil {
		return err
	}
	args, err := guest.Args()
	if err != nil {
		return err
	}
	klog.Infof("starting %s guest %s: %d vCPUs, %d MiB, address %s via %s",
		guest.Arch, guest.Hostname, guest.CPUs, guest.MemoryMiB, guest.Address, uplink)

	sup := &runner.Supervisor{
		Start: runner.StartProcess(binary, args, os.Stdout, os.Stderr),
		// Until the guest agent answers its health call, nfsd accepting
		// connections is the closest signal that the guest can serve.
		Probe: runner.TCPProbe(net.JoinHostPort(plan.Guest.String(), strconv.Itoa(netsetup.NFSPort))),
		Powerdown: func(ctx context.Context) error {
			client, err := qmp.Dial(ctx, qmpSocket)
			if err != nil {
				return err
			}
			defer deferrers.Close(client)
			return client.SystemPowerdown(ctx)
		},
		BootDeadline:  c.bootDeadline,
		ProbeInterval: 2 * time.Second,
		ShutdownGrace: c.shutdownGrace,
	}

	probes := serveProbes(c.probeAddress, sup)
	defer deferrers.Close(probes)
	return sup.Run(ctx)
}

// serveProbes serves the pod's probes: /healthz while the runner is alive,
// /readyz while the guest is healthy.
func serveProbes(addr string, sup *runner.Supervisor) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if sup.Ready() {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	server := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			klog.Errorf("probe server: %v", err)
		}
	}()
	return server
}
