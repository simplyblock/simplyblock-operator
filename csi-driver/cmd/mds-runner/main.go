//go:build linux

// The MDS runner: the one container of the pNFS metadata server pod, which
// starts and supervises the guest that serves the exports.
//
// It checks /dev/kvm, sizes the guest from the pod's limits, builds the
// private network between the pod and the guest, starts QEMU, and serves the
// pod's readiness from the guest agent's health. Once the guest is healthy it
// dials the operator over csi-link and relays the export calls to the agent,
// resolving each namespace's connection with the pod's own credentials. When the pod is stopped it shuts
// the guest down through its power button. Whatever ends the guest otherwise
// ends this process with an error, and kubelet restarts the pod
// (design-pnfs-mds-vm.md §5.2).

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"k8s.io/klog"

	"github.com/simplyblock/atlas/errs/deferrers"
	"github.com/simplyblock/atlas/link"
	"github.com/simplyblock/atlas/nfsexport/nfsexportrpc"
	"github.com/simplyblock/csi-driver/internal/csilink"
	"github.com/simplyblock/csi-driver/internal/mds/netsetup"
	"github.com/simplyblock/csi-driver/internal/mds/qemu"
	"github.com/simplyblock/csi-driver/internal/mds/qmp"
	"github.com/simplyblock/csi-driver/internal/mds/relay"
	"github.com/simplyblock/csi-driver/internal/mds/runner"
	"github.com/simplyblock/csi-driver/internal/nfsexport"
)

type config struct {
	kernel, firmware       string
	rootDisk, stateDisk    string
	kvmDevice, runDir      string
	hostname, probeAddress string
	cpuLimitMilli          int64
	memoryLimitMiB         int64
	vhostNet               bool
	zeroedLayouts          bool
	debugSSH               bool
	bootDeadline           time.Duration
	shutdownGrace          time.Duration

	// The operator's link endpoint, under the node plugin's flag names, so
	// the driver passes both the same arguments.
	link           bool
	linkHubAddress string
	linkServerName string
	linkCAFile     string
	linkTokenFile  string
	podUID         string
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
	flag.BoolVar(&c.zeroedLayouts, "zeroed-layouts", true,
		"Zero and write the blocks of a pNFS write layout at allocation, so data a client wrote "+
			"survives a server restart that lost its LAYOUTCOMMIT; costs a WRITE ZEROES per allocation. "+
			"--zeroed-layouts=false turns it off")
	flag.BoolVar(&c.debugSSH, "debug-ssh", false,
		"Run an SSH server in the guest, reachable with `kubectl exec -it <pod> -- ssh` through a "+
			"key this runner generates at every boot")
	flag.StringVar(&c.probeAddress, "probe-address", ":8080", "Address serving /readyz and /healthz")
	flag.DurationVar(&c.bootDeadline, "boot-deadline", 120*time.Second,
		"Time the guest has to turn healthy before the pod restarts")
	flag.DurationVar(&c.shutdownGrace, "shutdown-grace", 20*time.Second,
		"Time a powered-down guest has to exit; keep below the pod's termination grace period")
	flag.BoolVar(&c.link, "link", false, "Dial the operator once the guest is healthy and relay its export calls")
	flag.StringVar(&c.linkHubAddress, "link-hub-address", "", "The operator's link endpoint, host:port")
	flag.StringVar(&c.linkServerName, "link-server-name", "",
		"Name to verify against the operator's link certificate, when it differs from the address dialed")
	flag.StringVar(&c.linkCAFile, "link-ca-file", "",
		"CA bundle signing the operator's link certificate; empty or absent dials plaintext")
	flag.StringVar(&c.linkTokenFile, "link-token-file", "/var/run/secrets/simplyblock.io/link/token",
		"Projected ServiceAccount token presented to the operator")
	flag.StringVar(&c.podUID, "pod-uid", os.Getenv("POD_UID"),
		"This pod's UID (downward API), so a restart supersedes the previous link session")

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
	if len(os.Args) > 1 && os.Args[1] == "extents" {
		os.Exit(runExtents(os.Args[2:]))
	}
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
		ZeroedLayouts: c.zeroedLayouts,
	}
	if c.debugSSH {
		keys, err := runner.PrepareDebugSSH(filepath.Join(c.runDir, "ssh"))
		if err != nil {
			return fmt.Errorf("preparing debug SSH: %w", err)
		}
		guest.SSHAuthorizedKeysPath = keys
		klog.Infof("debug SSH is on: kubectl exec -it %s -- ssh", c.hostname)
	}
	agentAddress := net.JoinHostPort(plan.Guest.String(), strconv.Itoa(netsetup.AgentPort))
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
		Probe: runner.GRPCHealthProbe(agentAddress),
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
	if c.link {
		go linkWhenReady(ctx, c, sup, agentAddress)
	}
	return sup.Run(ctx)
}

// linkWhenReady dials the operator once the guest first turns healthy, and
// from then on relays its export calls to the guest agent. Linking earlier
// would advertise an export service the guest cannot answer yet. The session
// outlives later unhealthy spells: the operator reads readiness from the pod,
// not from the link.
func linkWhenReady(ctx context.Context, c config, sup *runner.Supervisor, agentAddress string) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for !sup.Ready() {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}

	guest, err := grpc.NewClient(agentAddress, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		klog.Errorf("guest agent client for %s: %v", agentAddress, err)
		return
	}
	exports, err := nfsexportrpc.NewServer(relay.Relay{
		Guest:   nfsexportrpc.Remote(guest),
		Resolve: nfsexport.PublishedConnection,
	})
	if err != nil {
		klog.Errorf("export relay: %v", err)
		return
	}
	if _, err := csilink.Start(ctx, csilink.Config{
		HubAddress:   c.linkHubAddress,
		CAFile:       c.linkCAFile,
		ServerName:   c.linkServerName,
		TokenFile:    c.linkTokenFile,
		ID:           link.MDSPeer(c.hostname),
		InstanceUID:  c.podUID,
		Register:     exports.Register,
		Capabilities: nfsexportrpc.Capabilities(),
	}); err != nil {
		klog.Errorf("csi link: %v", err)
	}
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
