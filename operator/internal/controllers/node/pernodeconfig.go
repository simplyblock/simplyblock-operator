// The per-node configuration the storage-node DaemonSet's init container reads.
//
// One ConfigMap holds one entry per worker, keyed by hostname, each a
// shell-sourceable env file. One ConfigMap rather than one per node is what lets a
// single DaemonSet serve nodes that differ.
//
// It is derived rather than authoritative. Every value in it comes from a
// StorageNode's own spec.config or from its cluster, so it can be rebuilt from
// them at any time, and nothing reads it back.
//
// The merge that used to happen here is gone. The retired StorageNodeSet held
// fleet defaults that each node's overrides were layered onto, which made the set
// the source of truth and the node a cache of it. A node carries its whole
// configuration now (§3.1), so an entry is a rendering of one object rather than a
// resolution of two.
//
// design-storagenode.md §5.3 is the specification.

package node

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	atlaskube "github.com/simplyblock/atlas/kube"
	"github.com/simplyblock/atlas/pci"
	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// PerNodeConfigMapName is the ConfigMap of one cluster's storage nodes. It is
// named for the cluster now rather than for a node set, which is the retirement's
// one visible trace in an object name (§15.3).
func PerNodeConfigMapName(cluster string) string {
	return cluster + "-per-node-config"
}

// ReconcileConfig writes the ConfigMap from the node set it is given.
//
// It is written before the workers are enrolled on every pass, and a worker is
// only schedulable once it carries the label enrollment puts on it, so the
// ordering is what stops a pod starting against an entry that is not there. A
// pod that starts against a missing entry reaches the node configuration script
// with no sizing and fails there, which is a long way from the cause. For the
// same reason, a cluster missing its required sizing is refused with an error
// naming the fields rather than written out as blanks.
//
// The node set is a parameter rather than a list of its own, because the
// ordering only holds while both halves are looking at the same one: a node that
// appeared between two readings had its worker enrolled by a pass that never
// wrote its entry.
func (w *Workload) ReconcileConfig(
	ctx context.Context,
	cluster *simplyblockv1alpha2.StorageCluster,
	nodes []simplyblockv1alpha2.StorageNode,
) error {
	if cluster.Spec.MaxSubsystemCount == nil || cluster.Spec.VCPUCount == nil {
		return fmt.Errorf(
			"cluster %s is missing the node sizing its workers boot from: "+
				"set spec.maxSubsystemCount and spec.vcpuCount", cluster.Name)
	}

	data := map[string]string{}
	for i := range nodes {
		data[nodes[i].Spec.WorkerNode] = renderNodeConfig(cluster, &nodes[i])
	}

	return w.applyConfigMap(ctx, cluster, data)
}

// CloneWorkerConfig copies one worker's entry onto another, merging any drives a
// migration is binding on the target into the cloned allow list.
//
// It runs before the target is labeled, so the entry exists by the time the pod's
// init container sources it. Persisting it here rather than letting the next
// ordinary pass rebuild it is what stops that pass writing the target's entry back
// from a node whose spec.workerNode still names the source (§9).
func (w *Workload) CloneWorkerConfig(
	ctx context.Context, namespace, cluster, source, target string, newSsdPcie []string,
) error {
	var clusterObject simplyblockv1alpha2.StorageCluster
	key := client.ObjectKey{Namespace: namespace, Name: cluster}
	if err := w.Get(ctx, key, &clusterObject); err != nil {
		return fmt.Errorf("read cluster %s: %w", cluster, err)
	}

	var configMap corev1.ConfigMap
	configKey := client.ObjectKey{Namespace: namespace, Name: PerNodeConfigMapName(cluster)}
	if err := w.Get(ctx, configKey, &configMap); err != nil {
		if apierrors.IsNotFound(err) {
			// Nothing to clone from. The ordinary pass writes the map, and the
			// migration's Preparing step holds until the target's pod is ready,
			// which cannot happen before the entry exists.
			return nil
		}
		return fmt.Errorf("read cluster %s's per-node configuration: %w", cluster, err)
	}

	entry, ok := configMap.Data[source]
	if !ok {
		return fmt.Errorf("worker %s has no per-node configuration to clone", source)
	}
	cloned := mergeAllowedIntoEntry(entry, newSsdPcie)
	if configMap.Data[target] == cloned {
		return nil
	}

	patch := client.MergeFrom(configMap.DeepCopy())
	if configMap.Data == nil {
		configMap.Data = map[string]string{}
	}
	configMap.Data[target] = cloned
	if err := w.Patch(ctx, &configMap, patch); err != nil {
		return fmt.Errorf("write worker %s's cloned configuration: %w", target, err)
	}
	return nil
}

// applyConfigMap creates or updates the ConfigMap, owned by the cluster.
func (w *Workload) applyConfigMap(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster, data map[string]string,
) error {
	name := PerNodeConfigMapName(cluster.Name)

	var existing corev1.ConfigMap
	key := client.ObjectKey{Namespace: cluster.Namespace, Name: name}
	err := w.Get(ctx, key, &existing)
	if apierrors.IsNotFound(err) {
		created := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: cluster.Namespace,
				Labels: map[string]string{
					atlaskube.LabelApp:            atlaskube.AppStorageNode,
					atlaskube.LabelStorageNodeSet: cluster.Name,
				},
			},
			Data: data,
		}
		if err := controllerutil.SetControllerReference(cluster, created, w.Scheme()); err != nil {
			return fmt.Errorf("own the per-node configuration: %w", err)
		}
		if err := w.Create(ctx, created); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create the per-node configuration: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the per-node configuration: %w", err)
	}

	// A worker a migration cloned an entry onto is not in the list until the
	// node's spec.workerNode has moved, and the re-point happens after the
	// promote. Dropping the entry in between would rebuild the target's
	// configuration from nothing while its pod is running against it, so entries
	// this pass did not produce are carried forward rather than removed.
	merged := make(map[string]string, len(existing.Data)+len(data))
	for worker, entry := range existing.Data {
		merged[worker] = entry
	}
	for worker, entry := range data {
		merged[worker] = entry
	}
	if equalConfigData(existing.Data, merged) {
		return nil
	}

	patch := client.MergeFrom(existing.DeepCopy())
	existing.Data = merged
	if err := w.Patch(ctx, &existing, patch); err != nil {
		return fmt.Errorf("write the per-node configuration: %w", err)
	}
	return nil
}

// renderNodeConfig is one worker's entry: a shell-sourceable env file.
//
// MAX_SUBSYS_COUNT comes from the cluster and is identical in every entry, because
// the cap bounds how many volumes any node can serve and is therefore the
// cluster's rather than a stamp on the node (§3.1). That is also what makes a
// change to it reach the nodes that already exist rather than only the next one
// created. MAX_HUGE_PAGES_SIZE and VCPU_COUNT come from the node's own sizing,
// which is equal across the fleet in steady state and deliberately unequal for the
// duration of a rolling hardware upgrade.
func renderNodeConfig(
	cluster *simplyblockv1alpha2.StorageCluster, node *simplyblockv1alpha2.StorageNode,
) string {
	config := node.Spec.Config

	var entry strings.Builder
	fmt.Fprintf(&entry, "MAX_SUBSYS_COUNT=%s\n", ptr.StringOrDefault(cluster.Spec.MaxSubsystemCount, ""))
	fmt.Fprintf(&entry, "MAX_HUGE_PAGES_SIZE=%s\n", utils.ShellQuote(config.Sizing.MinHugePagesSize))
	fmt.Fprintf(&entry, "VCPU_COUNT=%s\n", ptr.StringOrDefault(config.Sizing.VCPUCount, ""))
	addresses, names := splitDeviceNames(config.DeviceNames)
	fmt.Fprintf(&entry, "PCI_ALLOWED=%s\n",
		utils.ShellQuote(strings.Join(mergePCIAddresses(config.PcieAllowList, addresses), ",")))
	fmt.Fprintf(&entry, "PCI_BLOCKED=%s\n", utils.ShellQuote(strings.Join(config.PcieDenyList, ",")))
	fmt.Fprintf(&entry, "NVME_DEVICES=%s\n", utils.ShellQuote(strings.Join(names, ",")))
	fmt.Fprintf(&entry, "DEVICE_MODEL=%s\n", utils.ShellQuote(config.PcieModel))
	fmt.Fprintf(&entry, "SIZE_RANGE=%s\n", utils.ShellQuote(config.DriveSizeRange))
	if jm := config.JournalManager; jm != nil {
		fmt.Fprintf(&entry, "JM_PERCENT=%s\n", ptr.StringOrDefault(jm.PercentPerDevice, ""))
		fmt.Fprintf(&entry, "HA_JM_COUNT=%s\n", ptr.StringOrDefault(jm.Count, ""))
	} else {
		entry.WriteString("JM_PERCENT=\n")
		entry.WriteString("HA_JM_COUNT=\n")
	}
	return entry.String()
}

// splitDeviceNames sorts one device list into the two variables the storage
// node's configuration script reads it through.
//
// spec.config.deviceNames carries both spellings a device is named by, and the
// backend resolves them by different means: --pci-allowed takes PCI addresses
// and matches them against the controllers sysfs enumerates, while
// --nvme-devices is the namespace-name channel, whose argparse destination is
// nvme_names and whose lookup compares each entry against the NameSpace values
// of `nvme list` — nvme0n1 and its siblings. A PCI address sent through the
// second matches nothing, and matching nothing is not an error there: the
// configure selects no device, fails with `There are no enough SSD devices on
// system`, and its return value is discarded, so the pod starts on whatever
// configuration the host already had.
//
// So the spelling decides the variable, which is what the API says it does:
// design-storagenode.md's deviceNames admits an address, a path, or a bare
// name, and the class it belongs to is the cluster's to declare.
func splitDeviceNames(devices []string) (addresses, names []string) {
	for _, device := range devices {
		if address, err := pci.ParseAddress(device); err == nil {
			addresses = append(addresses, address)
			continue
		}
		names = append(names, device)
	}
	return addresses, names
}

// mergeAllowedIntoEntry rewrites one entry's PCI_ALLOWED to include the addresses
// a migration is binding on the target host.
//
// It edits the rendered text rather than re-rendering from the node, because the
// node's spec.config.pcieAllowList has not been rewritten yet: that happens after
// the promote, and this runs before the relocation.
func mergeAllowedIntoEntry(entry string, added []string) string {
	if len(added) == 0 {
		return entry
	}
	lines := strings.Split(entry, "\n")
	for i, line := range lines {
		if !strings.HasPrefix(line, "PCI_ALLOWED=") {
			continue
		}
		existing := parseShellList(strings.TrimPrefix(line, "PCI_ALLOWED="))
		merged := mergePCIAddresses(existing, added)
		lines[i] = "PCI_ALLOWED=" + utils.ShellQuote(strings.Join(merged, ","))
		return strings.Join(lines, "\n")
	}
	// The entry predates the field. Appending it is what a node whose allow list
	// was empty needs, and the init container reads the last assignment.
	return entry + "PCI_ALLOWED=" + utils.ShellQuote(strings.Join(added, ",")) + "\n"
}

// parseShellList reads back a comma-separated value this file wrote, stripping the
// quoting ShellQuote applied.
func parseShellList(value string) []string {
	value = strings.TrimSpace(value)
	value = strings.Trim(value, `'"`)
	if value == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// equalConfigData compares two entry sets, so a pass that changed nothing writes
// nothing. A map comparison is enough because both halves are rendered by the same
// function from the same objects.
func equalConfigData(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	keys := make([]string, 0, len(a))
	for key := range a {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if a[key] != b[key] {
			return false
		}
	}
	return true
}
