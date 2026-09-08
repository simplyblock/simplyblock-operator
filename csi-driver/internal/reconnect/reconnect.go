// Package reconnect keeps this node's attached NVMe-oF paths matching what the
// control plane says they should be.
//
// It is the driver's repair loop rather than its attach path: it polls the
// local fabric, decides which subsystems are short of the redundancy they were
// published with, and reconnects or repairs what is missing. A volume whose
// device disappears entirely is reported upward instead, because no reconnect
// can fix a namespace the kernel has already removed.
package reconnect

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog"

	"github.com/simplyblock/csi-driver/internal/clusters"
	"github.com/simplyblock/csi-driver/internal/controlplane"
	"github.com/simplyblock/csi-driver/internal/fabric"
	"github.com/simplyblock/csi-driver/internal/initiator"
	sbkube "github.com/simplyblock/csi-driver/internal/kubernetes"
	"github.com/simplyblock/csi-driver/internal/nqn"
)

var (
	// maxSeenPathsMap caches the highest number of active NVMe-oF paths ever
	// observed per NQN, so degradation is detected without querying the
	// control plane on every cycle.
	maxSeenPathsMap = make(map[string]int)
	maxSeenMu       sync.Mutex

	// nodeHostNQNMu guards nodeHostNQNVal, this process's cached result of
	// NodeHostNQN.
	nodeHostNQNMu  sync.Mutex
	nodeHostNQNVal string
)

// NodeHostNQN returns this Kubernetes node's simplyblock-format host NQN
// (nqn.2014-08.io.simplyblock:uuid:<node.UID>) — the identity DHCHAP/
// allowed_hosts pools authorize, and that the CSI driver must present on
// every connect to that node's volumes (see NodeStageVolume, which computes
// the same formula). It is a per-NODE constant, not a per-volume one: every
// lvol staged on this node shares the exact same value, since it depends
// only on this node's own UID. That makes it safe to cache indefinitely for
// the process's lifetime rather than tracking it per-lvolID — a per-lvolID
// cache would need eviction and, worse, would be silently wiped by any
// process restart (a csi-node pod restart, node reboot, OOM) for lvols that
// stay connected at the kernel level across it, reintroducing the very
// "reconnect drops the host identity" bug this exists to fix, just
// triggered by a different event. Recomputing this per-node value fresh on
// every process start has no such failure mode. A failed lookup is not
// cached, so the next call retries rather than getting stuck returning "".
func NodeHostNQN(ctx context.Context, client kubernetes.Interface, nodeName string) string {
	nodeHostNQNMu.Lock()
	defer nodeHostNQNMu.Unlock()
	if nodeHostNQNVal != "" {
		return nodeHostNQNVal
	}
	node, err := client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		klog.Warningf("failed to resolve node %s for hostNQN: %v", nodeName, err)
		return ""
	}
	nodeHostNQNVal = fmt.Sprintf("nqn.2014-08.io.simplyblock:uuid:%s", node.UID)
	return nodeHostNQNVal
}

// isManagedLvol reports whether lvolID is backed by a PersistentVolume
// provisioned by the given CSI driver. Only such lvols are reconnected;
// benchmark and foreign (non-simplyblock, or other-driver) volumes are skipped.
func isManagedLvol(manager *sbkube.Manager, lvolID, driver string) bool {
	pv, err := manager.PersistentVolumeByLogicalVolumeID(context.Background(), lvolID)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			klog.Errorf("reconnect: failed to read PersistentVolume for lvolID %s: %v", lvolID, err)
		}
		return false
	}
	return pv.Spec.CSI != nil && pv.Spec.CSI.Driver == driver
}

func reconnectSubsystems(markBroken func(lvolID string), manager *sbkube.Manager, driver, nodeName string) error {
	ctx := context.Background()

	// Resolved once per tick rather than once per degraded subsystem: it's
	// the same value for every lvol on this node (see NodeHostNQN), and this
	// keeps the guardian's poll loop from hitting the K8s API more than once
	// a cycle even when several subsystems need recovery at once.
	hostNQN := NodeHostNQN(ctx, manager.Client(), nodeName)

	devices, err := initiator.NVMeDevices(ctx)
	if err != nil {
		return fmt.Errorf("failed to get NVMe device paths: %v", err)
	}

	currentDevices := make(map[string]bool)

	for _, device := range devices {
		subsystems, err := initiator.SubsystemsForDevice(ctx, device.DevicePath)
		if err != nil {
			klog.Errorf("failed to get subsystems for device %s: %v", device.DevicePath, err)
			continue
		}

		currentDevices[device.DevicePath] = true

		for _, host := range subsystems {
			for _, subsystem := range host.Subsystems {
				clusterID, nqnLvolID := nqn.LvolIDFromNQN(subsystem.NQN)
				if nqnLvolID == "" {
					continue
				}
				// Prefer the sysfs UUID when available — it always identifies the
				// exact namespace LVol. Falls back to the NQN-derived ID.
				lvolID := device.LvolID
				if lvolID == "" {
					lvolID = nqnLvolID
				}

				// Only act on lvols backed by a PV from our CSI driver; skip
				// benchmark and foreign volumes.
				if !isManagedLvol(manager, lvolID, driver) {
					continue
				}

				// Only mark the device present once we have a confirmed lvolID,
				// so the cleanup loop never sees a device without a mapping.
				// TODO: replace devicePresentMap/deviceToLvolIDMap with a live
				// sysfs scan via atlas nvme.SysfsDeviceResolver once the atlas
				// connector is sufficiently tested — these maps duplicate what
				// atlas already reads from /sys.
				initiator.MarkDevicePresent(device.DevicePath, lvolID)

				numActive := len(subsystem.Paths)
				if numActive == 0 {
					continue
				}

				expected := resolveExpectedPathCount(subsystem.NQN, clusterID, lvolID, numActive, hostNQN)

				needsRecovery := numActive < expected ||
					(expected > 1 && hasConnectingPath(subsystem.Paths))

				if !needsRecovery {
					continue
				}

				if !confirmSubsystemNeedsRecovery(ctx, &subsystem, device.DevicePath, numActive) {
					continue
				}

				klog.Infof("Degraded subsystem: NQN=%s active=%d expected=%d device=%s",
					subsystem.NQN, numActive, expected, device.DevicePath)

				if err := recoverPathsWithANA(clusterID, lvolID, device.DevicePath, subsystem.Paths, hostNQN); err != nil {
					klog.Errorf("failed to recover paths for lvolID %s: %v", lvolID, err)
				}
			}
		}
	}

	var goneLvols []string
	for _, missing := range initiator.PruneMissingDevices(currentDevices) {
		klog.Errorf(
			"Device %s is no longer present — all NVMe-oF connections were lost and the kernel removed the device (lvolID=%s)",
			missing.DevicePath,
			missing.LvolID,
		)
		goneLvols = append(goneLvols, missing.LvolID)
	}

	if markBroken != nil {
		for _, lvolID := range goneLvols {
			markBroken(lvolID)
		}
	}

	return nil
}

func isAnyConnReachable(ctx context.Context, conns []*controlplane.LvolConnectResp) bool {
	for _, conn := range conns {
		if isTCPReachable(ctx, conn.IP, conn.Port) {
			return true
		}
	}
	return false
}

func isTCPReachable(ctx context.Context, ip string, port int) bool {
	d := net.Dialer{Timeout: 1 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", fmt.Sprintf("%s:%d", ip, port))
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func isNodeOnline(ctx context.Context, client *controlplane.ClusterClient, nodeID, ip string, port int) bool {
	status, err := client.StorageNodeStatus(ctx, nodeID)
	if err != nil {
		klog.Errorf("failed to fetch node status for node %s: %v", nodeID, err)
		return false
	}
	if status != "online" {
		return false
	}
	if ip != "" && port != 0 {
		if !isTCPReachable(ctx, ip, port) {
			klog.Infof("isNodeOnline: node %s API online but %s:%d not TCP-reachable", nodeID, ip, port)
			return false
		}
	}
	return true
}

// confirmSubsystemNeedsRecovery re-checks the subsystem 5 times over 5 seconds
// and returns true only if the path count remained stable at initialPathCount for
// all 5 checks. This debounces spurious triggers during normal ANA switchovers.
func confirmSubsystemNeedsRecovery(
	ctx context.Context,
	subsystem *initiator.Subsystem,
	devicePath string,
	initialPathCount int,
) bool {
	for i := 0; i < 5; i++ {
		recheck, err := initiator.SubsystemsForDevice(ctx, devicePath)
		if err != nil {
			klog.Errorf("failed to recheck subsystems for device %s: %v", devicePath, err)
			continue
		}

		found := false
		for _, h := range recheck {
			for _, s := range h.Subsystems {
				if s.NQN == subsystem.NQN {
					found = true
					if len(s.Paths) != initialPathCount {
						return false
					}
				}
			}
		}

		if !found {
			klog.Warningf("Subsystem %s not found during recheck, assuming it's gone", subsystem.NQN)
			return false
		}

		time.Sleep(1 * time.Second)
	}
	return true
}

// MonitorConnection monitors NVMe-oF connections and reconnects missing or
// IP-changed paths. Supports 1-path, 2-path, and 3-path volumes
// (1 optimized + up to 2 non-optimized).
const (
	monitorBaseInterval    = 3 * time.Second
	monitorJitter          = 500 * time.Millisecond
	monitorMaxBackoff      = 60 * time.Second
	monitorCircuitAfter    = 5
	monitorCircuitCooldown = 30 * time.Second
)

func MonitorConnection(markBroken func(lvolID string), manager *sbkube.Manager, driver, nodeName string) {
	var (
		consecutiveErrors int
		backoff           = monitorBaseInterval
	)

	for {
		err := reconnectSubsystems(markBroken, manager, driver, nodeName)
		if err != nil {
			consecutiveErrors++
			klog.Errorf("MonitorConnection error (%d consecutive): %v", consecutiveErrors, err)

			if consecutiveErrors >= monitorCircuitAfter {
				klog.Warningf(
					"MonitorConnection: circuit open after %d failures, cooling down for %s",
					consecutiveErrors,
					monitorCircuitCooldown,
				)
				time.Sleep(monitorCircuitCooldown)
				continue
			}

			// exponential backoff capped at monitorMaxBackoff
			backoff *= 2
			if backoff > monitorMaxBackoff {
				backoff = monitorMaxBackoff
			}
		} else {
			consecutiveErrors = 0
			backoff = monitorBaseInterval
		}

		jitter := time.Duration(rand.Int63n(int64(monitorJitter)))
		time.Sleep(backoff + jitter)
	}
}

// hasConnectingPath reports whether any path has State == "connecting".
// On a multi-path volume this typically means a node's IP changed and the kernel
// is still trying to reach the old address.
func hasConnectingPath(paths []initiator.Path) bool {
	for _, p := range paths {
		if p.State == "connecting" {
			return true
		}
	}
	return false
}

// resolveExpectedPathCount returns the expected number of NVMe-oF paths for the
// given NQN. On first encounter it queries the API once to seed the cache so the
// monitor works correctly even if started while a volume is already degraded.
// Subsequent calls use the in-memory cache, which only grows upward.
func resolveExpectedPathCount(subsystemNQN, clusterID, lvolID string, currentActive int, hostNQN string) int {
	maxSeenMu.Lock()
	cached, exists := maxSeenPathsMap[subsystemNQN]
	if currentActive > cached {
		cached = currentActive
		maxSeenPathsMap[subsystemNQN] = cached
	}
	maxSeenMu.Unlock()

	if exists {
		return cached
	}

	sbcClient, err := clusters.Client(context.Background(), clusterID, "")
	if err != nil {
		klog.Warningf("resolveExpectedPathCount: client error for NQN %s: %v", subsystemNQN, err)
		return cached
	}
	conns, err := sbcClient.LvolConnections(context.Background(), lvolID, hostNQN)
	if err != nil {
		klog.Warningf("resolveExpectedPathCount: fetch error for NQN %s: %v", subsystemNQN, err)
		return cached
	}

	maxSeenMu.Lock()
	if len(conns) > maxSeenPathsMap[subsystemNQN] {
		maxSeenPathsMap[subsystemNQN] = len(conns)
		cached = len(conns)
	}
	maxSeenMu.Unlock()

	return cached
}

func recoverPathsWithANA(clusterID, lvolID, devicePath string, activePaths []initiator.Path, hostNQN string) error {
	sbcClient, err := clusters.Client(context.Background(), clusterID, "")
	if err != nil {
		return fmt.Errorf("failed to create SimplyBlock client: %w", err)
	}

	nodeInfo, err := sbcClient.VolumeNodeInfo(context.Background(), lvolID)
	if err != nil {
		return fmt.Errorf("failed to fetch node info for lvol %s: %w", lvolID, err)
	}

	expectedConns, err := sbcClient.LvolConnections(context.Background(), lvolID, hostNQN)
	if err != nil {
		return fmt.Errorf("failed to fetch connections for lvol %s: %w", lvolID, err)
	}
	if len(expectedConns) == 0 {
		return fmt.Errorf("API returned no connections for lvol %s", lvolID)
	}

	subsystemNQN := expectedConns[0].Nqn
	maxSeenMu.Lock()
	if len(expectedConns) > maxSeenPathsMap[subsystemNQN] {
		maxSeenPathsMap[subsystemNQN] = len(expectedConns)
	}
	maxSeenMu.Unlock()

	ctrlLossTmo := initiator.DefaultCtrlLossTmo

	optConn := expectedConns[0]
	nonOptConns := expectedConns[1:]

	activeOpt := filterByANA(activePaths, initiator.ANAStateOptimized)

	var activeNonOpt []initiator.Path
	for _, p := range activePaths {
		if initiator.ParseAddress(p.Address) != optConn.IP {
			activeNonOpt = append(activeNonOpt, p)
		}
	}

	reconcileOptimizedPath(sbcClient, nodeInfo, devicePath, optConn, activeOpt, ctrlLossTmo)
	reconcileNonOptimizedPaths(sbcClient, nodeInfo, devicePath, nonOptConns, activeNonOpt, ctrlLossTmo)

	// The reconciles above can only connect what is missing, and the failure that
	// matters most is not a missing controller: it is a controller that exists,
	// is live, and contributes no path to this namespace. `nvme connect` refuses
	// it with "already connected", so the reconcile re-issues a connect that never
	// reaches the target and the volume stays below its published redundancy
	// indefinitely. Repairing that needs a teardown, which is what this does.
	fabric.HealMonitoredVolume(context.Background(), subsystemNQN, lvolID, expectedConns)

	return nil
}

//nolint:unparam // devicePath kept for parity with reconcileNonOptimizedPaths
func reconcileOptimizedPath(
	sbcClient *controlplane.ClusterClient,
	nodeInfo *controlplane.NodeInfo,
	devicePath string,
	conn *controlplane.LvolConnectResp,
	active []initiator.Path,
	ctrlLossTmo int,
) {
	if len(active) == 0 {
		if !isNodeOnline(context.Background(), sbcClient, nodeInfo.NodeID, conn.IP, conn.Port) {
			klog.Infof("reconcileOptimizedPath: primary node %s not yet online, skipping", nodeInfo.NodeID)
			return
		}
		klog.Infof("reconcileOptimizedPath: connecting missing optimized path ip=%s", conn.IP)
		if err := initiator.ConnectViaNVMe(context.Background(), conn, ctrlLossTmo, 1); err != nil {
			klog.Errorf("reconcileOptimizedPath: connect to %s failed: %v", conn.IP, err)
		}
		return
	}

	activeIP := initiator.ParseAddress(active[0].Address)
	if activeIP == conn.IP {
		return
	}

	if !isNodeOnline(context.Background(), sbcClient, nodeInfo.NodeID, conn.IP, conn.Port) {
		klog.Infof(
			"reconcileOptimizedPath: primary node %s not yet online, skipping IP change reconnect",
			nodeInfo.NodeID,
		)
		return
	}
	if err := initiator.ConnectViaNVMe(context.Background(), conn, ctrlLossTmo, 1); err != nil {
		klog.Errorf("reconcileOptimizedPath: connect to new IP %s failed: %v", conn.IP, err)
	}
}

// reconcileNonOptimizedPaths handles connections[1..N] (secondary nodes).
// Works for both 2-path (1 secondary) and 3-path (2 secondaries).
//
//nolint:unparam // devicePath kept for parity with reconcileOptimizedPath
func reconcileNonOptimizedPaths(
	sbcClient *controlplane.ClusterClient,
	nodeInfo *controlplane.NodeInfo,
	devicePath string,
	conns []*controlplane.LvolConnectResp,
	active []initiator.Path,
	ctrlLossTmo int,
) {
	if len(conns) == 0 {
		return
	}

	missing := missingEndpoints(conns, active)

	onlineSecondaries := 0
	totalSecondaries := 0
	for _, nodeID := range nodeInfo.Nodes {
		if nodeID == nodeInfo.NodeID {
			continue // skip primary
		}
		totalSecondaries++
		if isNodeOnline(context.Background(), sbcClient, nodeID, "", 0) {
			onlineSecondaries++
		}
	}
	if totalSecondaries > 0 && onlineSecondaries == 0 {
		klog.Infof("reconcileNonOptimizedPaths: all %d secondary node(s) offline, skipping", totalSecondaries)
		return
	}

	if len(conns) > 0 && !isAnyConnReachable(context.Background(), conns) {
		klog.Infof("reconcileNonOptimizedPaths: no secondary NVMe-oF endpoints TCP-reachable, skipping")
		return
	}

	for _, conn := range missing {
		if !isTCPReachable(context.Background(), conn.IP, conn.Port) {
			klog.Infof("reconcileNonOptimizedPaths: %s:%d not TCP-reachable, skipping", conn.IP, conn.Port)
			continue
		}
		klog.Infof("reconcileNonOptimizedPaths: connecting missing path %s:%d", conn.IP, conn.Port)
		if err := initiator.ConnectViaNVMe(context.Background(), conn, ctrlLossTmo, 1); err != nil {
			klog.Errorf("reconcileNonOptimizedPaths: connect to %s:%d failed: %v", conn.IP, conn.Port, err)
		}
	}
}

// missingEndpoints returns the published connections that have no controller attached,
// matching an expected endpoint against an attached one by address *and* port.
//
// The port is the whole point. A storage node listens for one subsystem on several ports,
// so 10.0.0.112:4426 and 10.0.0.112:4428 are different endpoints on one node — and
// matching on the address alone let any controller on a node stand in for every endpoint
// on it. A stale controller left at a port the control plane no longer publishes then
// read as "this node is already connected", and the endpoint it does publish was never
// connected at all: the volume sat below its published redundancy for as long as the
// stale controller survived, with a reconcile running every tick and finding nothing to
// do.
//
// An attached endpoint the control plane no longer publishes is ignored rather than
// disconnected. An endpoint missing from the current answer is not necessarily gone — a
// node in restart looks exactly the same — and tearing down a live data path on that
// evidence is not a decision to make from here; atlas diagnoses these as
// DefectStaleEndpoint and refuses to repair them unattended for the same reason. What
// bounds them is ctrl_loss_tmo, which is why DefaultCtrlLossTmo is a minute.
//
// A controller that is attached but cannot serve — stuck connecting, or live and
// exporting no namespace — still counts as attached here, and deliberately: connecting
// its endpoint again would add a second controller for one endpoint rather than replace
// the broken one. Those are repaired by tearing them down, which healMonitoredVolume does
// through atlas, and reconnected by the tick after that.
func missingEndpoints(conns []*controlplane.LvolConnectResp, active []initiator.Path) []*controlplane.LvolConnectResp {
	attached := make(map[string]bool, len(active))
	for _, p := range active {
		if ip, port := parseEndpoint(p.Address); ip != "" && port != "" {
			attached[net.JoinHostPort(ip, port)] = true
		}
	}

	missing := make([]*controlplane.LvolConnectResp, 0, len(conns))
	for _, conn := range conns {
		if !attached[net.JoinHostPort(conn.IP, strconv.Itoa(conn.Port))] {
			missing = append(missing, conn)
		}
	}
	return missing
}

// parseEndpoint splits an NVMe controller address attribute into its target address and
// port — the two halves that together identify one endpoint.
func parseEndpoint(address string) (ip, port string) {
	for _, part := range strings.Split(address, ",") {
		switch {
		case strings.HasPrefix(part, "traddr="):
			ip = strings.TrimPrefix(part, "traddr=")
		case strings.HasPrefix(part, "trsvcid="):
			port = strings.TrimPrefix(part, "trsvcid=")
		}
	}
	return ip, port
}

// filterByANA returns the subset of paths whose ANAState matches anaState.
func filterByANA(paths []initiator.Path, anaState string) []initiator.Path {
	var result []initiator.Path
	for _, p := range paths {
		if p.ANAState == anaState {
			result = append(result, p)
		}
	}
	return result
}
