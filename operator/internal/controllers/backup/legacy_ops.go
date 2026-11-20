// Detection of StorageBackupOps stored before spec.backupRef and spec.clusterRef
// became objects, so the operator can start without them. The ops controller
// lives beside it in this package.

package backup

import (
	"context"
	"sort"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// LegacyBackupOps names the StorageBackupOps whose spec still holds a reference
// as a string. A typed list fails on the first of them, so the ops controller's
// cache never syncs and the manager exits. It reads unstructured for that reason.
func LegacyBackupOps(ctx context.Context, reader client.Reader) ([]types.NamespacedName, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(simplyblockv1alpha2.GroupVersion.WithKind("StorageBackupOpsList"))
	if err := reader.List(ctx, list); err != nil {
		return nil, err
	}

	var legacy []types.NamespacedName
	for i := range list.Items {
		spec, _, _ := unstructured.NestedMap(list.Items[i].Object, "spec")
		_, backupIsString := spec["backupRef"].(string)
		_, clusterIsString := spec["clusterRef"].(string)
		if backupIsString || clusterIsString {
			legacy = append(legacy, client.ObjectKeyFromObject(&list.Items[i]))
		}
	}
	sort.Slice(legacy, func(a, b int) bool {
		if legacy[a].Namespace != legacy[b].Namespace {
			return legacy[a].Namespace < legacy[b].Namespace
		}
		return legacy[a].Name < legacy[b].Name
	})
	return legacy, nil
}
