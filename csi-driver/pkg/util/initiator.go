/*
Copyright (c) Arm Limited and Contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package util

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog"

	sbkube "github.com/spdk/spdk-csi/pkg/kubernetes"
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
	anaStateOptimized = "optimized"

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

// SpdkCsiInitiator defines interface for NVMeoF/iSCSI initiator
//   - Connect initiates target connection and returns local block device filename
//     e.g., /dev/disk/by-id/nvme-SPDK_Controller1_SPDK00000000000001
//   - Disconnect terminates target connection
//   - Caller(node service) should serialize calls to same initiator
//   - Implementation should be idempotent to duplicated requests
type SpdkCsiInitiator interface {
	Connect(ctx context.Context) (string, error)
	Disconnect(ctx context.Context) error
}

// initiatorNVMf is an implementation of NVMf tcp initiator
type initiatorNVMf struct {
	lvolID         string
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
}

type path struct {
	Name      string `json:"Name"`
	Transport string `json:"Transport"`
	Address   string `json:"Address"`
	State     string `json:"State"`
	ANAState  string `json:"ANAState"`
}

type subsystem struct {
	Name  string `json:"Name"`
	NQN   string `json:"NQN"`
	Paths []path `json:"Paths"`
}

type subsystemResponse struct {
	Subsystems []subsystem `json:"Subsystems"`
}

type NodeInfo struct {
	NodeID string   `json:"storage_node_id"` // v2 VolumeDTO field
	Nodes  []string `json:"nodes"`           // URL paths in v2; converted to UUIDs after parsing
	Status string   `json:"status"`
}

type nvmeDeviceInfo struct {
	devicePath   string
	serialNumber string
	lvolID       string // UUID from /sys/block/<dev>/uuid — set for namespaced LVols
}

var (
	devicePresentMap  = make(map[string]bool)
	deviceToLvolIDMap = make(map[string]string)
	mu                sync.Mutex

	// maxSeenPathsMap caches the highest number of active NVMe-oF paths ever
	// observed per NQN. Used by the connection monitor to detect degradation
	// without querying the API on every cycle.
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

// clusterConfig represents the Kubernetes secret structure
type ClusterConfig struct {
	ClusterID       string `json:"cluster_id"`
	ClusterEndpoint string `json:"cluster_endpoint"`
	ClusterSecret   string `json:"cluster_secret"`
}

type ClustersInfo struct {
	Clusters []ClusterConfig `json:"clusters"`
}

// NewsimplyBlockClient creates a new Simplyblock client scoped to a cluster and optionally a pool.
// poolIDOrName may be a pool UUID (used as-is) or a pool name (resolved via API), or empty
// (no pool context — only cluster-level operations will work).
func NewsimplyBlockClient(ctx context.Context, clusterID, poolIDOrName string) (*ClusterClient, error) {
	secretFile := FromEnv("SPDKCSI_SECRET", "/etc/spdkcsi-secret/secret.json")
	var clusters ClustersInfo
	err := ParseJSONFile(secretFile, &clusters)
	if err != nil {
		return nil, fmt.Errorf("failed to parse secret file: %w", err)
	}

	var clusterConfig *ClusterConfig
	for _, cluster := range clusters.Clusters {
		if cluster.ClusterID == clusterID {
			clusterConfig = &cluster
			break
		}
	}

	if clusterConfig == nil {
		return nil, fmt.Errorf("failed to find secret for clusterID %s: %w", clusterID, ErrClusterNotFound)
	}

	if clusterConfig.ClusterEndpoint == "" {
		return nil, fmt.Errorf("invalid cluster configuration for clusterID %s: missing endpoint", clusterID)
	}

	// Use API token when SPDKCSI_API_TOKEN_PATH is explicitly set; otherwise fall back to cluster_secret.
	credential := clusterConfig.ClusterSecret
	if tokenPath := os.Getenv("SPDKCSI_API_TOKEN_PATH"); tokenPath != "" {
		if tokenBytes, err := os.ReadFile(tokenPath); err != nil {
			klog.Warningf(
				"SPDKCSI_API_TOKEN_PATH is set but token file %q could not be read for cluster %s: %v; falling back to cluster_secret", //nolint:lll // unwrappable string/log/signature
				tokenPath,
				clusterID,
				err,
			)
		} else if token := strings.TrimSpace(string(tokenBytes)); token == "" {
			klog.Warningf("SPDKCSI_API_TOKEN_PATH is set but token file %q is empty for cluster %s; falling back to cluster_secret", tokenPath, clusterID) //nolint:lll // unwrappable string/log/signature
		} else {
			credential = token
			klog.Infof("Using API token from file for cluster %s", clusterID)
		}
	}
	if credential == "" {
		return nil, fmt.Errorf(
			"invalid cluster configuration for clusterID %s: no cluster_secret and no API token available",
			clusterID,
		)
	}

	klog.Infof("Simplyblock client created for ClusterID:%s, Endpoint:%s",
		clusterConfig.ClusterID,
		clusterConfig.ClusterEndpoint,
	)

	conn, err := NewConnection(clusterConfig.ClusterEndpoint)
	if err != nil {
		return nil, err
	}
	c := &ClusterClient{
		API: &APIClient{
			ClusterID:  clusterID,
			Credential: credential,
			conn:       conn,
		},
	}

	if poolIDOrName != "" {
		poolUUID, err := resolvePoolUUID(ctx, c, poolIDOrName)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve pool %q: %w", poolIDOrName, err)
		}
		c.poolID = poolUUID
	}

	return c, nil
}

// resolvePoolUUID returns poolIDOrName as-is if it is already a UUID,
// otherwise looks up the pool UUID by name via the API.
func resolvePoolUUID(ctx context.Context, c *ClusterClient, poolIDOrName string) (string, error) {
	if isUUID(poolIDOrName) {
		return poolIDOrName, nil
	}
	return c.GetPoolUUIDByName(ctx, poolIDOrName)
}

// isUUID reports whether s is a standard UUID (8-4-4-4-12 hex, with hyphens).
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
				return false
			}
		}
	}
	return true
}

// NewSpdkCsiInitiator creates a new SpdkCsiInitiator based on the target type
func NewSpdkCsiInitiator(volumeContext map[string]string) (SpdkCsiInitiator, error) {
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
			lvolID:         volumeContext["uuid"],
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
	if err != nil && nvmf.repairFabric(ctx) {
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
		clusterID, _ := getLvolIDFromNQN(nvmf.nqn)
		// the lvolID from NQN gives the master LvolID of the subsystem
		// Although the connection string is same for all the lvols in the subsystem,
		// volume/<lvol-id>/connect/ connect API return 404 if master lvol is deleted
		// so using the actual lvolID instead instead of master lvol ID
		lvolID := nvmf.lvolID
		sbcClient, err := NewsimplyBlockClient(ctx, clusterID, nvmf.poolID)
		if err != nil {
			klog.Errorf("failed to create SPDK client: %v", err)
			return "", err
		}
		connections, err := fetchLvolConnection(ctx, sbcClient, lvolID, nvmf.hostNQN)
		if err != nil {
			klog.Errorf("Failed to get lvol connection: %v", err)
			return "", err
		}

		ctrlLossTmo := DefaultCtrlLossTmo

		connected := 0
		var lastErr error

		for _, conn := range connections {
			err := connectViaNVMe(ctx, conn, ctrlLossTmo, len(connections))
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

	return matchNamespaceDevice(ctx, defaultDevDiskByID, nvmf.model, nvmf.lvolID, nvmf.nsId, time.Second)
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
	mu.Lock()
	devicePresentMap[realPath] = true
	deviceToLvolIDMap[realPath] = nvmf.lvolID
	mu.Unlock()
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

// matchNamespaceDevice waits in byIDDir for the block device of namespace nsID
// to show up. It prefers the subsystem model, which every namespace link
// carries, and falls back to the lvol UUID for volumes whose links are named
// after the lvol itself.
func matchNamespaceDevice(
	ctx context.Context,
	byIDDir, model, lvolID string,
	nsID int,
	pollInterval time.Duration,
) (string, error) {
	deviceGlob := namespaceDeviceGlob(byIDDir, model, nsID)
	deviceGlobFallback := namespaceDeviceGlob(byIDDir, lvolID, nsID)

	devicePath, primaryErr := waitForDeviceReady(ctx, deviceGlob, deviceReadyAttempts, pollInterval)
	if primaryErr == nil {
		return devicePath, nil
	}

	klog.Warningf("New device symlink not found (%s). Retrying fallback format: %s", deviceGlob, deviceGlobFallback)
	devicePath, err := waitForDeviceReady(ctx, deviceGlobFallback, deviceReadyAttempts, pollInterval)
	if err != nil {
		// Both reasons are kept: the model glob failing for a different reason
		// than the fallback glob (ambiguous match vs. nothing there at all) is
		// what tells a stale symlink apart from a missing namespace.
		return "", fmt.Errorf("device not found in both new (%s), and fallback (%s) formats: %w",
			deviceGlob, deviceGlobFallback, errors.Join(primaryErr, err))
	}
	return devicePath, nil
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
	var paths []path

	realPath, err := filepath.EvalSymlinks(devicePath)
	if err != nil {
		return fmt.Errorf("failed to resolve device path from %s: %w", devicePath, err)
	}

	subsystems, err := getSubsystemsForDevice(ctx, realPath)
	if err != nil {
		return fmt.Errorf("failed to get subsystems for %s: %w", realPath, err)
	}

	for _, host := range subsystems {
		for _, subsystem := range host.Subsystems {
			for _, p := range subsystem.Paths {
				paths = append(paths, path{
					Name:     p.Name,
					ANAState: p.ANAState,
				})
			}
		}
	}

	sort.Slice(paths, func(i, j int) bool {
		if paths[i].ANAState == anaStateOptimized && paths[j].ANAState != anaStateOptimized {
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

	mu.Lock()
	delete(devicePresentMap, realPath)
	delete(deviceToLvolIDMap, realPath)
	mu.Unlock()

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
	if !isUUID(uuid) {
		return ""
	}
	return uuid
}

func getNVMeDeviceInfos(ctx context.Context) ([]nvmeDeviceInfo, error) {
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
		var devices []nvmeDeviceInfo
		for _, host := range deviceResponse.Devices {
			for _, sub := range host.Subsystems {
				for _, ns := range sub.Namespaces {
					if ns.NameSpace == "" {
						continue
					}
					dp := "/dev/" + ns.NameSpace
					devices = append(devices, nvmeDeviceInfo{
						devicePath: dp,
						lvolID:     logicalVolumeIdByDevicePath(dp),
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
	var devices []nvmeDeviceInfo
	for _, dev := range legacyDeviceResp.Devices {
		if dev.DevicePath == "" {
			continue
		}
		devices = append(devices, nvmeDeviceInfo{
			devicePath:   dev.DevicePath,
			serialNumber: dev.SerialNumber,
			lvolID:       logicalVolumeIdByDevicePath(dev.DevicePath),
		})
	}
	return devices, nil
}

func isNqnConnected(ctx context.Context, nqn string) (bool, error) {
	output, err := execNVMeQuery(ctx, "nvme", "list-subsys", "-o", "json")
	if err != nil {
		return false, err
	}

	var subsystems []subsystemResponse
	if err := json.Unmarshal(output, &subsystems); err != nil {
		return false, fmt.Errorf("failed to unmarshal nvme list-subsys output: %v", err)
	}
	for _, host := range subsystems {
		for _, s := range host.Subsystems {
			if s.NQN == nqn {
				return true, nil
			}
		}
	}
	return false, nil
}

func getSubsystemsForDevice(ctx context.Context, devicePath string) ([]subsystemResponse, error) {
	output, err := execNVMeQuery(ctx, "nvme", "list-subsys", "-o", "json", devicePath)
	if err != nil {
		return nil, err
	}

	var subsystems []subsystemResponse
	if err := json.Unmarshal(output, &subsystems); err != nil {
		return nil, fmt.Errorf("failed to unmarshal nvme list-subsys output: %v", err)
	}

	return subsystems, nil
}

func getLvolIDFromNQN(nqn string) (clusterID, lvolID string) {
	parts := strings.Split(nqn, ":lvol:")
	if len(parts) > 1 {
		subparts := strings.Split(parts[0], ":")
		clusterID := subparts[len(subparts)-1]
		lvolID := parts[1]
		return clusterID, lvolID
	}
	return "", ""
}

func parseAddress(address string) string {
	parts := strings.Split(address, ",")
	for _, part := range parts {
		if strings.HasPrefix(part, "traddr=") {
			return strings.TrimPrefix(part, "traddr=")
		}
	}
	return ""
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

	devices, err := getNVMeDeviceInfos(ctx)
	if err != nil {
		return fmt.Errorf("failed to get NVMe device paths: %v", err)
	}

	currentDevices := make(map[string]bool)

	for _, device := range devices {
		subsystems, err := getSubsystemsForDevice(ctx, device.devicePath)
		if err != nil {
			klog.Errorf("failed to get subsystems for device %s: %v", device.devicePath, err)
			continue
		}

		currentDevices[device.devicePath] = true

		for _, host := range subsystems {
			for _, subsystem := range host.Subsystems {
				clusterID, nqnLvolID := getLvolIDFromNQN(subsystem.NQN)
				if nqnLvolID == "" {
					continue
				}
				// Prefer the sysfs UUID when available — it always identifies the
				// exact namespace LVol. Falls back to the NQN-derived ID.
				lvolID := device.lvolID
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
				mu.Lock()
				devicePresentMap[device.devicePath] = true
				deviceToLvolIDMap[device.devicePath] = lvolID
				mu.Unlock()

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

				if !confirmSubsystemNeedsRecovery(ctx, &subsystem, device.devicePath, numActive) {
					continue
				}

				klog.Infof("Degraded subsystem: NQN=%s active=%d expected=%d device=%s",
					subsystem.NQN, numActive, expected, device.devicePath)

				if err := recoverPathsWithANA(clusterID, lvolID, device.devicePath, subsystem.Paths, hostNQN); err != nil {
					klog.Errorf("failed to recover paths for lvolID %s: %v", lvolID, err)
				}
			}
		}
	}

	var goneLvols []string

	mu.Lock()
	for devPath := range devicePresentMap {
		if !currentDevices[devPath] {
			lvolID := deviceToLvolIDMap[devPath]
			klog.Errorf(
				"Device %s is no longer present — all NVMe-oF connections were lost and the kernel removed the device (lvolID=%s)",
				devPath,
				lvolID,
			)
			delete(devicePresentMap, devPath)
			delete(deviceToLvolIDMap, devPath)
			if lvolID != "" {
				goneLvols = append(goneLvols, lvolID)
			}
		}
	}
	mu.Unlock()

	if markBroken != nil {
		for _, lvolID := range goneLvols {
			markBroken(lvolID)
		}
	}

	return nil
}

func fetchNodeInfo(ctx context.Context, client *ClusterClient, lvolID string) (*NodeInfo, error) {
	poolID, err := client.poolForVolume(ctx, lvolID)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve pool for volume %s: %w", lvolID, err)
	}
	raw, err := client.API.do(ctx, http.MethodGet, client.API.v2volume(poolID, lvolID), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch node info: %w", err)
	}
	var info NodeInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return nil, fmt.Errorf("failed to unmarshal node info: %w", err)
	}
	// v2 nodes field returns URL paths; extract UUIDs from last path segment
	for i, n := range info.Nodes {
		info.Nodes[i] = locationToUUID(n)
	}
	return &info, nil
}

func isAnyConnReachable(ctx context.Context, conns []*LvolConnectResp) bool {
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

func isNodeOnline(ctx context.Context, client *ClusterClient, nodeID, ip string, port int) bool {
	status, err := client.API.getStorageNodeStatus(ctx, nodeID)
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
	if hostID := hostIDFromHostNQN(hostNQN); hostID != "" {
		args = append(args, "--hostid="+hostID)
	}
	return args
}

// hostIDFromHostNQN derives a --hostid from an
// nqn.2014-08.io.simplyblock:uuid:<uuid>-style host NQN by taking its UUID
// suffix, or "" if hostNQN doesn't carry one (e.g. empty, or some other NQN
// format not built by this codebase).
func hostIDFromHostNQN(hostNQN string) string {
	i := strings.LastIndex(hostNQN, ":")
	if i < 0 || i == len(hostNQN)-1 {
		return ""
	}
	return hostNQN[i+1:]
}

func fetchLvolConnection(
	ctx context.Context,
	client *ClusterClient,
	lvolID string,
	hostNQN string,
) ([]*LvolConnectResp, error) {
	poolID, err := client.poolForVolume(ctx, lvolID)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve pool for volume %s: %w", lvolID, err)
	}
	connections, err := client.API.getLvolConnections(ctx, poolID, lvolID, hostNQN)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch connection: %w", err)
	}
	if len(connections) == 0 {
		return nil, fmt.Errorf("empty connection response for volume %s", lvolID)
	}
	return connections, nil
}

func connectViaNVMe(ctx context.Context, conn *LvolConnectResp, ctrlLossTmo int, retries int) error {
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

// confirmSubsystemNeedsRecovery re-checks the subsystem 5 times over 5 seconds
// and returns true only if the path count remained stable at initialPathCount for
// all 5 checks. This debounces spurious triggers during normal ANA switchovers.
func confirmSubsystemNeedsRecovery(
	ctx context.Context,
	subsystem *subsystem,
	devicePath string,
	initialPathCount int,
) bool {
	for i := 0; i < 5; i++ {
		recheck, err := getSubsystemsForDevice(ctx, devicePath)
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
func hasConnectingPath(paths []path) bool {
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
func resolveExpectedPathCount(nqn, clusterID, lvolID string, currentActive int, hostNQN string) int {
	maxSeenMu.Lock()
	cached, exists := maxSeenPathsMap[nqn]
	if currentActive > cached {
		cached = currentActive
		maxSeenPathsMap[nqn] = cached
	}
	maxSeenMu.Unlock()

	if exists {
		return cached
	}

	sbcClient, err := NewsimplyBlockClient(context.Background(), clusterID, "")
	if err != nil {
		klog.Warningf("resolveExpectedPathCount: client error for NQN %s: %v", nqn, err)
		return cached
	}
	conns, err := fetchLvolConnection(context.Background(), sbcClient, lvolID, hostNQN)
	if err != nil {
		klog.Warningf("resolveExpectedPathCount: fetch error for NQN %s: %v", nqn, err)
		return cached
	}

	maxSeenMu.Lock()
	if len(conns) > maxSeenPathsMap[nqn] {
		maxSeenPathsMap[nqn] = len(conns)
		cached = len(conns)
	}
	maxSeenMu.Unlock()

	return cached
}

func recoverPathsWithANA(clusterID, lvolID, devicePath string, activePaths []path, hostNQN string) error {
	sbcClient, err := NewsimplyBlockClient(context.Background(), clusterID, "")
	if err != nil {
		return fmt.Errorf("failed to create SimplyBlock client: %w", err)
	}

	nodeInfo, err := fetchNodeInfo(context.Background(), sbcClient, lvolID)
	if err != nil {
		return fmt.Errorf("failed to fetch node info for lvol %s: %w", lvolID, err)
	}

	expectedConns, err := fetchLvolConnection(context.Background(), sbcClient, lvolID, hostNQN)
	if err != nil {
		return fmt.Errorf("failed to fetch connections for lvol %s: %w", lvolID, err)
	}
	if len(expectedConns) == 0 {
		return fmt.Errorf("API returned no connections for lvol %s", lvolID)
	}

	nqn := expectedConns[0].Nqn
	maxSeenMu.Lock()
	if len(expectedConns) > maxSeenPathsMap[nqn] {
		maxSeenPathsMap[nqn] = len(expectedConns)
	}
	maxSeenMu.Unlock()

	ctrlLossTmo := DefaultCtrlLossTmo

	optConn := expectedConns[0]
	nonOptConns := expectedConns[1:]

	activeOpt := filterByANA(activePaths, anaStateOptimized)

	var activeNonOpt []path
	for _, p := range activePaths {
		if parseAddress(p.Address) != optConn.IP {
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
	healMonitoredVolume(context.Background(), nqn, lvolID, expectedConns)

	return nil
}

//nolint:unparam // devicePath kept for parity with reconcileNonOptimizedPaths
func reconcileOptimizedPath(
	sbcClient *ClusterClient,
	nodeInfo *NodeInfo,
	devicePath string,
	conn *LvolConnectResp,
	active []path,
	ctrlLossTmo int,
) {
	if len(active) == 0 {
		if !isNodeOnline(context.Background(), sbcClient, nodeInfo.NodeID, conn.IP, conn.Port) {
			klog.Infof("reconcileOptimizedPath: primary node %s not yet online, skipping", nodeInfo.NodeID)
			return
		}
		klog.Infof("reconcileOptimizedPath: connecting missing optimized path ip=%s", conn.IP)
		if err := connectViaNVMe(context.Background(), conn, ctrlLossTmo, 1); err != nil {
			klog.Errorf("reconcileOptimizedPath: connect to %s failed: %v", conn.IP, err)
		}
		return
	}

	activeIP := parseAddress(active[0].Address)
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
	if err := connectViaNVMe(context.Background(), conn, ctrlLossTmo, 1); err != nil {
		klog.Errorf("reconcileOptimizedPath: connect to new IP %s failed: %v", conn.IP, err)
	}
}

// reconcileNonOptimizedPaths handles connections[1..N] (secondary nodes).
// Works for both 2-path (1 secondary) and 3-path (2 secondaries).
//
//nolint:unparam // devicePath kept for parity with reconcileOptimizedPath
func reconcileNonOptimizedPaths(
	sbcClient *ClusterClient,
	nodeInfo *NodeInfo,
	devicePath string,
	conns []*LvolConnectResp,
	active []path,
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
		if err := connectViaNVMe(context.Background(), conn, ctrlLossTmo, 1); err != nil {
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
func missingEndpoints(conns []*LvolConnectResp, active []path) []*LvolConnectResp {
	attached := make(map[string]bool, len(active))
	for _, p := range active {
		if ip, port := parseEndpoint(p.Address); ip != "" && port != "" {
			attached[net.JoinHostPort(ip, port)] = true
		}
	}

	missing := make([]*LvolConnectResp, 0, len(conns))
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
func filterByANA(paths []path, anaState string) []path {
	var result []path
	for _, p := range paths {
		if p.ANAState == anaState {
			result = append(result, p)
		}
	}
	return result
}
