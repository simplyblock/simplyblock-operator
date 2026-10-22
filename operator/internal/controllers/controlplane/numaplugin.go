// The NUMA device plugin the ControlPlane owns, from the embedded manifest.
//
// It is one DaemonSet for the Kubernetes cluster, in the operator's namespace,
// which is where the singleton ControlPlane lives, so the ControlPlane can own it
// and garbage collection removes it with the ControlPlane. It selects every worker
// a StorageCluster enrolled, so it does not depend on which cluster that is. The
// OpenShift node bootstrap finds its pods by label across namespaces.

package controlplane

import (
	"bytes"
	"context"
	_ "embed"
	"errors"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// NUMAPluginImageEnv names the image the plugin runs, so a disconnected
// installation points it at a mirror.
const NUMAPluginImageEnv = "SB_NUMA_PLUGIN_IMAGE"

// numaPluginManifest is the plugin's ServiceAccount and DaemonSet.
//
//go:embed manifests/numa-resource-plugin.yaml
var numaPluginManifest []byte

// numaPluginObjects decodes the manifest, the account first and the DaemonSet
// second, into the ControlPlane's namespace. Decoding is strict, so a misspelled
// field in the file fails here rather than being dropped.
func (r *ControlPlaneReconciler) numaPluginObjects(
	cp *simplyblockv1alpha2.ControlPlane,
) ([]client.Object, error) {
	docs := bytes.Split(numaPluginManifest, []byte("\n---\n"))
	account, daemonSet := &corev1.ServiceAccount{}, &appsv1.DaemonSet{}
	if len(docs) != 2 ||
		yaml.UnmarshalStrict(docs[0], account) != nil || yaml.UnmarshalStrict(docs[1], daemonSet) != nil ||
		len(daemonSet.Spec.Template.Spec.Containers) == 0 {
		return nil, errors.New("the NUMA plugin manifest is not an account followed by a daemon set")
	}
	account.Namespace, daemonSet.Namespace = cp.Namespace, cp.Namespace
	if r.NUMAPluginImage != "" {
		daemonSet.Spec.Template.Spec.Containers[0].Image = r.NUMAPluginImage
	}
	return []client.Object{account, daemonSet}, nil
}

// applyNUMAPlugin applies the plugin owned by the ControlPlane. It is applied
// whichever source the ControlPlane names: a remote control plane still has
// storage workers beside it.
func (r *ControlPlaneReconciler) applyNUMAPlugin(
	ctx context.Context, cp *simplyblockv1alpha2.ControlPlane,
) error {
	objects, err := r.numaPluginObjects(cp)
	if err != nil {
		return err
	}
	return applyAll(ctx, r.Client, cp, r.Scheme, objects)
}
