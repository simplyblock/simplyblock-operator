/*
Copyright 2025.

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

package controller

// reconcilePerNodeConfigMap creates or updates a single ConfigMap that holds
// per-worker-node effective configuration values. The DaemonSet init container
// mounts this ConfigMap and sources the file matching its hostname so that
// fields like maxSubsystemCount, corePercentage, deviceNames, etc. differ
// per node without requiring a separate DaemonSet per node.
//
// ConfigMap structure:
//
//	data:
//	  vm02.example.com: |
//	    MAX_LVOL=20
//	    MAX_SIZE=
//	    CORES_PERCENTAGE=50
//	    ...
//	  vm03.example.com: |
//	    MAX_LVOL=25
//	    ...

import (
	"context"
	"fmt"
	"strings"

	"github.com/simplyblock/simplyblock-operator/internal/utils"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/simplyblock/atlas/kube"
	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
)

// PerNodeConfigMapName returns the name of the per-node ConfigMap for a StorageNodeSet.
func PerNodeConfigMapName(snsName string) string {
	return snsName + "-per-node-config"
}

// reconcilePerNodeConfigMap creates or updates the per-node ConfigMap with the
// effective (fleet defaults merged with nodeConfigs overrides) values for every
// worker in the StorageNodeSet.
func (r *StorageNodeSetReconciler) reconcilePerNodeConfigMap(
	ctx context.Context,
	sns *simplyblockv1alpha1.StorageNodeSet,
) error {
	log := logf.FromContext(ctx)
	name := PerNodeConfigMapName(sns.Name)

	data := make(map[string]string, len(sns.Spec.WorkerNodes))
	for _, worker := range sns.Spec.WorkerNodes {
		data[worker] = buildPerNodeEnvFile(sns, worker)
	}

	// Also include manually created StorageNode CRs that reference this StorageNodeSet
	// but whose worker is not in spec.workerNodes. Their overrides are merged with
	// fleet defaults so the DaemonSet init container gets the right per-node config.
	var snList simplyblockv1alpha1.StorageNodeList
	if err := r.List(ctx, &snList,
		client.InNamespace(sns.Namespace),
		client.MatchingFields{"spec.storageNodeSetRef": sns.Name},
	); err == nil {
		for _, sn := range snList.Items {
			if _, ok := data[sn.Spec.WorkerNode]; ok {
				continue // already covered by spec.workerNodes
			}
			// Manually created: use its overrides on top of fleet defaults.
			snsCopy := sns.DeepCopy()
			if sn.Spec.Overrides != nil {
				if snsCopy.Spec.NodeConfigs == nil {
					snsCopy.Spec.NodeConfigs = make(map[string]simplyblockv1alpha1.StorageNodeOverrides)
				}
				snsCopy.Spec.NodeConfigs[sn.Spec.WorkerNode] = *sn.Spec.Overrides
			}
			data[sn.Spec.WorkerNode] = buildPerNodeEnvFile(snsCopy, sn.Spec.WorkerNode)
		}
	}

	var existing corev1.ConfigMap
	err := r.Get(ctx, client.ObjectKey{Name: name, Namespace: sns.Namespace}, &existing)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("getting per-node ConfigMap: %w", err)
	}

	// Include workers labeled into this StorageNodeSet but not yet in
	// spec.workerNodes or backed by a StorageNode CR — an in-flight StorageNodeOps
	// migration labels its target and ensureMigratedWorkerConfig authors the
	// target's per-node entry (source clone + newSsdPcie merged into PCI_ALLOWED)
	// during Preparing, before reconcileMigratedTopology folds the target into
	// spec.workerNodes once it is online. Without this, the full rebuild above
	// drops that entry, and when the target's DaemonSet pod (re)starts its init
	// container sources an empty env and config generation fails on max-lvol=0.
	// Prefer the existing entry so the clone/newSsdPcie survive the rebuild; fall
	// back to fleet defaults so a target whose entry was already lost still boots
	// with a valid config. Mirrors reconcileEndpointSlice's label-based inclusion.
	var labeledNodes corev1.NodeList
	if listErr := r.List(ctx, &labeledNodes, client.MatchingLabels{kube.LabelStorageNodeSet: sns.Name}); listErr != nil {
		return fmt.Errorf("listing storage-plane nodes for per-node ConfigMap: %w", listErr)
	}
	for i := range labeledNodes.Items {
		worker := labeledNodes.Items[i].Name
		if _, ok := data[worker]; ok {
			continue // already covered by spec.workerNodes or a StorageNode CR
		}
		if entry, ok := existing.Data[worker]; ok {
			data[worker] = entry
		} else {
			data[worker] = buildPerNodeEnvFile(sns, worker)
		}
	}

	if apierrors.IsNotFound(err) {
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: sns.Namespace,
			},
			Data: data,
		}
		if setErr := controllerutil.SetControllerReference(sns, cm, r.Scheme); setErr != nil {
			return fmt.Errorf("setting owner reference on per-node ConfigMap: %w", setErr)
		}
		if createErr := r.Create(ctx, cm); createErr != nil {
			return fmt.Errorf("creating per-node ConfigMap: %w", createErr)
		}
		log.Info("created per-node ConfigMap", "name", name)
		return nil
	}

	// Update if data changed.
	patch := client.MergeFrom(existing.DeepCopy())
	existing.Data = data
	if patchErr := r.Patch(ctx, &existing, patch); patchErr != nil {
		return fmt.Errorf("patching per-node ConfigMap: %w", patchErr)
	}
	return nil
}

// buildPerNodeEnvFile returns a shell-sourceable env file string with the
// effective per-node values for the given worker, merging fleet defaults from
// the StorageNodeSet spec with any nodeConfigs overrides.
func buildPerNodeEnvFile(sns *simplyblockv1alpha1.StorageNodeSet, worker string) string {
	// Start with fleet defaults.
	eff := simplyblockv1alpha1.StorageNodeOverrides{
		MaxSubsystemCount: sns.Spec.MaxSubsystemCount,
		MaxSize:               sns.Spec.MaxSize,
		CorePercentage:        sns.Spec.CorePercentage,
		SpdkSystemMemory:      sns.Spec.SpdkSystemMemory,
		JournalManagerSpec:    sns.Spec.JournalManagerSpec,
		PcieAllowList:         sns.Spec.PcieAllowList,
		PcieDenyList:          sns.Spec.PcieDenyList,
		PcieModel:             sns.Spec.PcieModel,
		DriveSizeRange:        sns.Spec.DriveSizeRange,
		DeviceNames:           sns.Spec.DeviceNames,
		EnableCpuTopology:     sns.Spec.EnableCpuTopology,
		ReservedSystemCPU:     sns.Spec.ReservedSystemCPU,
	}

	// Apply per-node overrides if present.
	if o, ok := sns.Spec.NodeConfigs[worker]; ok {
		if o.MaxSubsystemCount != nil {
			eff.MaxSubsystemCount = o.MaxSubsystemCount
		}
		if o.MaxSize != "" {
			eff.MaxSize = o.MaxSize
		}
		if o.CorePercentage != nil {
			eff.CorePercentage = o.CorePercentage
		}
		if o.SpdkSystemMemory != "" {
			eff.SpdkSystemMemory = o.SpdkSystemMemory
		}
		if o.JournalManagerSpec != nil {
			eff.JournalManagerSpec = o.JournalManagerSpec
		}
		if len(o.PcieAllowList) > 0 {
			eff.PcieAllowList = o.PcieAllowList
		}
		if len(o.PcieDenyList) > 0 {
			eff.PcieDenyList = o.PcieDenyList
		}
		if o.PcieModel != "" {
			eff.PcieModel = o.PcieModel
		}
		if o.DriveSizeRange != "" {
			eff.DriveSizeRange = o.DriveSizeRange
		}
		if len(o.DeviceNames) > 0 {
			eff.DeviceNames = o.DeviceNames
		}
		if o.EnableCpuTopology != nil {
			eff.EnableCpuTopology = o.EnableCpuTopology
		}
		if o.ReservedSystemCPU != "" {
			eff.ReservedSystemCPU = o.ReservedSystemCPU
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "MAX_LVOL=%s\n", ptr.StringOrDefault(eff.MaxSubsystemCount, ""))
	fmt.Fprintf(&b, "MAX_SIZE=%s\n", utils.ShellQuote(eff.MaxSize))
	fmt.Fprintf(&b, "CORES_PERCENTAGE=%s\n", ptr.StringOrDefault(eff.CorePercentage, ""))
	fmt.Fprintf(&b, "PCI_ALLOWED=%s\n", utils.ShellQuote(strings.Join(eff.PcieAllowList, ",")))
	fmt.Fprintf(&b, "PCI_BLOCKED=%s\n", utils.ShellQuote(strings.Join(eff.PcieDenyList, ",")))
	fmt.Fprintf(&b, "NVME_DEVICES=%s\n", utils.ShellQuote(strings.Join(eff.DeviceNames, ",")))
	fmt.Fprintf(&b, "DEVICE_MODEL=%s\n", utils.ShellQuote(eff.PcieModel))
	fmt.Fprintf(&b, "SIZE_RANGE=%s\n", utils.ShellQuote(eff.DriveSizeRange))
	if eff.JournalManagerSpec != nil {
		fmt.Fprintf(&b, "JM_PERCENT=%s\n", ptr.StringOrDefault(eff.JournalManagerSpec.PercentPerDevice, ""))
		fmt.Fprintf(&b, "HA_JM_COUNT=%s\n", ptr.StringOrDefault(eff.JournalManagerSpec.Count, ""))
	} else {
		b.WriteString("JM_PERCENT=\n")
		b.WriteString("HA_JM_COUNT=\n")
	}
	return b.String()
}
