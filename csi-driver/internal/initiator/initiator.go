// Package initiator attaches a volume's NVMe-oF subsystem to this node and
// resolves the block device it exports, and tears both down again.
//
// It also owns the primitives that reading and changing the local fabric is
// built from — the nvme-cli queries, the /dev/disk/by-id resolution, and the
// record of which device currently backs which logical volume — because the
// connection monitor in the reconnect package needs the same ones and must not
// grow a second copy.
package initiator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/simplyblock/atlas/nqn"
	"k8s.io/klog"

	"github.com/simplyblock/csi-driver/internal/clusters"
	"github.com/simplyblock/csi-driver/internal/controlplane"
	"github.com/simplyblock/csi-driver/internal/fabric"
	"github.com/simplyblock/csi-driver/internal/kubernetes/volumehandle"
)

const (
	// devByIDNamespacePattern matches the by-id symlink of one specific
	// namespace of a subsystem, e.g. nvme-<model>_<serial>_<nsid>. The
	// identifier is matched as a substring because udev prefixes the link
	// with the transport ("nvme-") and appends the controller serial.
	devByIDNamespacePattern = "*%s*_%d"
	// devByIDAnyNamespacePattern matches the by-id symlinks of every
	// namespace of a subsystem, whatever their nsid.
	devByIDAnyNamespacePattern = "*%s*_[0-9]*"
	// devByIDNSUUIDPattern matches the kernel-created nvme-uuid.<uuid> symlink
	// produced from the NVMe namespace NSUUID field. Failover clones of
	// namespaced (multi-namespace) volumes land here: the clone's own UUID is
	// set as NSUUID, creating nvme-uuid.<clone-uuid> with no _<nsid> suffix,
	// so the _<nsid>-based patterns above cannot find them.
	devByIDNSUUIDPattern = "nvme-uuid.%s"

	// defaultDevDiskByID is the udev directory holding the persistent device
	// symlinks scanned to find a namespace's block device.
	defaultDevDiskByID = "/dev/disk/by-id"

	// deviceReadyAttempts and deviceGoneAttempts bound how many times Connect
	// and Disconnect rescan the by-id directory before giving up.
	deviceReadyAttempts = 10
	deviceGoneAttempts  = 20

	// TargetTypeNVMf is the target type for NVMe over Fabrics
	TargetTypeTCP  = "tcp"
	TargetTypeRDMA = "rdma"

	// DefaultCtrlLossTmo is the NVMe-oF controller loss timeout in seconds.
	DefaultCtrlLossTmo = 60

	// anaStateOptimized is the ANA state nvme-cli reports for the path the
	// kernel prefers for I/O.
	ANAStateOptimized = "optimized"

	// nvmeQueryTimeoutSeconds bounds read-only "nvme list"/"nvme list-subsys"
	// queries. MonitorConnection is a single, sequential loop with no
	// concurrency of its own; without this timeout, a stuck nvme-cli/kernel
	// call would block that goroutine forever, silently disabling path
	// recovery and guardian broken-lvol detection for the rest of the
	// process's life.
	nvmeQueryTimeoutSeconds = 10
)

// devByIDPartitionSuffix matches the partition suffix udev appends to the by-id
// link of the whole device it was derived from, e.g. "..._ha_1-part1".
var devByIDPartitionSuffix = regexp.MustCompile(`[-_]part[0-9]+$`)

// Initiator attaches one volume's subsystem and resolves its block device.
//   - Connect initiates target connection and returns local block device filename
//     e.g., /dev/disk/by-id/nvme-SPDK_Controller1_SPDK00000000000001
//   - Disconnect terminates target connection
//   - Caller(node service) should serialize calls to same initiator
//   - Implementation should be idempotent to duplicated requests
type Initiator interface {
	Connect(ctx context.Context) (string, error)
	Disconnect(ctx context.Context) error
}

// initiatorNVMf is an implementation of NVMf tcp initiator
type initiatorNVMf struct {
	lvolID         string // source lvol UUID — used for backend API calls
	deviceLvolID   string // clone UUID after failover, else same as lvolID — used for local device lookup
	targetType     string
	nqn            string
	reconnectDelay string
	nrIoQueues     string
	ctrlLossTmo    string
	model          string
	nsId           int
	hostIface      string
	hostNQN        string
	poolID         string
	clusterID      string // explicit cluster override; empty means derive from NQN
}

type Path struct {
	Name      string `json:"Name"`
	Transport string `json:"Transport"`
	Address   string `json:"Address"`
	State     string `json:"State"`
	ANAState  string `json:"ANAState"`
}

type Subsystem struct {
	Name  string `json:"Name"`
	NQN   string `json:"NQN"`
	Paths []Path `json:"Paths"`
}

type SubsystemResponse struct {
	Subsystems []Subsystem `json:"Subsystems"`
}

type DeviceInfo struct {
	DevicePath   string
	SerialNumber string
	LvolID       string // UUID from /sys/block/<dev>/uuid — set for namespaced LVols
}

// New creates an Initiator for the target type named in the volume context.
func New(volumeContext map[string]string) (Initiator, error) {
	targetType := strings.ToLower(volumeContext["targetType"])
	klog.Infof("Simplyblock targetType created :%s", targetType)
	nsId, err := strconv.Atoi(volumeContext["nsId"])
	if err != nil {
		return nil, fmt.Errorf("failed to convert namespace ID %s to integer: %w", volumeContext["nsId"], err)
	}
	if nsId < 1 {
		return nil, fmt.Errorf("namespace ID must be greater than zero")
	}
	switch targetType {
	case TargetTypeTCP, TargetTypeRDMA:
		srcLvolID := volumeContext["uuid"]
		deviceLvolID := volumeContext["targetLvolID"]
		if deviceLvolID == "" {
			deviceLvolID = srcLvolID
		}
		return &initiatorNVMf{
			nsId:           nsId,
			targetType:     volumeContext["targetType"],
			nqn:            volumeContext["nqn"],
			reconnectDelay: volumeContext["reconnectDelay"],
			nrIoQueues:     volumeContext["nrIoQueues"],
			ctrlLossTmo:    volumeContext["ctrlLossTmo"],
			model:          volumeContext["model"],
			hostIface:      volumeContext["hostIface"],
			hostNQN:        volumeContext["hostNQN"],
			poolID:         volumeContext["poolID"],
			clusterID:      volumeContext["cluster_id"],
			lvolID:         srcLvolID,
			deviceLvolID:   deviceLvolID,
		}, nil

	default:
		return nil, fmt.Errorf("unknown initiator: %s", targetType)
	}
}

func execWithTimeoutRetry(ctx context.Context, cmdLine []string, timeout, retry int) (err error) {
	for retry > 0 {
		err = execWithTimeout(ctx, cmdLine, timeout)
		if err == nil {
			return nil
		}
		retry--
	}
	return err
}

// Connect attaches the volume's subsystem and returns its block device.
//
// A connected subsystem is not the same thing as a usable one: it can be attached
// with live controllers and export no namespace at all, in which case waiting can
// never produce a device — every retry short-circuits on the existing connection
// and times out on device discovery, and kubelet retries NodeStageVolume forever.
// So a failed device lookup is diagnosed rather than simply returned, and if the
// fabric could be repaired the attach is tried once more. See nvmerepair.go.
func (nvmf *initiatorNVMf) Connect(ctx context.Context) (string, error) {
	devicePath, err := nvmf.connectOnce(ctx)
	if err != nil && fabric.RepairAttach(ctx, nvmf.nqn, nvmf.nsId) {
		klog.Infof("Connect: retrying attach of %s after a fabric repair", nvmf.nqn)
		devicePath, err = nvmf.connectOnce(ctx)
	}
	if err != nil {
		return "", err
	}

	nvmf.registerDevicePresence(devicePath)
	return devicePath, nil
}

// connectOnce establishes the volume's paths if they are not up and looks up its
// block device, without repairing anything.
func (nvmf *initiatorNVMf) connectOnce(ctx context.Context) (string, error) {
	alreadyConnected, err := isNqnConnected(ctx, nvmf.nqn)
	if err != nil {
		klog.Errorf("Failed to check existing connections: %v", err)
		return "", err
	}

	if !alreadyConnected {
		clusterID := nvmf.clusterID
		if clusterID == "" {
			if subsystem, ok := nqn.Parse(nvmf.nqn); ok {
				clusterID = subsystem.ClusterID
			}
		}
		// the lvolID from NQN gives the master LvolID of the subsystem
		// Although the connection string is same for all the lvols in the subsystem,
		// volume/<lvol-id>/connect/ connect API return 404 if master lvol is deleted
		// so using the actual lvolID instead instead of master lvol ID
		lvolID := nvmf.lvolID
		sbcClient, err := clusters.Client(ctx, clusterID, nvmf.poolID)
		if err != nil {
			klog.Errorf("failed to create SPDK client: %v", err)
			return "", err
		}
		connections, err := sbcClient.LvolConnections(ctx, lvolID, nvmf.hostNQN)
		if err != nil {
			klog.Errorf("Failed to get lvol connection: %v", err)
			return "", err
		}

		ctrlLossTmo := DefaultCtrlLossTmo

		connected := 0
		var lastErr error

		for _, conn := range connections {
			err := ConnectViaNVMe(ctx, conn, ctrlLossTmo, len(connections))
			if err != nil {
				klog.Errorf("nvme connect failed for %s:%d: %v", conn.IP, conn.Port, err)
				lastErr = err
				continue
			}
			connected++
		}
		if connected == 0 {
			return "", fmt.Errorf(
				"failed to connect to any NVMe path for NQN %s: error: %v",
				nvmf.nqn, lastErr,
			)
		}
	}

	return matchNamespaceDevice(ctx, defaultDevDiskByID, nvmf.model, nvmf.deviceLvolID, nvmf.nsId, time.Second)
}

// registerDevicePresence records a freshly connected device in the shared
// presence maps instead of waiting for the next MonitorConnection poll to
// discover it. Without this, a device that connects and then loses all paths
// faster than one poll interval (~3s+jitter) is never seen as "present", so the
// guardian's gone-device detection in reconnectSubsystems has nothing to diff
// against and can silently miss the loss forever.
func (nvmf *initiatorNVMf) registerDevicePresence(devicePath string) {
	realPath, err := filepath.EvalSymlinks(devicePath)
	if err != nil {
		klog.Warningf("Connect: failed to resolve device path %s for lvol %s: %v", devicePath, nvmf.lvolID, err)
		return
	}
	MarkDevicePresent(realPath, nvmf.lvolID)
}

func (nvmf *initiatorNVMf) Disconnect(ctx context.Context) error {
	deviceGlob := anyNamespaceDeviceGlob(defaultDevDiskByID, nvmf.model)
	matches, err := listNamespaceDevices(deviceGlob)
	if err != nil {
		return fmt.Errorf("failed to find device paths matching %s: %w", deviceGlob, err)
	}

	devicePath, shared := selectDisconnectTarget(matches)
	if shared {
		klog.Infof("Keeping subsystem of model %s connected, %d namespaces still in use", nvmf.model, len(matches))
		return nil
	}
	if devicePath != "" {
		if err := disconnectDevicePath(ctx, devicePath); err != nil {
			return err
		}
	}

	return waitForDeviceGone(ctx, deviceGlob, deviceGoneAttempts, time.Second)
}

// selectDisconnectTarget decides which device a Disconnect tears down:
//
//   - no match: nothing to disconnect
//   - one match: that device's controller can be torn down
//   - several matches: further namespaces of the same subsystem are still
//     connected on this host, and tearing down the controller would take their
//     devices down with it, so the subsystem has to stay up (shared == true)
func selectDisconnectTarget(matches []string) (devicePath string, shared bool) {
	switch len(matches) {
	case 0:
		return "", false
	case 1:
		return matches[0], false
	default:
		return "", true
	}
}

// namespaceDeviceGlob returns the glob matching the by-id symlink of namespace
// nsID of the subsystem identified by id — a model or an lvol UUID. The
// identifier is matched as a substring because udev prefixes the link with the
// transport ("nvme-") and appends the controller serial before the nsid suffix.
func namespaceDeviceGlob(byIDDir, id string, nsID int) string {
	return filepath.Join(byIDDir, fmt.Sprintf(devByIDNamespacePattern, id, nsID))
}

// anyNamespaceDeviceGlob returns the glob matching the by-id symlinks of every
// namespace of the subsystem identified by id, whatever their nsid.
func anyNamespaceDeviceGlob(byIDDir, id string) string {
	return filepath.Join(byIDDir, fmt.Sprintf(devByIDAnyNamespacePattern, id))
}

// listNamespaceDevices returns the whole-namespace links matching deviceGlob,
// dropping the partition links derived from them.
//
// The glob cannot do this alone: "_[0-9]*" ends in a wildcard, so a partitioned
// namespace matches twice. Counting the partition would make a single-namespace
// subsystem look like it still carries siblings, and its controller would never
// be torn down.
func listNamespaceDevices(deviceGlob string) ([]string, error) {
	matches, err := filepath.Glob(deviceGlob)
	if err != nil {
		return nil, err
	}
	devices := make([]string, 0, len(matches))
	for _, match := range matches {
		if !devByIDPartitionSuffix.MatchString(filepath.Base(match)) {
			devices = append(devices, match)
		}
	}
	return devices, nil
}

// nsuuidDeviceGlob returns the glob matching the kernel-created nvme-uuid.<id>
// symlink produced from the NVMe namespace NSUUID field (no nsID suffix).
func nsuuidDeviceGlob(byIDDir, id string) string {
	return filepath.Join(byIDDir, fmt.Sprintf(devByIDNSUUIDPattern, id))
}

// matchNamespaceDevice waits in byIDDir for the block device of namespace nsID
// to show up. It tries three patterns in order:
//  1. *<model>*_<nsID>  — subsystem model carried by every namespace link.
//  2. *<lvolID>*_<nsID> — clone UUID with nsID suffix (_ha_N udev rule).
//  3. nvme-uuid.<lvolID> — kernel NSUUID symlink (no nsID suffix); produced for
//     failover clones of namespaced volumes whose NSUUID = clone UUID.
func matchNamespaceDevice(
	ctx context.Context,
	byIDDir, model, lvolID string,
	nsID int,
	pollInterval time.Duration,
) (string, error) {
	deviceGlob := namespaceDeviceGlob(byIDDir, model, nsID)
	deviceGlobFallback := namespaceDeviceGlob(byIDDir, lvolID, nsID)
	deviceGlobNSUUID := nsuuidDeviceGlob(byIDDir, lvolID)

	devicePath, primaryErr := waitForDeviceReady(ctx, deviceGlob, deviceReadyAttempts, pollInterval)
	if primaryErr == nil {
		return devicePath, nil
	}

	klog.Warningf("New device symlink not found (%s). Retrying fallback format: %s", deviceGlob, deviceGlobFallback)
	devicePath, err := waitForDeviceReady(ctx, deviceGlobFallback, deviceReadyAttempts, pollInterval)
	if err == nil {
		return devicePath, nil
	}

	// Failover clones of namespaced volumes land under nvme-uuid.<clone-uuid>
	// (no _<nsID> suffix) because the NSUUID is set to the clone's own UUID.
	klog.Warningf("New device symlink not found (%s). Retrying NSUUID format: %s", deviceGlobFallback, deviceGlobNSUUID)
	devicePath, nsuuidErr := waitForDeviceReady(ctx, deviceGlobNSUUID, deviceReadyAttempts, pollInterval)
	if nsuuidErr == nil {
		return devicePath, nil
	}

	return "", fmt.Errorf("device not found in model (%s), fallback (%s), or NSUUID (%s) formats: %w",
		deviceGlob, deviceGlobFallback, deviceGlobNSUUID,
		errors.Join(primaryErr, err, nsuuidErr))
}

// waitForDeviceReady rescans deviceGlob until it resolves to a single device,
// waiting pollInterval between attempts. With attempts set to 0 it scans once
// and returns immediately.
func waitForDeviceReady(
	ctx context.Context,
	deviceGlob string,
	attempts int,
	pollInterval time.Duration,
) (string, error) {
	var lastErr error
	for i := 0; ; i++ {
		matches, err := filepath.Glob(deviceGlob)
		if err != nil {
			return "", err
		}
		switch {
		case len(matches) == 1:
			return matches[0], nil
		case len(matches) > 1:
			// Several links under /dev/disk/by-id/ usually point at the same
			// device, which is fine. But a broken matcher may match multiple
			// devices, which is not fine. Also, a dangling device from a
			// previous connect may match — that one goes away shortly, so keep
			// scanning instead of failing the connect outright.
			match, err := resolveToSameDevice(matches)
			if err == nil {
				return match, nil
			}
			lastErr = err
			klog.Warningf("device glob %s has not settled yet: %v", deviceGlob, err)
		}
		// Never sleep after the last scan: with attempts set to 0 the caller
		// asked for a single immediate look.
		if i >= attempts {
			break
		}
		select {
		case <-time.After(pollInterval):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if lastErr != nil {
		return "", fmt.Errorf("timed out waiting device ready: %s: %w", deviceGlob, lastErr)
	}
	return "", fmt.Errorf("timed out waiting device ready: %s", deviceGlob)
}

// resolveToSameDevice resolves every path in matches and returns the first one
// if they all point at the same device. It fails if a path cannot be resolved or
// if the resolved targets diverge.
func resolveToSameDevice(matches []string) (string, error) {
	var target string
	for _, match := range matches {
		resolved, err := filepath.EvalSymlinks(match)
		if err != nil {
			return "", fmt.Errorf("failed to resolve device path %s: %w", match, err)
		}
		if target == "" {
			target = resolved
			continue
		}
		if resolved != target {
			return "", fmt.Errorf("matches resolve to different devices: %s -> %s, %s -> %s",
				matches[0], target, match, resolved)
		}
	}
	return matches[0], nil
}

// waitForDeviceGone rescans deviceGlob until no namespace device is left,
// waiting pollInterval between attempts. Partition links are ignored for the
// same reason Disconnect ignores them: they go away with their parent device.
func waitForDeviceGone(ctx context.Context, deviceGlob string, attempts int, pollInterval time.Duration) error {
	for i := 0; ; i++ {
		matches, err := listNamespaceDevices(deviceGlob)
		if err != nil {
			return err
		}
		if len(matches) == 0 {
			return nil
		}
		// Never sleep after the last scan; attempts set to 0 means a single
		// immediate look, as in waitForDeviceReady.
		if i >= attempts {
			break
		}
		select {
		case <-time.After(pollInterval):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return fmt.Errorf("timed out waiting device gone: %s", deviceGlob)
}

// exec shell command with timeout(in seconds)
func execWithTimeout(ctx context.Context, cmdLine []string, timeout int) error {
	execCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	klog.Infof("running command: %v", cmdLine)
	//nolint:gosec // execWithTimeout assumes valid cmd arguments
	cmd := exec.CommandContext(execCtx, cmdLine[0], cmdLine[1:]...)
	output, err := cmd.CombinedOutput()

	if errors.Is(execCtx.Err(), context.DeadlineExceeded) {
		return errors.New("timed out")
	}
	if output != nil {
		klog.Infof("command returned: %s", output)
	}
	if err != nil && len(output) > 0 {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}
	return err
}

// execNVMeQuery runs a read-only "nvme" CLI query (list/list-subsys) bounded
// by nvmeQueryTimeoutSeconds, so a stuck nvme-cli/kernel call can never block
// a caller forever — notably the single-threaded reconnect monitor loop,
// which has no other goroutine to pick up the work if this one wedges.
func execNVMeQuery(ctx context.Context, cmdLine ...string) ([]byte, error) {
	execCtx, cancel := context.WithTimeout(ctx, nvmeQueryTimeoutSeconds*time.Second)
	defer cancel()

	//nolint:gosec // execNVMeQuery assumes valid cmd arguments
	cmd := exec.CommandContext(execCtx, cmdLine[0], cmdLine[1:]...)
	output, err := cmd.Output()
	if errors.Is(execCtx.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("timed out running %v", cmdLine)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to execute %v: %w", cmdLine, err)
	}
	return output, nil
}

func disconnectDevicePath(ctx context.Context, devicePath string) error {
	var paths []Path

	realPath, err := filepath.EvalSymlinks(devicePath)
	if err != nil {
		return fmt.Errorf("failed to resolve device path from %s: %w", devicePath, err)
	}

	subsystems, err := SubsystemsForDevice(ctx, realPath)
	if err != nil {
		return fmt.Errorf("failed to get subsystems for %s: %w", realPath, err)
	}

	for _, host := range subsystems {
		for _, subsystem := range host.Subsystems {
			for _, p := range subsystem.Paths {
				paths = append(paths, Path{
					Name:     p.Name,
					ANAState: p.ANAState,
				})
			}
		}
	}

	sort.Slice(paths, func(i, j int) bool {
		if paths[i].ANAState == ANAStateOptimized && paths[j].ANAState != ANAStateOptimized {
			return false
		}
		return true
	})

	for _, p := range paths {
		klog.Infof("Disconnecting device %s", p.Name)
		disconnectCmd := []string{"nvme", "disconnect", "-d", p.Name}
		err := execWithTimeoutRetry(ctx, disconnectCmd, 40, 1)
		if err != nil {
			klog.Errorf("Failed to disconnect device %s: %v", p.Name, err)
		}
	}

	ForgetDevice(realPath)

	return nil
}

// logicalVolumeIdByDevicePath reads /sys/block/<dev>/uuid for a device path like /dev/nvme0n2.
// Returns an empty string if the file is absent, unreadable, or not a valid UUID.
func logicalVolumeIdByDevicePath(devicePath string) string {
	name := filepath.Base(devicePath)
	data, err := os.ReadFile(filepath.Join("/sys/block", name, "uuid"))
	if err != nil {
		return ""
	}
	uuid := strings.TrimSpace(string(data))
	if !volumehandle.IsUUID(uuid) {
		return ""
	}
	return uuid
}

func NVMeDevices(ctx context.Context) ([]DeviceInfo, error) {
	output, err := execNVMeQuery(ctx, "nvme", "list", "-o", "json")
	if err != nil {
		return nil, err
	}

	var deviceResponse struct {
		Devices []struct {
			Subsystems []struct {
				Namespaces []struct {
					NameSpace string `json:"NameSpace"`
				} `json:"Namespaces"`
			} `json:"Subsystems"`
		} `json:"Devices"`
	}
	if err := json.Unmarshal(output, &deviceResponse); err == nil {
		var devices []DeviceInfo
		for _, host := range deviceResponse.Devices {
			for _, sub := range host.Subsystems {
				for _, ns := range sub.Namespaces {
					if ns.NameSpace == "" {
						continue
					}
					dp := "/dev/" + ns.NameSpace
					devices = append(devices, DeviceInfo{
						DevicePath: dp,
						LvolID:     logicalVolumeIdByDevicePath(dp),
					})
				}
			}
		}
		if len(devices) > 0 {
			return devices, nil
		}
	}

	// Legacy flat format: Devices[].DevicePath
	var legacyDeviceResp struct {
		Devices []struct {
			DevicePath   string `json:"DevicePath"`
			SerialNumber string `json:"SerialNumber"`
		} `json:"Devices"`
	}
	if err := json.Unmarshal(output, &legacyDeviceResp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal nvme list output: %v", err)
	}
	var devices []DeviceInfo
	for _, dev := range legacyDeviceResp.Devices {
		if dev.DevicePath == "" {
			continue
		}
		devices = append(devices, DeviceInfo{
			DevicePath:   dev.DevicePath,
			SerialNumber: dev.SerialNumber,
			LvolID:       logicalVolumeIdByDevicePath(dev.DevicePath),
		})
	}
	return devices, nil
}

func isNqnConnected(ctx context.Context, subsystemNQN string) (bool, error) {
	output, err := execNVMeQuery(ctx, "nvme", "list-subsys", "-o", "json")
	if err != nil {
		return false, err
	}

	var subsystems []SubsystemResponse
	if err := json.Unmarshal(output, &subsystems); err != nil {
		return false, fmt.Errorf("failed to unmarshal nvme list-subsys output: %v", err)
	}
	for _, host := range subsystems {
		for _, s := range host.Subsystems {
			if s.NQN == subsystemNQN {
				return true, nil
			}
		}
	}
	return false, nil
}

func SubsystemsForDevice(ctx context.Context, devicePath string) ([]SubsystemResponse, error) {
	output, err := execNVMeQuery(ctx, "nvme", "list-subsys", "-o", "json", devicePath)
	if err != nil {
		return nil, err
	}

	var subsystems []SubsystemResponse
	if err := json.Unmarshal(output, &subsystems); err != nil {
		return nil, fmt.Errorf("failed to unmarshal nvme list-subsys output: %v", err)
	}

	return subsystems, nil
}

func ParseAddress(address string) string {
	parts := strings.Split(address, ",")
	for _, part := range parts {
		if strings.HasPrefix(part, "traddr=") {
			return strings.TrimPrefix(part, "traddr=")
		}
	}
	return ""
}

// dhchapAuthArgs extracts the --hostnqn, --dhchap-secret, --dhchap-ctrl-secret,
// and --tls flags from the control-plane-supplied nvme-connect command line.
// The control plane is the only party that resolves the connecting host's
// DHCHAP secret (pool-shared or per-host key material); it bakes these flags
// into LvolConnectResp.Connect rather than exposing them as separate fields
// (see build_nvme_connect_entry/HostConnectAuth in sbcli), so this is the only
// channel the CSI driver has for them today.
//
// Whenever --hostnqn is present, this also adds a --hostid derived from that
// same NQN's UUID. Without an explicit --hostid, nvme-cli falls back to the
// node's static /etc/nvme/hostid (written once, node-wide, by the CSI
// DaemonSet's postStart hook) — shared by every connect on that node
// regardless of --hostnqn. The kernel refuses to associate one hostid with
// two different hostnqns ("found same hostid ... but different hostnqn"), so
// a node with even one pre-existing default-hostnqn connection (the common
// case: any plain, non-gated volume) would reject every later connect that
// names an explicit, different hostnqn — exactly what allowed_hosts/DHCHAP
// volumes need. Deriving hostid from hostnqn's own UUID keeps the pair
// internally consistent and never collides with the node's random
// file-based default.
func dhchapAuthArgs(connectCmd string) []string {
	var args []string
	var hostNQN string
	for _, field := range strings.Fields(connectCmd) {
		switch {
		case strings.HasPrefix(field, "--hostnqn="):
			hostNQN = strings.TrimPrefix(field, "--hostnqn=")
			args = append(args, field)
		case strings.HasPrefix(field, "--dhchap-secret="),
			strings.HasPrefix(field, "--dhchap-ctrl-secret="),
			field == "--tls":
			args = append(args, field)
		}
	}
	if hostID, ok := nqn.HostUUID(hostNQN); ok {
		args = append(args, "--hostid="+hostID)
	}
	return args
}

func ConnectViaNVMe(ctx context.Context, conn *controlplane.LvolConnectResp, ctrlLossTmo int, retries int) error {
	cmd := []string{
		"nvme", "connect", "-t", strings.ToLower(conn.TargetType),
		"-a", conn.IP, "-s", strconv.Itoa(conn.Port),
		"-n", conn.Nqn,
		"-l", strconv.Itoa(ctrlLossTmo),
		"-c", strconv.Itoa(conn.ReconnectDelay),
		"-i", strconv.Itoa(conn.NrIoQueues),
	}
	if conn.HostIface != "" {
		cmd = append(cmd, "-f", conn.HostIface)
	}
	// conn.Connect is the full "nvme connect ..." line the control plane
	// built for this exact host NQN — the only source of the DHCHAP/host
	// identity flags below, since LvolConnectResp carries no separate fields
	// for them.
	cmd = append(cmd, dhchapAuthArgs(conn.Connect)...)
	if err := execWithTimeoutRetry(ctx, cmd, 40, retries); err != nil {
		if strings.Contains(err.Error(), "already connected") {
			return nil
		}
		klog.Errorf("nvme connect failed: %v", err)
		return err
	}
	return nil
}
