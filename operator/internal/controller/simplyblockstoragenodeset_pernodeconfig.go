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
// fields like maxLogicalVolumeCount, corePercentage, deviceNames, etc. differ
// per node without requiring a separate DaemonSet per node.
//
// ConfigMap structure:
//
//	data:
//	  vm02.example.com: |
//	    MAX_LVOL=20
//	    MAX_SIZE=
//	    CORES_PERCENTAGE=50
//	    RESERVED_SYSTEM_CPUS=0,1
//	    CPU_TOPOLOGY_ENABLED=true
//	    ...
//	  vm03.example.com: |
//	    MAX_LVOL=25
//	    ...

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
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

	var existing corev1.ConfigMap
	err := r.Get(ctx, client.ObjectKey{Name: name, Namespace: sns.Namespace}, &existing)

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
	if err != nil {
		return fmt.Errorf("getting per-node ConfigMap: %w", err)
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
		MaxLogicalVolumeCount: sns.Spec.MaxLogicalVolumeCount,
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
		if o.MaxLogicalVolumeCount != nil {
			eff.MaxLogicalVolumeCount = o.MaxLogicalVolumeCount
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
	b.WriteString(fmt.Sprintf("MAX_LVOL=%s\n", utils.Int32PtrToString(eff.MaxLogicalVolumeCount)))
	b.WriteString(fmt.Sprintf("MAX_SIZE=%s\n", eff.MaxSize))
	b.WriteString(fmt.Sprintf("CORES_PERCENTAGE=%s\n", utils.Int32PtrToString(eff.CorePercentage)))
	b.WriteString(fmt.Sprintf("RESERVED_SYSTEM_CPUS=%s\n", eff.ReservedSystemCPU))
	b.WriteString(fmt.Sprintf("CPU_TOPOLOGY_ENABLED=%s\n", utils.BoolPtrToString(eff.EnableCpuTopology)))
	b.WriteString(fmt.Sprintf("PCI_ALLOWED=%s\n", strings.Join(eff.PcieAllowList, ",")))
	b.WriteString(fmt.Sprintf("PCI_BLOCKED=%s\n", strings.Join(eff.PcieDenyList, ",")))
	b.WriteString(fmt.Sprintf("NVME_DEVICES=%s\n", strings.Join(eff.DeviceNames, ",")))
	b.WriteString(fmt.Sprintf("DEVICE_MODEL=%s\n", eff.PcieModel))
	b.WriteString(fmt.Sprintf("SIZE_RANGE=%s\n", eff.DriveSizeRange))
	if eff.JournalManagerSpec != nil {
		b.WriteString(fmt.Sprintf("JM_PERCENT=%s\n", utils.Int32PtrToString(eff.JournalManagerSpec.PercentPerDevice)))
		b.WriteString(fmt.Sprintf("HA_JM_COUNT=%s\n", utils.Int32PtrToString(eff.JournalManagerSpec.Count)))
	} else {
		b.WriteString("JM_PERCENT=\n")
		b.WriteString("HA_JM_COUNT=\n")
	}
	return b.String()
}
