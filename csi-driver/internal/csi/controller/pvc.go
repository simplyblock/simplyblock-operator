// Reading and clearing the annotations a PersistentVolumeClaim carries into
// provisioning. They are how a workload asks for something its StorageClass
// does not say, so they are read at create time and cleared afterwards.
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

package controller

import (
	"context"
	"encoding/json"
	"fmt"

	"k8s.io/klog"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func (cs *Server) fetchPVCAnnotations(
	ctx context.Context,
	pvcName, pvcNamespace string,
) (map[string]string, error) {
	if cs.kubeClient == nil {
		return nil, fmt.Errorf("kubernetes client not configured (no in-cluster config)")
	}
	pvc, err := cs.kubeClient.CoreV1().PersistentVolumeClaims(pvcNamespace).Get(ctx, pvcName, metav1.GetOptions{})
	if err != nil {
		klog.Errorf("failed to get PVC %s in namespace %s: %v", pvcName, pvcNamespace, err)
		return nil, fmt.Errorf("could not get PVC %s in namespace %s: %w", pvcName, pvcNamespace, err)
	}

	return pvc.Annotations, nil
}

// removePVCAnnotations deletes the given annotation keys from a PVC via a JSON
// merge patch: a null value removes the key, and is a no-op when the key is
// already absent, so this is safe to call on CreateVolume retries.
func (cs *Server) removePVCAnnotations(
	ctx context.Context,
	pvcName, pvcNamespace string,
	keys ...string,
) error {
	if cs.kubeClient == nil {
		return fmt.Errorf("kubernetes client not configured (no in-cluster config)")
	}

	annotations := make(map[string]interface{}, len(keys))
	for _, k := range keys {
		annotations[k] = nil
	}
	patch, err := json.Marshal(map[string]interface{}{
		"metadata": map[string]interface{}{"annotations": annotations},
	})
	if err != nil {
		return fmt.Errorf("marshal annotation patch: %w", err)
	}

	if _, err := cs.kubeClient.CoreV1().PersistentVolumeClaims(pvcNamespace).Patch(
		ctx, pvcName, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("patch PVC %s/%s: %w", pvcNamespace, pvcName, err)
	}
	return nil
}

// pvcAnnotation returns the first non-empty value among keys, in priority order.
// It lets a value be sourced from a primary annotation with one or more
// fallbacks (e.g. selected-storage-node, then the legacy host-id forms).
func pvcAnnotation(annotations map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := annotations[k]; v != "" {
			return v
		}
	}
	return ""
}
