//go:build linux

// The pNFS metadata server's guest agent: it runs inside the guest and
// assembles the exports the runner relays to it, with the same assembler the
// node plugin hosts on a node (design-pnfs-mds-vm.md §6.3).
//
// It listens on the guest's bridge address, which only the runner reaches,
// and serves the export service, the metadata server's role service, and gRPC's
// health service there. A guest boots as a standby that mounts and connects
// nothing and serves health at once. Promote (promote.go) makes it the active
// one, which then also needs the state volume mounted and nfsd running. The
// export service refuses CreateExport until then.
// It also watches nfsd and logs its state to the console (nfsd_watchdog.go),
// and keeps every connection on all its paths (paths.go).
// It holds no credentials. Every volume's connection arrives with the call,
// resolved by the runner.

package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	oexec "os/exec"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"k8s.io/klog"
	mountutils "k8s.io/mount-utils"

	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/atlas/lvol"
	atlasexport "github.com/simplyblock/atlas/nfsexport"
	"github.com/simplyblock/atlas/nfsexport/nfsexportrpc"
	"github.com/simplyblock/atlas/nvme"
	"github.com/simplyblock/atlas/nvmeof"
	"github.com/simplyblock/atlas/nvmepr"
	"github.com/simplyblock/atlas/storage"
	"github.com/simplyblock/csi-driver/internal/mds/agent"
	"github.com/simplyblock/csi-driver/internal/mds/netsetup"
	csimount "github.com/simplyblock/csi-driver/internal/mount"
	"github.com/simplyblock/csi-driver/internal/nfsexport"
)

// healthInterval is how often the guest's health is re-evaluated. The pod's
// readiness probe runs every five seconds, so this keeps the answer fresh for
// each probe.
const healthInterval = 2 * time.Second

func main() {
	listen := flag.String("listen", ":"+strconv.Itoa(netsetup.AgentPort), "Address serving the export and health services")
	stateDir := flag.String("state-dir", nfsexport.NFSStateDir, "Where the state disk is mounted")
	watchdog := flag.Duration("nfsd-watchdog-interval", agent.DefaultNFSDWatchdogInterval,
		"How often nfsd's threads and pool counters are read and judged; 0 turns the watchdog off")
	pathInterval := flag.Duration("path-interval", agent.DefaultPathInterval,
		"How often every export's namespace is reconnected over the paths it is missing")
	klog.InitFlags(nil)
	if err := flag.Set("logtostderr", "true"); err != nil {
		klog.Exitf("failed to set logtostderr flag: %v", err)
	}
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := run(ctx, *listen, *stateDir, *watchdog, *pathInterval); err != nil {
		klog.Errorf("mds-agent: %v", err)
		klog.Flush()
		os.Exit(1)
	}
}

// reconcilePaths brings one namespace onto every path its connection
// publishes, with the connector and identity the assembler attached it with.
func reconcilePaths(ctx context.Context, conn lvol.Connection, hostNQN string) (nvmeof.PathState, error) {
	var opts []nvmeof.TargetOption
	if hostNQN != "" {
		opts = append(opts, nvmeof.WithHostNQN(hostNQN))
	}
	connector := nvmeof.NewCLIConnector(nvme.NewSysfsSubsystemResolver(nvme.SysfsConfig{}))
	return nvmeof.ReconcilePaths(ctx, connector, nil, conn, opts...)
}

// command runs a command and returns its combined output.
func command(ctx context.Context, name string, args ...string) ([]byte, error) {
	return oexec.CommandContext(ctx, name, args...).CombinedOutput()
}

// guestConfig wires the role machine to the guest's real connector, reservation
// device, mounter, nfsd key parameter, and systemd.
func guestConfig(keeper *agent.PathKeeper, stateDir string) agent.GuestConfig {
	subs := nvme.NewSysfsSubsystemResolver(nvme.SysfsConfig{})
	return agent.GuestConfig{
		Connector: agent.FabricConnector{
			Connector: nvmeof.NewCLIConnector(subs),
			Devices:   storage.Local(nvme.SysfsConfig{}).DeviceResolver,
		},
		Open:   nvmepr.Open,
		HostID: agent.HostIDFromNQN,
		State: agent.FSMounter{
			Prober: blockdev.NewProber(),
			FS:     csimount.New().FilesystemOps(),
		},
		Key: agent.ParamKeyWriter{
			Path: agent.NFSDKeyParameter,
			LoadModule: func(ctx context.Context) error {
				out, err := command(ctx, "modprobe", "nfsd")
				if err != nil {
					return fmt.Errorf("modprobe nfsd: %w (%s)", err, out)
				}
				return nil
			},
		},
		Units:    agent.Systemctl{Run: command},
		Tracker:  keeper,
		StateDir: stateDir,
		Log:      agent.KlogLogger{},
	}
}

func run(ctx context.Context, listen, stateDir string, watchdog, pathInterval time.Duration) error {
	// No host NQN function: the identity arrives with every call, decided by
	// the operator and resolved by the runner.
	assembler, err := nfsexport.NewAssembler(
		storage.Local(nvme.SysfsConfig{}).DeviceResolver, csimount.New(), nil)
	if err != nil {
		return err
	}
	keeper := &agent.PathKeeper{Reconcile: reconcilePaths, Log: agent.KlogLogger{}}
	mounter := mountutils.New("")
	guest := agent.NewGuest(guestConfig(keeper, stateDir))
	exports, err := nfsexportrpc.NewServer(guest.Guard(keeper.Wrap(nfsexport.WithNFSD(assembler))),
		nfsexportrpc.WithExtentReader(&atlasexport.ExtentReader{Root: atlasexport.ExportsRoot}))
	if err != nil {
		return err
	}
	roles, err := nfsexportrpc.NewMDSServer(guest)
	if err != nil {
		return err
	}

	server := grpc.NewServer()
	exports.Register(server)
	roles.Register(server)
	healthServer := health.NewServer()
	healthpb.RegisterHealthServer(server, healthServer)

	go agent.Report(ctx, healthServer, agent.Health{
		Role:     guest.Role,
		StateDir: stateDir,
		Mounted:  mounter.IsMountPoint,
		NFSD:     nfsexport.CheckNFSDThreads,
	}.Check, healthInterval)
	go agent.RunNFSDWatchdog(ctx, &agent.NFSDWatchdog{Log: agent.KlogLogger{}}, watchdog)
	go keeper.Run(ctx, pathInterval)

	lis, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		server.GracefulStop()
	}()
	klog.Infof("mds-agent serving on %s", listen)
	return server.Serve(lis)
}
